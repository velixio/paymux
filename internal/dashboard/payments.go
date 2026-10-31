package dashboard

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/velixio/paymux/connectors/simulator"
	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/money"
)

type payRow struct {
	ID, App, Status, StatusClass, Amount, Customer, OrderRef, Created string
	Provider, ProviderKey, ProviderMark, Method, MethodMeta           string
	Attempts                                                          int
}

type payFilter struct{ App, Status, Q, From, To string }

type attemptRow struct {
	ID, Status, Provider, ProviderRef, OrderID, PaymentID, Method, FailureCode, FailureMessage, Created string
	MethodSummary, MethodMeta                                                                           string
	Duplicate                                                                                           bool
}

type providerAttemptRow struct {
	PaymentID, AttemptID, Status, Provider, OrderID, Method, Failure, Amount, Created string
}

type customerDetail struct {
	ExternalRef, Name, Email, Phone                                      string
	LegalName, GSTIN, Line1, Line2, City, StateCode, PostalCode, Country string
	HasBilling                                                           bool
}

type ledgerRow struct{ Kind, Amount, At string }

type refundRow struct{ ID, Amount, Status, Reason, InitiatedBy, Actor, Created string }

type deliveryRow struct {
	ID, EventType, Endpoint, Status, LastError, At string
	Attempts, LastStatus                           int
}

type eventRow struct {
	Type, Object string
	Version      int64
	At           string
}

type attemptOpt struct{ ID, Label string }

type payDetail struct {
	ID, App, AppID, Mode, Status, StatusClass, Amount, Received, Refunded, OrderRef, Description, Created string
	Currency                                                                                              string
	Customer                                                                                              *customerDetail
	Flags                                                                                                 []string
	Attempts                                                                                              []attemptRow
	ProviderAttempts                                                                                      []providerAttemptRow
	Ledger                                                                                                []ledgerRow
	Refunds                                                                                               []refundRow
	Deliveries                                                                                            []deliveryRow
	Events                                                                                                []eventRow
	Pickable                                                                                              []attemptOpt
	CanRefund, CanReplay, CanSync                                                                         bool
	// RefundNonce is the single-use key of the refund form on this page.
	RefundNonce string
	// SimAttempts are the captured attempts on a test-mode simulator account,
	// the ones a dispute can be simulated on.
	SimAttempts  []attemptOpt
	CanSimulate  bool
	AttemptCount int
}

// parseDay parses YYYY-MM-DD in the dashboard's time zone.
func (d *Dashboard) parseDay(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", s, d.o.Location)
}

