package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/disputes"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/money"
)

// dueSoon is the window in which an open dispute's deadline is flagged red.
const dueSoon = 72 * time.Hour

var disputeStatuses = []string{"needs_response", "under_review", "won", "lost", "accepted", "closed"}

// creditReasons are the reason codes of "Issue credit note". The code is kept
// in the audit log next to the credit note.
var creditReasons = []codeOpt{
	{"chargeback_lost", "Chargeback lost"}, {"chargeback_accepted", "Chargeback accepted"},
	{"settled_with_customer", "Settled with the customer"}, {"other", "Other"},
}

type codeOpt struct{ Code, Label string }

// portalLinks are the provider dashboards to answer a dispute in when the
// connector cannot do it (the exact page changes; these are the dashboards).
var portalLinks = map[string]struct{ Name, URL string }{
	"razorpay": {"Razorpay Dashboard", "https://dashboard.razorpay.com/"},
	"payu":     {"PayU merchant dashboard", "https://onboarding.payu.in/"},
	"zoho":     {"Zoho Payments", "https://payments.zoho.in/"},
	"whop":     {"Whop dashboard", "https://whop.com/dashboard/"},
}

type dispFilter struct{ Status, App, Provider string }

type dispRow struct {
	ID, App, Provider, Status, Amount, Reason, Stage, Due, Source string
	DueSoon                                                       bool
}

type evidenceRow struct {
	ID, ContentType, Size, At, Href string
}

type timelineRow struct{ At, Text string }

type dispView struct {
	ID, App, Provider, Account, Status, Stage, Reason, Amount, Due, Source, ProviderID string
	PaymentID, PaymentRef, Customer, Created, Updated                                  string
	Open, CanAct, CanRespond, CanMark, CanCredit                                       bool
	Notes                                                                              string
	Evidence                                                                           []evidenceRow
	Timeline                                                                           []timelineRow
	Portal                                                                             struct{ Name, URL string }
	MarkOptions                                                                        []string
}

func (d *Dashboard) routesDisputes() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/disputes", d.authed(authz.PaymentRead, d.disputesPage))
	m.HandleFunc("POST "+prefix+"/disputes", d.authed(authz.DisputeManage, d.disputeManual))
	m.HandleFunc("GET "+prefix+"/disputes/{id}", d.authed(authz.PaymentRead, d.disputePage))
	m.HandleFunc("GET "+prefix+"/disputes/{id}/evidence/{blob}", d.authed(authz.PaymentRead, d.disputeEvidenceFile))
	m.HandleFunc("POST "+prefix+"/disputes/{id}/accept", d.authed(authz.DisputeManage, d.disputeAccept))
	m.HandleFunc("POST "+prefix+"/disputes/{id}/evidence", d.authed(authz.DisputeManage, d.disputeEvidence))
	m.HandleFunc("POST "+prefix+"/disputes/{id}/status", d.authed(authz.DisputeManage, d.disputeStatus))
	m.HandleFunc("POST "+prefix+"/disputes/{id}/credit-note", d.authed(authz.DisputeManage, d.disputeCreditNote))
}

func isOpenDispute(status string) bool { return status == "needs_response" || status == "under_review" }

func (d *Dashboard) disputesPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderDisputes(w, r, s, http.StatusOK, "")
}

