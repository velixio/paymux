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
	// logs (the Logs page and a log entry)
	LogFilter    logFilter
	LogLevels    []string
	LogKinds     []string
	LogRows      []logRowView
	LogEntry     *logDetail
	LogExportURL string

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
	Table    *settingsTable
	Routing  *routingView
	Route    *routeResult
	TestForm testForm

	// invoices and credit notes
	DocRows        []docRow
	DocFilter      docFilter
	DocFlags       []string
	Entities       []entityOpt
	FlaggedRefunds []flaggedRefund
	Doc            *docView

	// disputes
	DispRows        []dispRow
	DispFilter      dispFilter
	DisputeStatuses []string
	ProviderOpts    []string
	Disp            *dispView
	CreditCodes     []codeOpt

	// reconciliation
	ReconAccounts []reconAcct

	// alerts
	AlertRows   []alertRow
	AlertFilter alertFilter
	AlertKinds  []string

	// payment links
	LinkRows   []linkRow
	LinkFilter linkFilter
	LinkForm   linkFormValues

	// appearance
	ThemeRows      []themeRow
	CurrentTheme   string
	Languages      []string
	AppearanceRows []appearanceRow
	PreviewTheme   themeRow
	ReconRuns      []reconRunRow
	ReconItems     []reconItemRow
	ReconFilter    reconFilter
	ReconKinds     []string

	// provider accounts
	Providers []provRow
	ConnOpts  []connOpt
	Prov      *provForm

	// reports, tax entities, fx
	Reports   *reportsView
	TaxEnts   []taxEntRow
	TaxForm   *taxEntForm
	Warnings  []string
	FXRates   []fxRow
	FXPending []fxPendingRow
	FXDate    string

	// audit log and overview
	AuditRows   []auditRow
	AuditFilter auditFilter
	Periods     []periodView
	Revenue     revenueView
}

type appOpt struct{ ID, Name string }

// routesPages registers the part-2 pages.
func (d *Dashboard) routesPages() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/payments", d.authed(authz.PaymentRead, d.paymentsPage))
	m.HandleFunc("GET "+prefix+"/payments/{id}", d.authed(authz.PaymentRead, d.paymentPage))
	m.HandleFunc("POST "+prefix+"/payments/{id}/refund", d.authed(authz.RefundCreate, d.refundSubmit))
	m.HandleFunc("POST "+prefix+"/payments/{id}/simulate-dispute", d.authed(authz.DisputeManage, d.simulateDispute))
	m.HandleFunc("POST "+prefix+"/payments/{id}/simulate-refund", d.authed(authz.DisputeManage, d.simulateProviderRefund))
	m.HandleFunc("POST "+prefix+"/deliveries/{id}/replay", d.authed(authz.WebhookReplay, d.replaySubmit))
	d.routesApps()
	d.routesSettings()
	d.routesAuditOverview()
	d.routesLogs()
	d.routesInvoices()
	d.routesTax()
	d.routesProviders()
	d.routesDomains()
	d.routesDisputes()
	d.routesRecon()
	d.routesAlerts()
	d.routesEInvoice()
	d.routesPaymentLinks()
	d.routesAppearance()
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

// sessionOrg is the org a signed-in user belongs to; "" when it cannot be read,
// which authz treats as no org check (the handlers' own inOrg checks still run).
func (d *Dashboard) sessionOrg(ctx context.Context) string {
	org, _ := d.orgID(ctx)
	return org
}

// inOrg reports whether an object belongs to the dashboard's org. Handlers
// answer 404 for anything else, so ids of another org reveal nothing.
func (d *Dashboard) inOrg(ctx context.Context, objectOrg string) bool {
	org, err := d.orgID(ctx)
	return err == nil && org != "" && org == objectOrg
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
			Before: before, After: after, IP: d.clientIP(r)})
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