func (d *Dashboard) paymentsPage(w http.ResponseWriter, r *http.Request, s *session) {
	qv := r.URL.Query()
	f := payFilter{App: qv.Get("app"), Status: qv.Get("status"), Q: strings.TrimSpace(qv.Get("q")), From: qv.Get("from"), To: qv.Get("to")}
	org, err := d.orgID(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	sf := store.PaymentFilter{OrgID: org, AppID: f.App, Mode: d.mode(r, s), Status: f.Status, Search: f.Q}
	if f.Status != "" {
		known := false
		for _, st := range state.AllPaymentStatuses {
			known = known || string(st) == f.Status
		}
		if !known {
			badRequest(w, "unknown status")
			return
		}
	}
	if f.From != "" {
		t, err := d.parseDay(f.From)
		if err != nil {
			badRequest(w, "from must be a date like 2026-10-01")
			return
		}
		sf.From = t
	}
	if f.To != "" {
		t, err := d.parseDay(f.To)
		if err != nil {
			badRequest(w, "to must be a date like 2026-10-01")
			return
		}
		sf.To = t.AddDate(0, 0, 1) // the "to" day is included
	}
	after := qv.Get("after")

	var rows []store.Payment
	var apps []store.App
	var accounts []store.ProviderAccountInfo
	var attempts []store.Attempt
	err = d.o.DB.Read(r.Context(), func(q store.Querier) (err error) {
		if apps, err = store.ListApps(r.Context(), q, org); err != nil {
			return err
		}
		if rows, err = store.SearchPayments(r.Context(), q, sf, after, d.o.PageSize+1); err != nil {
			return err
		}
		visible := rows
		if len(visible) > d.o.PageSize {
			visible = visible[:d.o.PageSize]
		}
		ids := make([]string, len(visible))
		for i := range visible {
			ids[i] = visible[i].ID
		}
		if attempts, err = store.ListAttemptsForPayments(r.Context(), q, ids); err != nil {
			return err
		}
		accounts, err = store.ListProviderAccounts(r.Context(), q, org, d.mode(r, s))
		return err
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	names := map[string]string{}
	accountConnectors := map[string]string{}
	attemptsByPayment := map[string][]store.Attempt{}
	v := d.shell(r, s, "payments", "Payments")
	for _, a := range apps {
		names[a.ID] = a.Name
		v.AppOpts = append(v.AppOpts, appOpt{ID: a.ID, Name: a.Name})
	}
	for _, account := range accounts {
		accountConnectors[account.ID] = account.Connector
	}
	for _, attempt := range attempts {
		attemptsByPayment[attempt.PaymentID] = append(attemptsByPayment[attempt.PaymentID], attempt)
	}
	hasMore := len(rows) > d.o.PageSize
	if hasMore {
		rows = rows[:d.o.PageSize]
	}
	for _, p := range rows {
		status, statusClass := paymentDisplayStatus(p)
		row := payRow{ID: p.ID, App: names[p.AppID], Status: status, StatusClass: statusClass, Amount: fmtMoney(p.Amount, p.Currency), OrderRef: p.OrderRef,
			Created: d.fmtTime(p.CreatedAt)}
		paymentAttempts := attemptsByPayment[p.ID]
		row.Attempts = len(paymentAttempts)
		if selected, ok := paymentListAttempt(p, paymentAttempts); ok {
			connectorName := accountConnectors[selected.ProviderAccountID]
			row.ProviderKey = connectorName
			row.ProviderMark = paymentProviderMark(connectorName)
			if _, info, found := connector.Lookup(connectorName); found {
				row.Provider = info.DisplayName
			} else {
				row.Provider = connectorName
			}
			row.Method, row.MethodMeta = paymentListMethod(selected)
		}
		if p.CustomerID != "" {
			_ = d.o.DB.Read(r.Context(), func(q store.Querier) error {
				if c, err := store.GetCustomer(r.Context(), q, p.CustomerID); err == nil {
					row.Customer = c.Email
				}
				return nil
			})
		}
		v.Rows = append(v.Rows, row)
	}
	v.Filter = f
	for _, st := range state.AllPaymentStatuses {
		v.Statuses = append(v.Statuses, string(st))
	}
	if hasMore {
		next := url.Values{}
		for k, val := range map[string]string{"app": f.App, "status": f.Status, "q": f.Q, "from": f.From, "to": f.To} {
			if val != "" {
				next.Set(k, val)
			}
		}
		next.Set("after", rows[len(rows)-1].ID)
		v.NextURL = prefix + "/payments?" + next.Encode()
	}
	d.render(w, http.StatusOK, "dash_payments", v)
}

func paymentProviderMark(name string) string {
	marks := map[string]string{
		"cashfree":  "CF",
		"payu":      "PU",
		"razorpay":  "RZ",
		"remote":    "RM",
		"simulator": "SM",
		"stripe":    "ST",
		"whop":      "WH",
		"zoho":      "ZH",
	}
	if mark := marks[name]; mark != "" {
		return mark
	}
	name = strings.ToUpper(name)
	if len(name) > 2 {
		return name[:2]
	}
	return name
}

// paymentListAttempt chooses the attempt that best explains the payment. A
// captured non-duplicate attempt is authoritative for succeeded payments;
// otherwise the newest attempt describes what the customer sees now.
func paymentListAttempt(p store.Payment, attempts []store.Attempt) (store.Attempt, bool) {
	if p.Status == string(state.PaymentSucceeded) {
		for i := len(attempts) - 1; i >= 0; i-- {
			if attempts[i].Status == string(state.AttemptCaptured) && !attempts[i].Duplicate {
				return attempts[i], true
			}
		}
	}
	if len(attempts) == 0 {
		return store.Attempt{}, false
	}
	return attempts[len(attempts)-1], true
}

func paymentListMethod(a store.Attempt) (string, string) {
	summary, meta := paymentMethodSummary(a)
	method := map[string]string{
		"card": "Card", "upi": "UPI", "netbanking": "Net banking", "wallet": "Wallet",
		"bank_transfer": "Bank transfer", "emi": "EMI", "paylater": "Pay later",
	}[strings.ToLower(a.Method)]
	if method == "" {
		method = a.Method
	}
	switch strings.ToLower(a.Method) {
	case "card":
		if summary != "" {
			method = strings.ReplaceAll(summary, "•••• ", "")
		}
	case "upi", "netbanking", "wallet", "bank_transfer":
		if summary != "" {
			if meta != "" {
				meta += " · "
			}
			meta += strings.ReplaceAll(summary, "_", " ")
		}
	default:
		if method == "" {
			method = summary
		}
	}
	return method, meta
}

func (d *Dashboard) paymentPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderPayment(w, r, s, http.StatusOK, "", "")
}

// renderPayment loads and renders one payment's page; a missing payment is a 404.
func (d *Dashboard) renderPayment(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg, notice string) {
	ctx := r.Context()
	id := r.PathValue("id")
	var (
		p                store.Payment
		app              store.App
		attempts         []store.Attempt
		providerAttempts []store.ProviderPaymentAttempt
		ledger           []store.LedgerEntry
		refunds          []store.Refund
		events           []store.EventRow
		dels             []store.DeliveryListRow
		accounts         []store.ProviderAccountInfo
		cust             store.Customer
	)
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if p, err = store.GetPayment(ctx, q, id); err != nil {
			return err
		}
		if app, err = store.GetApp(ctx, q, p.AppID); err != nil {
			return err
		}
		if attempts, err = store.ListAttempts(ctx, q, id); err != nil {
			return err
		}
		if providerAttempts, err = store.ListProviderPaymentAttempts(ctx, q, id); err != nil {
			return err
		}
		if ledger, err = store.ListLedgerEntries(ctx, q, id); err != nil {
			return err
		}
		if refunds, err = store.ListRefunds(ctx, q, id); err != nil {
			return err
		}
		if events, err = store.ListPaymentEvents(ctx, q, id); err != nil {
			return err
		}
		if dels, err = store.ListPaymentDeliveries(ctx, q, id); err != nil {
			return err
		}
		if accounts, err = store.ListProviderAccounts(ctx, q, p.OrgID, p.Mode); err != nil {
			return err
		}
		if p.CustomerID != "" {
			if cust, err = store.GetCustomer(ctx, q, p.CustomerID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) || err == nil && (!d.inOrg(ctx, p.OrgID) || p.Mode != d.mode(r, s)) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}

	provider := map[string]string{}
	connectorOf := map[string]string{}
	for _, a := range accounts {
		provider[a.ID] = a.Name + " (" + a.Connector + ")"
		connectorOf[a.ID] = a.Connector
	}
	displayStatus, statusClass := paymentDisplayStatus(p)
	det := &payDetail{ID: p.ID, App: app.Name, AppID: app.ID, Mode: p.Mode, Status: displayStatus, StatusClass: statusClass, Currency: p.Currency,
		Amount: fmtMoney(p.Amount, p.Currency), Received: fmtMoney(p.AmountReceived, p.Currency), Refunded: fmtMoney(p.AmountRefunded, p.Currency),
		OrderRef: p.OrderRef, Description: p.Description, Created: d.fmtTime(p.CreatedAt), Flags: p.Flags,
		CanReplay: s.can(authz.WebhookReplay), AttemptCount: len(providerAttempts)}
	if cust.ID != "" {
		det.Customer = &customerDetail{ExternalRef: cust.ExternalRef, Name: cust.Name, Email: cust.Email, Phone: cust.Phone,
			LegalName: cust.Billing.LegalName, GSTIN: cust.Billing.GSTIN, Line1: cust.Billing.Line1, Line2: cust.Billing.Line2,
			City: cust.Billing.City, StateCode: cust.Billing.StateCode, PostalCode: cust.Billing.PostalCode, Country: cust.Billing.Country,
			HasBilling: cust.Billing.Line1 != "" || cust.Billing.Line2 != "" || cust.Billing.City != "" || cust.Billing.StateCode != "" ||
				cust.Billing.PostalCode != "" || cust.Billing.Country != ""}
	}
	activeRefunds := map[string]int64{}
	for _, rf := range refunds {
		if rf.Status == string(state.RefundPending) || rf.Status == string(state.RefundSucceeded) {
			activeRefunds[rf.AttemptID] += rf.Amount
		}
	}
	for _, a := range attempts {
		name := provider[a.ProviderAccountID]
		if name == "" {
			name = a.ProviderAccountID
		}
		methodSummary, methodMeta := paymentMethodSummary(a)
		det.Attempts = append(det.Attempts, attemptRow{ID: a.ID, Status: a.Status, Provider: name, ProviderRef: a.ProviderRef,
			OrderID: a.ProviderOrderID, PaymentID: a.ProviderPaymentID, Method: a.Method, FailureCode: a.FailureCode,
			FailureMessage: a.FailureMessage, Created: d.fmtTime(a.CreatedAt), Duplicate: a.Duplicate,
			MethodSummary: methodSummary, MethodMeta: methodMeta})
		remaining := max(a.AmountReceived-activeRefunds[a.ID], 0)
		minimum := int64(1)
		if _, info, ok := connector.Lookup(connectorOf[a.ProviderAccountID]); ok {
			minimum = max(info.MinRefundMinor[strings.ToUpper(a.CurrencyReceived)], minimum)
		}
		if a.Status == string(state.AttemptCaptured) && !a.Duplicate && remaining >= minimum {
			det.Pickable = append(det.Pickable, attemptOpt{ID: a.ID, Label: a.ID + " - " + fmtMoney(remaining, a.CurrencyReceived) + " refundable via " + name})
		}
		if a.Status == string(state.AttemptCaptured) && !a.Duplicate && p.Mode == "test" && connectorOf[a.ProviderAccountID] == simulator.Name {
			det.SimAttempts = append(det.SimAttempts, attemptOpt{ID: a.ID, Label: a.ID + " - " + fmtMoney(a.AmountReceived, a.CurrencyReceived)})
		}
	}
	for _, a := range providerAttempts {
		name := provider[a.ProviderAccountID]
		if name == "" {
			name = a.ProviderAccountID
		}
		failure := a.FailureCode
		if a.FailureMessage != "" {
			if failure != "" {
				failure += ": "
			}
			failure += a.FailureMessage
		}
		amount := ""
		if a.Currency != "" {
			amount = fmtMoney(a.Amount, a.Currency)
		}
		det.ProviderAttempts = append(det.ProviderAttempts, providerAttemptRow{PaymentID: a.ProviderPaymentID,
			AttemptID: a.AttemptID, Status: a.Status, Provider: name, OrderID: a.ProviderOrderID, Method: a.Method,
			Failure: failure, Amount: amount, Created: d.fmtTime(a.CreatedAt)})
	}
	det.CanRefund = s.can(authz.RefundCreate) && len(det.Pickable) > 0
	det.CanSync = s.can(authz.PaymentCancel) && d.o.PaymentSync != nil && len(attempts) > 0
	if det.CanRefund {
		det.RefundNonce = refundNonce(s, p.ID)
	}
	det.CanSimulate = s.can(authz.DisputeManage) && len(det.SimAttempts) > 0
	for _, l := range ledger {
		det.Ledger = append(det.Ledger, ledgerRow{Kind: l.Kind, Amount: fmtMoney(l.Amount, l.Currency), At: d.fmtTime(l.OccurredAt)})
	}
	for _, rf := range refunds {
		det.Refunds = append(det.Refunds, refundRow{ID: rf.ID, Amount: fmtMoney(rf.Amount, rf.Currency), Status: rf.Status, Reason: rf.Reason,
			InitiatedBy: rf.InitiatedBy, Actor: rf.Actor, Created: d.fmtTime(rf.CreatedAt)})
	}
	for _, dl := range dels {
		det.Deliveries = append(det.Deliveries, deliveryRow{ID: dl.DeliveryID, EventType: dl.EventType, Endpoint: dl.EndpointURL,
			Status: dl.Status, Attempts: dl.AttemptCount, LastStatus: dl.LastStatus, LastError: dl.LastError, At: d.fmtTime(dl.UpdatedAt)})
	}
	for _, ev := range events {
		det.Events = append(det.Events, eventRow{Type: ev.Type, Object: ev.ObjectType + " " + ev.ObjectID, Version: ev.ObjectVersion, At: d.fmtTime(ev.CreatedAt)})
	}
	v := d.shell(r, s, "payments", "Payment "+p.ID)
	v.Pay, v.Error, v.Notice = det, errMsg, notice
	d.render(w, status, "dash_payment", v)
}

// paymentSyncSubmit fetches every attempt of the payment from its provider.
// The checkout service deliberately uses each stored order/reference rather
// than a stale failed provider-payment id, allowing a successful retry on the
// same order to become authoritative.
func (d *Dashboard) paymentSyncSubmit(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	// Custom roles can be granted payment.cancel without payment.read. Never
	// render payment or customer details to a role that cannot read them.
	if !s.can(authz.PaymentRead) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	var (
		p        store.Payment
		attempts []store.Attempt
	)
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if p, err = store.GetPayment(ctx, q, id); err != nil {
			return err
		}
		attempts, err = store.ListAttempts(ctx, q, id)
		return err
	})
	if errors.Is(err, store.ErrNotFound) || err == nil && (!d.inOrg(ctx, p.OrgID) || p.Mode != d.mode(r, s)) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	if d.o.PaymentSync == nil || len(attempts) == 0 {
		d.renderPayment(w, r, s, http.StatusBadRequest, "This payment has no provider attempt to sync.", "")
		return
	}
	for _, attempt := range attempts {
		if _, err := d.o.PaymentSync.SyncAttemptLatest(ctx, attempt.ID); err != nil {
			d.o.Logger.Warn("dashboard: payment provider sync failed", "payment", p.ID, "attempt", attempt.ID, "err", err)
			d.renderPayment(w, r, s, http.StatusBadGateway, "The latest status could not be fetched from the provider. Try again shortly.", "")
			return
		}
	}
	if err := d.record(ctx, r, s, "payment.sync", p.ID, nil, map[string]any{"attempts": len(attempts)}); err != nil {
		d.fail(w, err)
		return
	}
	// Redirect to the exact pane URL so the dashboard runtime replaces its
	// cached payment detail immediately instead of waiting for a page reload.
	http.Redirect(w, r, prefix+"/payments/"+p.ID, http.StatusSeeOther)
}