func (d *Dashboard) renderDisputes(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg string) {
	ctx := r.Context()
	qv := r.URL.Query()
	f := dispFilter{Status: qv.Get("status"), App: qv.Get("app"), Provider: qv.Get("provider")}
	if f.Status != "" && !slices.Contains(disputeStatuses, f.Status) {
		badRequest(w, "unknown status")
		return
	}
	scope, err := d.orgScope(r)
	if err != nil {
		d.fail(w, err)
		return
	}
	all, err := d.o.Disputes.List(ctx, scope, false)
	if err != nil {
		d.fail(w, err)
		return
	}
	var apps []store.App
	var accts []store.ProviderAccountInfo
	err = d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if apps, err = store.ListApps(ctx, q, scope.OrgID); err != nil {
			return err
		}
		accts, err = store.ListProviderAccounts(ctx, q, scope.OrgID, d.mode(r, s))
		return err
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	appName, connOf := map[string]string{}, map[string]string{}
	v := d.shell(r, s, "disputes", "Disputes")
	v.Error, v.CanManage = errMsg, s.can(authz.DisputeManage)
	for _, a := range apps {
		appName[a.ID] = a.Name
		v.AppOpts = append(v.AppOpts, appOpt{ID: a.ID, Name: a.Name})
	}
	if f.App != "" && appName[f.App] == "" {
		badRequest(w, "unknown app")
		return
	}
	providers := map[string]bool{}
	for _, a := range accts {
		connOf[a.ID] = a.Connector
		providers[a.Connector] = true
	}
	for p := range providers {
		v.ProviderOpts = append(v.ProviderOpts, p)
	}
	slices.Sort(v.ProviderOpts)
	mode, now := d.mode(r, s), d.o.Now()
	for _, dp := range all {
		if dp.Mode != mode || f.Status != "" && dp.Status != f.Status || f.App != "" && dp.AppID != f.App || f.Provider != "" && connOf[dp.ProviderAccountID] != f.Provider {
			continue
		}
		row := dispRow{ID: dp.ID, App: appName[dp.AppID], Provider: connOf[dp.ProviderAccountID], Status: dp.Status, Amount: fmtMoney(dp.Amount, dp.Currency),
			Reason: dp.Reason, Stage: dp.Stage, Due: d.fmtTime(dp.DueBy), Source: dp.Source}
		row.DueSoon = isOpenDispute(dp.Status) && !dp.DueBy.IsZero() && dp.DueBy.Sub(now) < dueSoon
		v.DispRows = append(v.DispRows, row)
	}
	v.DispFilter, v.DisputeStatuses = f, disputeStatuses
	d.render(w, status, "dash_disputes", v)
}

// manualDueBy turns a date into the end of that day (IST), the deadline a
// provider portal states.
func manualDueBy(s string) (time.Time, error) {
	t, err := parseISTDate("The deadline", s)
	if err != nil || t.IsZero() {
		return t, err
	}
	return t.Add(24*time.Hour - time.Second), nil
}

// disputeErr answers the usual service errors; it reports false when err is
// not one of them.
func (d *Dashboard) disputeErr(w http.ResponseWriter, r *http.Request, s *session, err error, retry func(status int, msg string)) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, core.ErrInvalid), errors.Is(err, core.ErrInvalidState):
		retry(http.StatusBadRequest, err.Error())
	default:
		var fe fieldErr
		if errors.As(err, &fe) {
			retry(http.StatusBadRequest, err.Error())
			return
		}
		d.o.Logger.Error("dashboard: dispute action", "err", err)
		retry(http.StatusBadGateway, "The action failed: "+err.Error())
	}
}

func (d *Dashboard) disputeManual(w http.ResponseWriter, r *http.Request, s *session) {
	f := r.PostForm
	retry := func(status int, msg string) { d.renderDisputes(w, r, s, status, msg) }
	scope, err := d.orgScope(r)
	if err != nil {
		d.fail(w, err)
		return
	}
	var p store.Payment
	err = d.o.DB.Read(r.Context(), func(q store.Querier) (err error) {
		p, err = store.GetPayment(r.Context(), q, strings.TrimSpace(f.Get("payment_id")))
		return
	})
	if errors.Is(err, store.ErrNotFound) || err == nil && (p.OrgID != scope.OrgID || p.Mode != d.mode(r, s)) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	amount, err := money.ParseDecimal(strings.TrimSpace(f.Get("amount")), p.Currency)
	if err != nil || amount <= 0 {
		retry(http.StatusBadRequest, "Enter the disputed amount as a positive number like 250.50.")
		return
	}
	due, err := manualDueBy(f.Get("due_by"))
	if err != nil {
		retry(http.StatusBadRequest, err.Error())
		return
	}
	dp, err := d.o.Disputes.CreateManual(r.Context(), s.auditActor(), scope, disputes.ManualInput{PaymentID: p.ID, Amount: amount,
		Reason: strings.TrimSpace(f.Get("reason")), Stage: strings.TrimSpace(f.Get("stage")), DueBy: due})
	if err != nil {
		d.disputeErr(w, r, s, err, retry)
		return
	}
	http.Redirect(w, r, prefix+"/disputes/"+dp.ID, http.StatusSeeOther)
}

