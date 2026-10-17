package invoice

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
)

// JobAlert is the job that raises the ops alert of a tax exception. Documents
// are issued inside the payment's transaction, where an alert must not be sent
// (it could be sent for a transaction that then rolls back); the job is
// enqueued in that transaction instead and raises the alert after it commits.
const JobAlert = "invoice.alert"

// Alert kinds of the tax exceptions. They are the exceptions the monthly
// report lists, raised as they happen.
const (
	AlertPOSDefaulted = "tax.pos_defaulted"
	AlertExportIGST   = "tax.export_igst"
	AlertFXPending    = "tax.fx_pending"
	AlertCutoffPassed = "tax.credit_note_cutoff_passed"
)

const alertPOSDefaultedMs = "the buyer gave no State: the place of supply defaulted to the supplier's State. Ask the buyer for it and reissue the invoice if it differs."

type alertPayload struct {
	Kind    string            `json:"kind"`
	Message string            `json:"message"`
	Refs    map[string]string `json:"refs"`
}

// enqueueAlert queues the alert inside tx. dedupe makes the same condition
// queue one job; the alert service rate-limits identical alerts on top.
func (s *Service) enqueueAlert(ctx context.Context, tx store.Tx, dedupe string, al alertPayload) error {
	if s.alerter == nil || s.jobs == nil {
		return nil
	}
	return s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobAlert, Payload: al, DedupeKey: JobAlert + ":" + dedupe})
}

// alertInvoiceFlags queues one alert for each tax exception flag of a new invoice.
func (s *Service) alertInvoiceFlags(ctx context.Context, tx store.Tx, inv store.Invoice) error {
	refs := map[string]string{"invoice": inv.ID, "payment": inv.PaymentID, "number": inv.Number}
	for _, f := range []struct{ flag, kind, msg string }{
		{tax.FlagPOSDefaulted, AlertPOSDefaulted, "invoice " + inv.Number + ": " + alertPOSDefaultedMs},
		{tax.FlagExportIGST, AlertExportIGST, "invoice " + inv.Number + " is an export taxed with IGST: no valid LUT covered it, or it was paid in rupees. Check the LUT dates on the tax entity."},
		{FlagFXPending, AlertFXPending, "invoice " + inv.Number + " is in " + inv.Currency + " and has no exchange rate for its date: its rupee value is blank in the GST report until a rate is entered."},
	} {
		if !inv.HasFlag(f.flag) {
			continue
		}
		if err := s.enqueueAlert(ctx, tx, f.kind+":"+inv.ID, alertPayload{Kind: f.kind, Message: f.msg, Refs: refs}); err != nil {
			return err
		}
	}
	return nil
}

// alertCutoffPassed queues the alert of a refund that could not be credit-noted.
func (s *Service) alertCutoffPassed(ctx context.Context, tx store.Tx, inv store.Invoice, refundID string) error {
	return s.enqueueAlert(ctx, tx, AlertCutoffPassed+":"+refundID, alertPayload{Kind: AlertCutoffPassed,
		Message: fmt.Sprintf("refund %s is after the credit-note cutoff for invoice %s: no credit note was issued. Handle it outside Paymux.", refundID, inv.Number),
		Refs:    map[string]string{"invoice": inv.ID, "payment": inv.PaymentID, "refund": refundID}})
}

func (s *Service) handleAlert(ctx context.Context, raw json.RawMessage) error {
	var pl alertPayload
	if err := json.Unmarshal(raw, &pl); err != nil || pl.Kind == "" {
		return jobs.Permanent(fmt.Errorf("invoice.alert: bad payload %s", raw))
	}
	if s.alerter == nil {
		return nil
	}
	return s.alerter.Raise(ctx, notify.Alert{Severity: notify.SeverityWarning, Kind: pl.Kind, Message: pl.Message, Refs: pl.Refs})
}