// paymentDisplayStatus keeps the payment lifecycle state unchanged while
// making its financial outcome obvious in the dashboard. A succeeded charge
// remains succeeded in the API; successful refunds are separate resources.
func paymentDisplayStatus(p store.Payment) (label, class string) {
	switch {
	case p.HasFlag(core.FlagRefundedFull):
		return "refunded", "refunded"
	case p.HasFlag(core.FlagRefundedPartial):
		return "partially refunded", "partially-refunded"
	case p.Status == string(state.PaymentRequiresAction):
		return "Waiting for customer", p.Status
	case p.Status == string(state.PaymentCreated):
		return "Created", p.Status
	case p.Status == string(state.PaymentFailed):
		return "Ready to retry", p.Status
	default:
		return p.Status, p.Status
	}
}

func paymentMethodSummary(a store.Attempt) (string, string) {
	var details connector.PaymentMethodDetails
	if json.Unmarshal([]byte(a.MethodDetails), &details) != nil {
		return "", ""
	}
	if c := details.Card; c != nil {
		summary := c.Network
		if c.Last4 != "" {
			if summary != "" {
				summary += " "
			}
			summary += "•••• " + c.Last4
		}
		parts := []string{}
		for _, value := range []string{c.Funding, c.Issuer, c.Country, c.SubType} {
			if value != "" {
				parts = append(parts, value)
			}
		}
		if c.International != nil {
			if *c.International {
				parts = append(parts, "international")
			} else {
				parts = append(parts, "domestic")
			}
		}
		if c.EMI != nil && *c.EMI {
			parts = append(parts, "EMI")
		}
		return summary, strings.Join(parts, " · ")
	}
	if details.Bank != "" {
		return details.Bank, ""
	}
	if details.Wallet != "" {
		return details.Wallet, ""
	}
	if details.UPI != nil {
		return details.UPI.PayerAccountType, ""
	}
	return "", ""
}