// loadDispute answers 404 for an unknown dispute or one of another org.
func (d *Dashboard) loadDispute(w http.ResponseWriter, r *http.Request, s *session) (store.Dispute, bool) {
	scope, err := d.orgScope(r)
	if err != nil {
		d.fail(w, err)
		return store.Dispute{}, false
	}
	dp, err := d.o.Disputes.Get(r.Context(), scope, r.PathValue("id"))
	if errors.Is(err, core.ErrNotFound) {
		http.NotFound(w, r)
		return dp, false
	}
	if err != nil {
		d.fail(w, err)
		return dp, false
	}
	if dp.Mode != d.mode(r, s) {
		http.NotFound(w, r)
		return dp, false
	}
	return dp, true
}

func (d *Dashboard) disputePage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderDispute(w, r, s, http.StatusOK, "")
}

func (d *Dashboard) renderDispute(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg string) {
	ctx := r.Context()
	dp, ok := d.loadDispute(w, r, s)
	if !ok {
		return
	}
	var (
		p     store.Payment
		app   store.App
		acct  store.ProviderAccount
		cust  store.Customer
		evs   []store.EventRow
		aud   []store.AuditRow
		blobs []store.Blob
		doc   struct {
			Notes   string   `json:"notes"`
			BlobIDs []string `json:"blob_ids"`
		}
	)
	_ = json.Unmarshal([]byte(dp.Evidence), &doc)
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if p, err = store.GetPayment(ctx, q, dp.PaymentID); err != nil {
			return err
		}
		if app, err = store.GetApp(ctx, q, dp.AppID); err != nil {
			return err
		}
		if acct, err = store.GetProviderAccount(ctx, q, dp.ProviderAccountID); err != nil {
			return err
		}
		if p.CustomerID != "" {
			if cust, err = store.GetCustomer(ctx, q, p.CustomerID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		if evs, err = store.ListObjectEvents(ctx, q, dp.OrgID, "dispute", dp.ID); err != nil {
			return err
		}
		if aud, err = store.SearchAudit(ctx, q, store.AuditFilter{OrgID: dp.OrgID, Target: dp.ID}, 100); err != nil {
			return err
		}
		for _, id := range doc.BlobIDs {
			b, err := store.GetBlob(ctx, q, id)
			if errors.Is(err, store.ErrNotFound) || err == nil && b.OrgID != dp.OrgID {
				continue
			}
			if err != nil {
				return err
			}
			blobs = append(blobs, b)
		}
		return nil
	})
	if err != nil {
		d.fail(w, err)
		return
	}

	v := &dispView{ID: dp.ID, App: app.Name, Provider: acct.Connector, Account: acct.Name, Status: dp.Status, Stage: dp.Stage, Reason: dp.Reason,
		Amount: fmtMoney(dp.Amount, dp.Currency), Due: d.fmtTime(dp.DueBy), Source: dp.Source, ProviderID: dp.ProviderDisputeID, PaymentID: p.ID,
		PaymentRef: p.OrderRef, Customer: cust.Email, Created: d.fmtTime(dp.CreatedAt), Updated: d.fmtTime(dp.UpdatedAt), Notes: doc.Notes,
		Open: isOpenDispute(dp.Status), CanAct: s.can(authz.DisputeManage)}
	if live, err := d.o.Connectors.Get(ctx, dp.ProviderAccountID); err == nil {
		_, v.CanRespond = live.Connector.(connector.DisputeResponder)
	}
	v.CanRespond = v.CanRespond && v.Open && dp.Source != "manual"
	if v.Open {
		if link, ok := portalLinks[acct.Connector]; ok {
			v.Portal.Name, v.Portal.URL = link.Name, link.URL
		} else {
			v.Portal.Name = "the provider's portal"
		}
	}
	if dp.Source == "manual" && v.Open {
		for _, st := range disputeStatuses {
			if st != dp.Status && st != "needs_response" {
				v.MarkOptions = append(v.MarkOptions, st)
			}
		}
	}
	v.CanMark = len(v.MarkOptions) > 0
	v.CanCredit = dp.Status == "lost" || dp.Status == "accepted"
	for _, b := range blobs {
		v.Evidence = append(v.Evidence, evidenceRow{ID: b.ID, ContentType: b.ContentType, Size: fmt.Sprintf("%d KB", (b.Size+1023)/1024), At: d.fmtTime(b.CreatedAt),
			Href: prefix + "/disputes/" + dp.ID + "/evidence/" + b.ID})
	}
	type entry struct {
		at   time.Time
		text string
	}
	tl := []entry{{dp.CreatedAt, "Dispute recorded (" + dp.Source + ")"}}
	for _, e := range evs {
		tl = append(tl, entry{e.CreatedAt, fmt.Sprintf("Event %s (v%d)", e.Type, e.ObjectVersion)})
	}
	for _, a := range aud {
		who := a.ActorType
		if a.ActorID != "" {
			who += " " + a.ActorID
		}
		tl = append(tl, entry{a.At, fmt.Sprintf("%s by %s", a.Action, who)})
	}
	slices.SortStableFunc(tl, func(a, b entry) int { return a.at.Compare(b.at) })
	for _, e := range tl {
		v.Timeline = append(v.Timeline, timelineRow{At: d.fmtTime(e.at), Text: e.text})
	}

	page := d.shell(r, s, "disputes", "Dispute "+dp.ID)
	page.Disp, page.Error, page.CreditCodes = v, errMsg, creditReasons
	d.render(w, status, "dash_dispute", page)
}

