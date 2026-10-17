package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector/money"
)

type payRow struct {
	ID, App, Status, Amount, Customer, OrderRef, Created string
}

type payFilter struct{ App, Status, Q, From, To string }

type attemptRow struct {
	ID, Status, Provider, ProviderRef, OrderID, PaymentID, Method, FailureCode, FailureMessage, Created string
	Duplicate                                                                                           bool
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
	ID, App, AppID, Mode, Status, Amount, Received, Refunded, OrderRef, Description, Customer, Created string
	Currency                                                                                           string
	Flags                                                                                              []string
	Attempts                                                                                           []attemptRow
	Ledger                                                                                             []ledgerRow
	Refunds                                                                                            []refundRow
	Deliveries                                                                                         []deliveryRow
	Events                                                                                             []eventRow
	Pickable                                                                                           []attemptOpt
	CanRefund, CanReplay                                                                               bool
}

// parseDay parses YYYY-MM-DD in the dashboard's time zone.
func (d *Dashboard) parseDay(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", s, d.o.Location)
}

func (d *Dashboard) paymentsPage(w http.ResponseWriter, r *http.Request, s *session) {
	qv := r.URL.Query()
	f := payFilter{App: qv.Get("app"), Status: qv.Get("status"), Q: strings.TrimSpace(qv.Get("q")), From: qv.Get("from"), To: qv.Get("to")}
	sf := store.PaymentFilter{AppID: f.App, Mode: d.mode(r, s), Status: f.Status, Search: f.Q}
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
	err := d.o.DB.Read(r.Context(), func(q store.Querier) (err error) {
		org, err := store.FirstOrgID(r.Context(), q)
		if err != nil {
			return err
		}
		if apps, err = store.ListApps(r.Context(), q, org); err != nil {
			return err
		}
		rows, err = store.SearchPayments(r.Context(), q, sf, after, d.o.PageSize+1)
		return err
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	names := map[string]string{}
	v := d.shell(r, s, "payments", "Payments")
	for _, a := range apps {
		names[a.ID] = a.Name
		v.AppOpts = append(v.AppOpts, appOpt{ID: a.ID, Name: a.Name})
	}
	hasMore := len(rows) > d.o.PageSize
	if hasMore {
		rows = rows[:d.o.PageSize]
	}
	for _, p := range rows {
		row := payRow{ID: p.ID, App: names[p.AppID], Status: p.Status, Amount: fmtMoney(p.Amount, p.Currency), OrderRef: p.OrderRef,
			Created: d.fmtTime(p.CreatedAt)}
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

func (d *Dashboard) paymentPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderPayment(w, r, s, http.StatusOK, "", "")
}

// renderPayment loads and renders one payment's page; a missing payment is a 404.
func (d *Dashboard) renderPayment(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg, notice string) {
	ctx := r.Context()
	id := r.PathValue("id")
	var (
		p        store.Payment
		app      store.App
		attempts []store.Attempt
		ledger   []store.LedgerEntry
		refunds  []store.Refund
		events   []store.EventRow
		dels     []store.DeliveryListRow
		accounts []store.ProviderAccountInfo
		cust     store.Customer
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
		if accounts, err = store.ListProviderAccounts(ctx, q, p.OrgID, ""); err != nil {
			return err
		}
		if p.CustomerID != "" {
			if cust, err = store.GetCustomer(ctx, q, p.CustomerID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
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

	provider := map[string]string{}
	for _, a := range accounts {
		provider[a.ID] = a.Name + " (" + a.Connector + ")"
	}
	det := &payDetail{ID: p.ID, App: app.Name, AppID: app.ID, Mode: p.Mode, Status: p.Status, Currency: p.Currency,
		Amount: fmtMoney(p.Amount, p.Currency), Received: fmtMoney(p.AmountReceived, p.Currency), Refunded: fmtMoney(p.AmountRefunded, p.Currency),
		OrderRef: p.OrderRef, Description: p.Description, Customer: cust.Email, Created: d.fmtTime(p.CreatedAt), Flags: p.Flags,
		CanReplay: s.can(authz.WebhookReplay)}
	for _, a := range attempts {
		name := provider[a.ProviderAccountID]
		if name == "" {
			name = a.ProviderAccountID
		}
		det.Attempts = append(det.Attempts, attemptRow{ID: a.ID, Status: a.Status, Provider: name, ProviderRef: a.ProviderRef,
			OrderID: a.ProviderOrderID, PaymentID: a.ProviderPaymentID, Method: a.Method, FailureCode: a.FailureCode,
			FailureMessage: a.FailureMessage, Created: d.fmtTime(a.CreatedAt), Duplicate: a.Duplicate})
		if a.Status == string(state.AttemptCaptured) && !a.Duplicate {
			det.Pickable = append(det.Pickable, attemptOpt{ID: a.ID, Label: a.ID + " - " + fmtMoney(a.AmountReceived, a.CurrencyReceived) + " via " + name})
		}
	}
	det.CanRefund = s.can(authz.RefundCreate) && len(det.Pickable) > 0
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

func (d *Dashboard) refundSubmit(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	var p store.Payment
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) { p, err = store.GetPayment(ctx, q, r.PathValue("id")); return })
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
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
		PaymentID: p.ID, AttemptID: r.PostForm.Get("attempt_id"), Amount: amount, Reason: strings.TrimSpace(r.PostForm.Get("reason"))})
	switch {
	case err == nil:
		http.Redirect(w, r, prefix+"/payments/"+p.ID, http.StatusSeeOther)
	case errors.Is(err, core.ErrNotFound):
		http.NotFound(w, r)
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
	if errors.Is(err, store.ErrNotFound) {
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