func (d *Dashboard) refundSubmit(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	var p store.Payment
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) { p, err = store.GetPayment(ctx, q, r.PathValue("id")); return })
	if errors.Is(err, store.ErrNotFound) || err == nil && (!d.inOrg(ctx, p.OrgID) || p.Mode != d.mode(r, s)) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	nonce := r.PostForm.Get("nonce")
	if !validRefundNonce(s, p.ID, nonce) {
		d.renderPayment(w, r, s, http.StatusBadRequest, "This refund form has expired. Check the refunds below, then try again.", "")
		return
	}
	var amount int64
	if raw := strings.TrimSpace(r.PostForm.Get("amount")); raw != "" {
		amount, err = money.ParseDecimal(raw, p.Currency)
		if err != nil || amount <= 0 {
			d.renderPayment(w, r, s, http.StatusBadRequest, "Enter the refund amount as a positive number like 25.50, or leave it blank to refund everything.", "")
			return
		}
	}
	_, err = d.o.Core.CreateRefund(ctx, core.CreateRefundInput{
		Scope: core.Scope{AppID: p.AppID, Mode: p.Mode}, Actor: s.auditActor(), InitiatedBy: "dashboard",
		PaymentID: p.ID, AttemptID: r.PostForm.Get("attempt_id"), Amount: amount, Reason: strings.TrimSpace(r.PostForm.Get("reason")),
		IdempotencyKey: nonce})
	switch {
	case err == nil:
		http.Redirect(w, r, prefix+"/payments/"+p.ID, http.StatusSeeOther)
	case errors.Is(err, core.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, core.ErrConflict):
		d.renderPayment(w, r, s, http.StatusBadRequest, "This form was already used for a different refund. Check the refunds below, then try again.", "")
	case errors.Is(err, core.ErrInvalid), errors.Is(err, core.ErrNotRefundable), errors.Is(err, core.ErrRefundExceeds), errors.Is(err, core.ErrInvalidState):
		d.renderPayment(w, r, s, http.StatusBadRequest, err.Error(), "")
	default:
		d.fail(w, err)
	}
}