func (d *Dashboard) disputeEvidenceFile(w http.ResponseWriter, r *http.Request, s *session) {
	dp, ok := d.loadDispute(w, r, s)
	if !ok {
		return
	}
	var doc struct {
		BlobIDs []string `json:"blob_ids"`
	}
	_ = json.Unmarshal([]byte(dp.Evidence), &doc)
	id := r.PathValue("blob")
	if !slices.Contains(doc.BlobIDs, id) {
		http.NotFound(w, r)
		return
	}
	var b store.Blob
	err := d.o.DB.Read(r.Context(), func(q store.Querier) (err error) { b, err = store.GetBlob(r.Context(), q, id); return })
	if errors.Is(err, store.ErrNotFound) || err == nil && b.OrgID != dp.OrgID {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", b.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, b.ID))
	_, _ = w.Write(b.Data)
}

func (d *Dashboard) disputeAction(w http.ResponseWriter, r *http.Request, s *session, do func(scope core.OrgScope) error, then func() string) {
	if _, ok := d.loadDispute(w, r, s); !ok {
		return
	}
	scope, err := d.orgScope(r)
	if err != nil {
		d.fail(w, err)
		return
	}
	if err := do(scope); err != nil {
		d.disputeErr(w, r, s, err, func(status int, msg string) { d.renderDispute(w, r, s, status, msg) })
		return
	}
	http.Redirect(w, r, then(), http.StatusSeeOther)
}

func (d *Dashboard) disputeAccept(w http.ResponseWriter, r *http.Request, s *session) {
	d.disputeAction(w, r, s, func(sc core.OrgScope) error {
		_, err := d.o.Disputes.Accept(r.Context(), s.auditActor(), sc, r.PathValue("id"))
		return err
	}, func() string { return prefix + "/disputes/" + r.PathValue("id") })
}

func (d *Dashboard) disputeEvidence(w http.ResponseWriter, r *http.Request, s *session) {
	var in disputes.EvidenceInput
	in.Text = strings.TrimSpace(r.PostForm.Get("notes"))
	for i := 1; i <= 3; i++ {
		if r.MultipartForm == nil {
			break
		}
		fhs := r.MultipartForm.File[fmt.Sprintf("file_%d", i)]
		if len(fhs) == 0 || fhs[0].Filename == "" {
			continue
		}
		if fhs[0].Size > disputes.MaxEvidenceBytes {
			d.renderDispute(w, r, s, http.StatusBadRequest, "Each evidence file must be 10 MiB or smaller.")
			return
		}
		fh, err := fhs[0].Open()
		if err != nil {
			d.fail(w, err)
			return
		}
		data, err := io.ReadAll(io.LimitReader(fh, disputes.MaxEvidenceBytes+1))
		fh.Close()
		if err != nil {
			d.fail(w, err)
			return
		}
		in.Files = append(in.Files, disputes.EvidenceFile{Type: strings.TrimSpace(r.PostForm.Get(fmt.Sprintf("type_%d", i))),
			ContentType: http.DetectContentType(data), Data: data})
	}
	d.disputeAction(w, r, s, func(sc core.OrgScope) error {
		_, err := d.o.Disputes.SubmitEvidence(r.Context(), s.auditActor(), sc, r.PathValue("id"), in)
		return err
	}, func() string { return prefix + "/disputes/" + r.PathValue("id") })
}

func (d *Dashboard) disputeStatus(w http.ResponseWriter, r *http.Request, s *session) {
	to := r.PostForm.Get("status")
	if !slices.Contains(disputeStatuses, to) {
		d.renderDispute(w, r, s, http.StatusBadRequest, "Choose a status to mark the dispute as.")
		return
	}
	d.disputeAction(w, r, s, func(sc core.OrgScope) error {
		_, err := d.o.Disputes.SetManualStatus(r.Context(), s.auditActor(), sc, r.PathValue("id"), state.DisputeStatus(to))
		return err
	}, func() string { return prefix + "/disputes/" + r.PathValue("id") })
}

func (d *Dashboard) disputeCreditNote(w http.ResponseWriter, r *http.Request, s *session) {
	code := r.PostForm.Get("reason_code")
	if !slices.ContainsFunc(creditReasons, func(c codeOpt) bool { return c.Code == code }) {
		d.renderDispute(w, r, s, http.StatusBadRequest, "Choose a reason code for the credit note.")
		return
	}
	var cn *store.CreditNote
	d.disputeAction(w, r, s, func(sc core.OrgScope) error {
		var err error
		if cn, err = d.o.Disputes.IssueCreditNote(r.Context(), s.auditActor(), sc, r.PathValue("id")); err != nil {
			return err
		}
		return d.record(r.Context(), r, s, "dispute.credit_note_reason", r.PathValue("id"), nil,
			map[string]string{"reason_code": code, "credit_note_id": cn.ID})
	}, func() string { return prefix + "/credit-notes/" + cn.ID })
}

// disputeCount is the number of disputes needing a response, for the nav; a
// failure shows no count rather than failing the page.
func (d *Dashboard) disputeCount(r *http.Request) (n int) {
	org, err := d.orgID(r.Context())
	if err != nil {
		return 0
	}
	_ = d.o.DB.Read(r.Context(), func(q store.Querier) (err error) {
		n, err = store.CountDisputesNeedingResponse(r.Context(), q, org)
		return
	})
	return n
}
