package dashboard

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/paylinks"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector/money"
)

const maxLinkRows = 200

type linkRow struct {
	ID, App, URL, Description, Amount, Kind, Status, Expires, Created string
	CanDeactivate                                                     bool
}

// linkFormValues is the create form as typed, shown again after a mistake.
type linkFormValues struct {
	App, Currency, Amount, MinAmount, MaxAmount, Description, ReturnURL, ExpiresAt, Metadata string
	SingleUse                                                                                bool
}

type linkFilter struct{ App, State string }

func (d *Dashboard) routesPaymentLinks() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/payment-links", d.authed(authz.PaymentRead, d.linksPage))
	// Creating and switching off links are operator actions, like creating payments.
	m.HandleFunc("POST "+prefix+"/payment-links", d.authed(authz.PaymentCreate, d.linkCreate))
	m.HandleFunc("POST "+prefix+"/payment-links/{id}/deactivate", d.authed(authz.PaymentCreate, d.linkDeactivate))
}

func (d *Dashboard) linkStatus(l store.PaymentLink) string {
	switch {
	case !l.Active && l.DeactivatedReason == paylinks.ReasonUsed:
		return "used"
	case !l.Active:
		return "inactive"
	case !l.ExpiresAt.IsZero() && !d.o.Now().Before(l.ExpiresAt):
		return "expired"
	}
	return "active"
}

func (d *Dashboard) linksPage(w http.ResponseWriter, r *http.Request, s *session) {
	var f linkFilter
	qv := r.URL.Query()
	f.App, f.State = qv.Get("app"), qv.Get("state")
	if f.State != "" && f.State != "active" && f.State != "inactive" {
		badRequest(w, "state must be active or inactive")
		return
	}
	d.renderLinks(w, r, s, http.StatusOK, f, linkFormValues{Currency: "INR"}, "")
}

func (d *Dashboard) renderLinks(w http.ResponseWriter, r *http.Request, s *session, status int, f linkFilter, form linkFormValues, errMsg string) {
	ctx := r.Context()
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	var apps []store.App
	if err := d.o.DB.Read(ctx, func(q store.Querier) (err error) { apps, err = store.ListApps(ctx, q, org); return }); err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "payment-links", "Payment links")
	names := map[string]string{}
	for _, a := range apps {
		names[a.ID] = a.Name
		v.AppOpts = append(v.AppOpts, appOpt{ID: a.ID, Name: a.Name})
	}
	if f.App != "" {
		if _, ok := names[f.App]; !ok {
			badRequest(w, "unknown app")
			return
		}
	}
	mode := d.mode(r, s)
	links, err := d.o.PaymentLinks.ListOrg(ctx, org, paylinks.OrgListParams{AppID: f.App, Mode: mode, Limit: maxLinkRows})
	if err != nil {
		d.fail(w, err)
		return
	}
	canAct := s.can(authz.PaymentCreate)
	for _, l := range links {
		st := d.linkStatus(l)
		if f.State == "active" && st != "active" || f.State == "inactive" && st == "active" {
			continue
		}
		row := linkRow{ID: l.ID, App: names[l.AppID], URL: d.o.PaymentLinks.URL(l), Description: l.Description, Kind: "reusable", Status: st,
			Created: d.fmtTime(l.CreatedAt), CanDeactivate: canAct && l.Active}
		if l.SingleUse {
			row.Kind = "single-use"
		}
		if l.Amount > 0 {
			row.Amount = fmtMoney(l.Amount, l.Currency)
		} else if l.MaxAmount > 0 {
			row.Amount = "customer chooses: " + fmtMoney(l.MinAmount, l.Currency) + " to " + fmtMoney(l.MaxAmount, l.Currency)
		} else {
			row.Amount = "customer chooses: from " + fmtMoney(l.MinAmount, l.Currency)
		}
		if !l.ExpiresAt.IsZero() {
			row.Expires = d.fmtTime(l.ExpiresAt)
		}
		v.LinkRows = append(v.LinkRows, row)
	}
	v.LinkFilter, v.LinkForm, v.Error, v.CanManage = f, form, errMsg, canAct
	d.render(w, status, "dash_links", v)
}

// majorToMinor reads an amount typed in major units ("1,499.50").
func majorToMinor(s, currency string) (int64, error) {
	return money.ParseDecimal(strings.NewReplacer(",", "", " ", "").Replace(strings.TrimSpace(s)), currency)
}

func (d *Dashboard) linkCreate(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	f := r.PostForm
	form := linkFormValues{App: f.Get("app"), Currency: strings.ToUpper(strings.TrimSpace(f.Get("currency"))), Amount: f.Get("amount"),
		MinAmount: f.Get("min_amount"), MaxAmount: f.Get("max_amount"), Description: strings.TrimSpace(f.Get("description")),
		ReturnURL: strings.TrimSpace(f.Get("return_url")), ExpiresAt: strings.TrimSpace(f.Get("expires_at")), Metadata: f.Get("metadata"),
		SingleUse: f.Get("single_use") != ""}
	again := func(msg string) { d.renderLinks(w, r, s, http.StatusBadRequest, linkFilter{}, form, msg) }

	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	var app store.App
	err = d.o.DB.Read(ctx, func(q store.Querier) (err error) { app, err = store.GetApp(ctx, q, form.App); return })
	if errors.Is(err, store.ErrNotFound) || err == nil && !d.inOrg(ctx, app.OrgID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}

	in := paylinks.CreateInput{Scope: core.Scope{AppID: app.ID, Mode: d.mode(r, s)}, OrgID: org, Currency: form.Currency, Description: form.Description,
		ReturnURL: form.ReturnURL, SingleUse: form.SingleUse}
	for _, a := range []struct {
		name, raw string
		dst       *int64
	}{{"Amount", form.Amount, &in.Amount}, {"Minimum amount", form.MinAmount, &in.MinAmount}, {"Maximum amount", form.MaxAmount, &in.MaxAmount}} {
		if strings.TrimSpace(a.raw) == "" {
			continue
		}
		n, err := majorToMinor(a.raw, form.Currency)
		if err != nil || n <= 0 {
			again(a.name + " must be a positive number such as 499 or 1,499.50 (no more decimals than the currency has).")
			return
		}
		*a.dst = n
	}
	if form.ExpiresAt != "" {
		t, err := time.ParseInLocation("2006-01-02T15:04", form.ExpiresAt, d.o.Location)
		if err != nil {
			again("The expiry must be a date and time such as 2026-12-31T18:00.")
			return
		}
		in.ExpiresAt = t.UTC()
	}
	if in.Metadata, err = parseLinkMetadata(form.Metadata); err != nil {
		again(err.Error())
		return
	}
	if _, err := d.o.PaymentLinks.Create(ctx, s.auditActor(), in); err != nil {
		var le *paylinks.Error
		if errors.As(err, &le) && errors.Is(err, paylinks.ErrInvalid) {
			again(strings.ToUpper(le.Message[:1]) + le.Message[1:] + ".")
			return
		}
		if errors.Is(err, paylinks.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/payment-links", http.StatusSeeOther)
}

// parseLinkMetadata reads "key=value" lines.
func parseLinkMetadata(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			return nil, errors.New("Metadata lines look like key=value.")
		}
		out[k] = v
	}
	return out, nil
}

func (d *Dashboard) linkDeactivate(w http.ResponseWriter, r *http.Request, s *session) {
	org, err := d.orgID(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	if _, err := d.o.PaymentLinks.DeactivateInOrg(r.Context(), s.auditActor(), org, r.PathValue("id")); err != nil {
		if errors.Is(err, paylinks.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/payment-links", http.StatusSeeOther)
}