func (d *Dashboard) replaySubmit(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	id := r.PathValue("id")
	det, err := d.o.DB.GetDelivery(ctx, id)
	if errors.Is(err, store.ErrNotFound) || err == nil && !d.inOrg(ctx, det.Event.OrgID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	if err := d.o.Outbound.ReplayDelivery(ctx, id); err != nil {
		badRequest(w, err.Error())
		return
	}
	if err := d.record(ctx, r, s, "webhook.replay", id, nil, map[string]string{"event_id": det.Delivery.EventID, "endpoint_id": det.Delivery.EndpointID}); err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, safeNext(r.PostForm.Get("next"), prefix+"/overview"), http.StatusSeeOther)
}

// simulatorTarget finds the payment, the captured attempt and the simulator
// controller a "simulate ..." button acts on. It writes the error page itself.
func (d *Dashboard) simulatorTarget(w http.ResponseWriter, r *http.Request, s *session) (store.Payment, store.Attempt, simulator.Controller, bool) {
	ctx := r.Context()
	var (
		p        store.Payment
		attempts []store.Attempt
	)
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if p, err = store.GetPayment(ctx, q, r.PathValue("id")); err != nil {
			return err
		}
		attempts, err = store.ListAttempts(ctx, q, p.ID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) || err == nil && (!d.inOrg(ctx, p.OrgID) || p.Mode != d.mode(r, s)) {
		http.NotFound(w, r)
		return p, store.Attempt{}, nil, false
	}
	if err != nil {
		d.fail(w, err)
		return p, store.Attempt{}, nil, false
	}
	var att store.Attempt
	for _, a := range attempts {
		if a.ID == r.PostForm.Get("attempt_id") {
			att = a
		}
	}
	var ctl simulator.Controller
	if att.ID != "" && att.Status == string(state.AttemptCaptured) && p.Mode == "test" {
		if live, err := d.o.Connectors.Get(ctx, att.ProviderAccountID); err == nil {
			ctl, _ = live.Connector.(simulator.Controller)
		}
	}
	if ctl == nil {
		d.renderPayment(w, r, s, http.StatusBadRequest, "This only works on a captured test-mode payment made through the simulator.", "")
		return p, att, nil, false
	}
	return p, att, ctl, true
}

