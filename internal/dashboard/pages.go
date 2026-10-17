package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector/money"
)

// Page holds the page-specific data of the part-2 templates. It is embedded
// in view, so templates address its fields directly.
type Page struct {
	// payments list
	Rows     []payRow
	Filter   payFilter
	AppOpts  []appOpt
	Statuses []string
	NextURL  string

	// payment detail
	Pay *payDetail

	// apps
	Apps   []appRow
	App    *appView
	Reveal *secretReveal // a secret shown once

	// settings (app tab and org page)
	Table *settingsTable
}

type appOpt struct{ ID, Name string }

// routesPages registers the part-2 pages.
func (d *Dashboard) routesPages() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/payments", d.authed(authz.PaymentRead, d.paymentsPage))
	m.HandleFunc("GET "+prefix+"/payments/{id}", d.authed(authz.PaymentRead, d.paymentPage))
	m.HandleFunc("POST "+prefix+"/payments/{id}/refund", d.authed(authz.RefundCreate, d.refundSubmit))
	m.HandleFunc("POST "+prefix+"/deliveries/{id}/replay", d.authed(authz.WebhookReplay, d.replaySubmit))
	d.routesApps()
}

// orgID returns the single org's id (cached once found).
func (d *Dashboard) orgID(ctx context.Context) (string, error) {
	d.orgMu.Lock()
	defer d.orgMu.Unlock()
	if d.org != "" {
		return d.org, nil
	}
	id, err := d.o.DB.FirstOrgID(ctx)
	if err != nil {
		return "", err
	}
	d.org = id
	return id, nil
}

func (s *session) auditActor() audit.Actor {
	return audit.Actor{Type: audit.ActorUser, ID: s.User.ID}
}

// record audits one action in its own transaction.
func (d *Dashboard) record(ctx context.Context, r *http.Request, s *session, action, target string, before, after any) error {
	org, err := d.orgID(ctx)
	if err != nil {
		return err
	}
	return d.o.DB.Write(ctx, func(tx store.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{OrgID: org, Actor: s.auditActor(), Action: action, Target: target,
			Before: before, After: after, IP: clientIP(r)})
	})
}

func (d *Dashboard) fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(d.o.Location).Format("2006-01-02 15:04:05")
}

// fmtMoney renders minor units as "100.00 INR".
func fmtMoney(minor int64, currency string) string {
	s, err := money.FormatDecimal(minor, currency)
	if err != nil {
		return fmt.Sprintf("%d %s", minor, currency)
	}
	return s + " " + currency
}

func badRequest(w http.ResponseWriter, msg string) { http.Error(w, msg, http.StatusBadRequest) }
