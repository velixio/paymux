package dashboard

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/subscriptions"
)

// Subscription pages: list, detail (periods, renewals with links to their
// payments, mandate, dunning) and the operator actions cancel and "create a
// checkout link" (the link of a pending (re-)authorization, shown once).

type subRow struct {
	ID, App, Customer, Status, Amount, Billing, PeriodEnd, Created string
	Notes                                                          []string
}

type subFilter struct{ App, Status, Q string }

type cycleRow struct {
	Period, State, Amount, Retries, Notified, NextRun, PaymentID, PaymentStatus, Provider string
}

type mandateRow struct {
	ID, Method, Status, MaxAmount, ValidUntil, ProviderRef, ProviderID, Role string
}

type subDetail struct {
	ID, App, AppID, Mode, Status, Billing, Amount, Interval, Customer, Description, Created string
	PeriodStart, PeriodEnd, TrialEnd, Anchor, CanceledAt, EndedAt                           string
	Renewals, ProviderSubscription                                                          string
	PendingAmount, NextRetry                                                                string
	Dunning                                                                                 int
	CancelAtPeriodEnd, RequiresReauth                                                       bool
	Open, CanCancel, CanLink, WaitingForCustomer                                            bool
	Mandates                                                                                []mandateRow
	Cycles                                                                                  []cycleRow
}

// subsView is the data of the dash_subscription* templates: the shared view
// plus the page's own fields.
type subsView struct {
	view
	Subs        []subRow
	SubFilter   subFilter
	SubStatuses []string
	SubApps     []appOpt
	SubNext     string
	Sub         *subDetail
	// Link is a checkout link issued by the request (shown once).
	Link string
}

func (d *Dashboard) routesSubscriptions() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/subscriptions", d.authed(authz.PaymentRead, d.subscriptionsPage))
	m.HandleFunc("GET "+prefix+"/subscriptions/{id}", d.authed(authz.PaymentRead, d.subscriptionPage))
	m.HandleFunc("POST "+prefix+"/subscriptions/{id}/cancel", d.authed(authz.PaymentCancel, d.subscriptionCancel))
	m.HandleFunc("POST "+prefix+"/subscriptions/{id}/checkout-link", d.authed(authz.PaymentCreate, d.subscriptionLink))
}