// simulateDispute opens a dispute on a test-mode payment made through the
// simulator connector. The simulator then sends its signed dispute.created
// webhook, so what appears is exactly what a real provider's dispute would
// produce.
func (d *Dashboard) simulateDispute(w http.ResponseWriter, r *http.Request, s *session) {
	p, att, ctl, ok := d.simulatorTarget(w, r, s)
	if !ok {
		return
	}
	reason := r.PostForm.Get("reason")
	switch reason {
	case "fraudulent", "product_not_received", "duplicate", "credit_not_processed":
	default:
		reason = "fraudulent"
	}
	snap, err := ctl.SimulateDispute(r.Context(), att.ProviderPaymentID, reason)
	if err != nil {
		d.renderPayment(w, r, s, http.StatusBadRequest, err.Error(), "")
		return
	}
	if err := d.record(r.Context(), r, s, "simulator.dispute", p.ID, nil, map[string]string{"dispute": snap.ProviderDisputeID, "reason": reason}); err != nil {
		d.fail(w, err)
		return
	}
	d.renderPayment(w, r, s, http.StatusOK, "", "Dispute simulated. It shows up under Disputes as soon as the simulator's webhook has been processed (a second or two).")
}

// simulateProviderRefund refunds a simulator payment "in the provider's own
// dashboard": Paymux did not ask for it, and learns of it from the webhook (or
// reconciliation), as it would for a refund made in Razorpay's dashboard.
func (d *Dashboard) simulateProviderRefund(w http.ResponseWriter, r *http.Request, s *session) {
	p, att, ctl, ok := d.simulatorTarget(w, r, s)
	if !ok {
		return
	}
	var amount int64
	if raw := strings.TrimSpace(r.PostForm.Get("amount")); raw != "" {
		var err error
		if amount, err = money.ParseDecimal(raw, p.Currency); err != nil || amount <= 0 {
			d.renderPayment(w, r, s, http.StatusBadRequest, "Enter the amount as a positive number like 25.50, or leave it blank to refund what is left.", "")
			return
		}
	}
	snap, err := ctl.SimulateProviderRefund(r.Context(), att.ProviderPaymentID, amount)
	if err != nil {
		d.renderPayment(w, r, s, http.StatusBadRequest, err.Error(), "")
		return
	}
	if err := d.record(r.Context(), r, s, "simulator.provider_refund", p.ID, nil, map[string]any{"refund": snap.ProviderRefundID, "amount": snap.Amount}); err != nil {
		d.fail(w, err)
		return
	}
	d.renderPayment(w, r, s, http.StatusOK, "", "Refund made at the simulated provider. Paymux books it as a provider refund once the webhook has been processed (a second or two).")
}

// refundNonce issues the key of one rendering of the refund form: a random
// value and its HMAC under the session's key, bound to the session and the
// payment. The form sends it back and it becomes the refund's idempotency key,
// so a resubmitted form replays the first refund.
func refundNonce(s *session, paymentID string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the system's randomness failed; nothing safe can be done
	}
	rnd := base64.RawURLEncoding.EncodeToString(b[:])
	return rnd + "." + refundNonceMAC(s, paymentID, rnd)
}

func refundNonceMAC(s *session, paymentID, rnd string) string {
	m := hmac.New(sha256.New, s.CSRFKey)
	m.Write([]byte("refund-nonce:" + s.ID + ":" + paymentID + ":" + rnd))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func validRefundNonce(s *session, paymentID, nonce string) bool {
	rnd, mac, ok := strings.Cut(nonce, ".")
	return ok && rnd != "" && hmac.Equal([]byte(refundNonceMAC(s, paymentID, rnd)), []byte(mac))
}