func (d *Dashboard) renderSubs(w http.ResponseWriter, status int, page string, v subsView) {
	v.DevBanner = d.o.DevSkip2FA
	var buf bytes.Buffer
	if err := d.o.Renderer.Render(&buf, page, v); err != nil {
		d.o.Logger.Error("dashboard: render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (d *Dashboard) subsEnabled(w http.ResponseWriter) bool {
	if d.o.Subscriptions == nil {
		http.Error(w, "subscriptions are not enabled", http.StatusNotFound)
		return false
	}
	return true
}

func perInterval(amount int64, currency, interval string, count int) string {
	every := interval
	if count > 1 {
		every = "every " + itoa(count) + " " + interval + "s"
	}
	return fmtMoney(amount, currency) + " / " + every
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func (d *Dashboard) subscriptionsPage(w http.ResponseWriter, r *http.Request, s *session) {
	if !d.subsEnabled(w) {
		return
	}
	qv := r.URL.Query()
	f := subFilter{App: qv.Get("app"), Status: qv.Get("status"), Q: strings.TrimSpace(qv.Get("q"))}
	if f.Status != "" {
		known := false
		for _, st := range state.AllSubscriptionStatuses {
			known = known || string(st) == f.Status
		}
		if !known {
			badRequest(w, "unknown status")
			return
		}
	}
	after := qv.Get("after")
	ctx := r.Context()
	var (
		rows []store.Subscription
		apps []store.App
		cust = map[string]store.Customer{}
	)
	err := d.o.DB.Read(ctx, func(q store.Querier) error {
		org, err := store.FirstOrgID(ctx, q)
		if err != nil {
			return err
		}
		if apps, err = store.ListApps(ctx, q, org); err != nil {
			return err
		}
		if rows, err = store.SearchSubscriptions(ctx, q, store.SubFilter{AppID: f.App, Mode: d.mode(r, s), Status: f.Status, Search: f.Q}, after, d.o.PageSize+1); err != nil {
			return err
		}
		for _, sub := range rows {
			if _, ok := cust[sub.CustomerID]; !ok {
				if c, err := store.GetCustomer(ctx, q, sub.CustomerID); err == nil {
					cust[sub.CustomerID] = c
				}
			}
		}
		return nil
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	v := subsView{view: d.shell(r, s, "subscriptions", "Subscriptions"), SubFilter: f}
	names := map[string]string{}
	for _, a := range apps {
		names[a.ID] = a.Name
		v.SubApps = append(v.SubApps, appOpt{ID: a.ID, Name: a.Name})
	}
	for _, st := range state.AllSubscriptionStatuses {
		v.SubStatuses = append(v.SubStatuses, string(st))
	}
	hasMore := len(rows) > d.o.PageSize
	if hasMore {
		rows = rows[:d.o.PageSize]
	}
	for _, sub := range rows {
		c := cust[sub.CustomerID]
		row := subRow{ID: sub.ID, App: names[sub.AppID], Customer: firstNonEmpty(c.Email, c.Name), Status: sub.Status,
			Amount: perInterval(sub.Amount, sub.Currency, sub.Interval, sub.IntervalCount), Billing: sub.BillingMode,
			PeriodEnd: d.fmtTime(sub.CurrentPeriodEnd), Created: d.fmtTime(sub.CreatedAt)}
		if sub.RequiresReauthorization {
			row.Notes = append(row.Notes, "needs re-authorization")
		}
		if sub.CancelAtPeriodEnd {
			row.Notes = append(row.Notes, "cancels at period end")
		}
		if sub.Status == string(state.SubPastDue) {
			row.Notes = append(row.Notes, "retry "+itoa(sub.DunningAttempt))
		}
		v.Subs = append(v.Subs, row)
	}
	if hasMore {
		nq := url.Values{}
		for k, vs := range qv {
			nq[k] = vs
		}
		nq.Set("after", rows[len(rows)-1].ID)
		v.SubNext = prefix + "/subscriptions?" + nq.Encode()
	}
	d.renderSubs(w, http.StatusOK, "dash_subscriptions", v)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (d *Dashboard) subscriptionPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderSubscription(w, r, s, http.StatusOK, "", "", "")
}

func (d *Dashboard) renderSubscription(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg, notice, link string) {
	if !d.subsEnabled(w) {
		return
	}
	ctx := r.Context()
	var (
		sub     store.Subscription
		app     store.App
		cust    store.Customer
		mans    []store.Mandate
		cycles  []store.Cycle
		pays    = map[string]store.Payment{}
		account store.ProviderAccount
	)
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if sub, err = store.GetSubscription(ctx, q, r.PathValue("id")); err != nil {
			return err
		}
		if app, err = store.GetApp(ctx, q, sub.AppID); err != nil {
			return err
		}
		if cust, err = store.GetCustomer(ctx, q, sub.CustomerID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		for _, id := range []string{sub.MandateID, sub.PendingMandateID} {
			if id == "" {
				continue
			}
			m, err := store.GetMandate(ctx, q, id)
			if err != nil {
				return err
			}
			mans = append(mans, m)
		}
		if cycles, err = store.ListCycles(ctx, q, sub.ID); err != nil {
			return err
		}
		for _, c := range cycles {
			if c.PaymentID == "" {
				continue
			}
			if p, err := store.GetPayment(ctx, q, c.PaymentID); err == nil {
				pays[c.PaymentID] = p
			}
		}
		account, _ = store.GetProviderAccount(ctx, q, sub.ProviderAccountID)
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	open := sub.Status != string(state.SubCanceled) && sub.Status != string(state.SubEnded) && sub.Status != string(state.SubIncompleteExpired)
	det := &subDetail{ID: sub.ID, App: app.Name, AppID: app.ID, Mode: sub.Mode, Status: sub.Status, Billing: sub.BillingMode,
		Amount: perInterval(sub.Amount, sub.Currency, sub.Interval, sub.IntervalCount), Customer: firstNonEmpty(cust.Email, cust.Name),
		Description: sub.Description, Created: d.fmtTime(sub.CreatedAt), PeriodStart: d.fmtTime(sub.CurrentPeriodStart),
		PeriodEnd: d.fmtTime(sub.CurrentPeriodEnd), TrialEnd: d.fmtTime(sub.TrialEnd), Anchor: d.fmtTime(sub.AnchorAt),
		CanceledAt: d.fmtTime(sub.CanceledAt), EndedAt: d.fmtTime(sub.EndedAt), ProviderSubscription: sub.ProviderSubscriptionID,
		Dunning: sub.DunningAttempt, CancelAtPeriodEnd: sub.CancelAtPeriodEnd, RequiresReauth: sub.RequiresReauthorization, Open: open,
		CanCancel:          open && s.can(authz.PaymentCancel),
		WaitingForCustomer: sub.BillingMode == "merchant_triggered" && (sub.Status == string(state.SubIncomplete) || sub.PendingMandateID != "" || sub.RequiresReauthorization),
		Renewals:           itoa(sub.RenewalsDone)}
	det.CanLink = det.WaitingForCustomer && open && s.can(authz.PaymentCreate)
	if sub.TotalCount > 0 {
		det.Renewals += " of " + itoa(sub.TotalCount)
	}
	if sub.PendingAmount > 0 {
		det.PendingAmount = fmtMoney(sub.PendingAmount, sub.Currency)
	}
	if !sub.NextRetryAt.IsZero() {
		det.NextRetry = d.fmtTime(sub.NextRetryAt)
	}
	if account.ID != "" {
		det.Billing += " via " + account.Name + " (" + account.Connector + ")"
	}
	for _, m := range mans {
		role := "current"
		if m.ID == sub.PendingMandateID {
			role = "pending replacement"
		}
		det.Mandates = append(det.Mandates, mandateRow{ID: m.ID, Method: m.Method, Status: m.Status, MaxAmount: fmtMoney(m.MaxAmount, m.Currency),
			ValidUntil: d.fmtTime(m.ValidUntil), ProviderRef: m.ProviderRef, ProviderID: m.ProviderMandateID, Role: role})
	}
	for _, c := range cycles {
		row := cycleRow{Period: itoa(c.PeriodIndex), State: c.State, Amount: fmtMoney(c.Amount, sub.Currency), Retries: itoa(c.Retries),
			Notified: d.fmtTime(c.NotifiedAt), NextRun: d.fmtTime(c.NextRunAt), PaymentID: c.PaymentID}
		if c.PeriodIndex < 0 {
			row.Period = "provider charge " + c.ProviderPaymentID
		}
		if p, ok := pays[c.PaymentID]; ok {
			row.PaymentStatus = p.Status
		}
		det.Cycles = append(det.Cycles, row)
	}
	v := subsView{view: d.shell(r, s, "subscriptions", "Subscription "+sub.ID), Sub: det, Link: link}
	v.Error, v.Notice = errMsg, notice
	d.renderSubs(w, status, "dash_subscription", v)
}

func (d *Dashboard) subscriptionCancel(w http.ResponseWriter, r *http.Request, s *session) {
	if !d.subsEnabled(w) {
		return
	}
	ctx := r.Context()
	var sub store.Subscription
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) { sub, err = store.GetSubscription(ctx, q, r.PathValue("id")); return })
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	when := r.PostForm.Get("when")
	if when != "now" && when != "period_end" {
		d.renderSubscription(w, r, s, http.StatusBadRequest, "Choose whether to cancel now or at the end of the period.", "", "")
		return
	}
	_, err = d.o.Subscriptions.CancelBy(ctx, core.Scope{AppID: sub.AppID, Mode: sub.Mode}, sub.ID, when == "period_end", s.auditActor())
	switch {
	case err == nil:
		http.Redirect(w, r, prefix+"/subscriptions/"+sub.ID, http.StatusSeeOther)
	case errors.Is(err, subscriptions.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, subscriptions.ErrInvalidState), errors.Is(err, subscriptions.ErrInvalid):
		d.renderSubscription(w, r, s, http.StatusConflict, err.Error(), "", "")
	default:
		d.fail(w, err)
	}
}

func (d *Dashboard) subscriptionLink(w http.ResponseWriter, r *http.Request, s *session) {
	if !d.subsEnabled(w) {
		return
	}
	ctx := r.Context()
	var sub store.Subscription
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) { sub, err = store.GetSubscription(ctx, q, r.PathValue("id")); return })
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	res, err := d.o.Subscriptions.IssueCheckoutLink(ctx, core.Scope{AppID: sub.AppID, Mode: sub.Mode}, sub.ID, s.auditActor())
	switch {
	case err == nil:
		// Shown once: the page that answers this POST carries the link.
		d.renderSubscription(w, r, s, http.StatusOK, "", "New checkout link created. Any earlier link no longer works.", res.CheckoutURL)
	case errors.Is(err, subscriptions.ErrInvalidState), errors.Is(err, subscriptions.ErrAmountExceedsMandateLimit):
		d.renderSubscription(w, r, s, http.StatusConflict, err.Error(), "", "")
	case errors.Is(err, subscriptions.ErrNotFound):
		http.NotFound(w, r)
	default:
		d.fail(w, err)
	}
}
