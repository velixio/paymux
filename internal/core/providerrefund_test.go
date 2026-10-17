package core_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/store"
)

func dashboardRefund(id string, amount int64) core.ProviderRefund {
	return core.ProviderRefund{ProviderRefundID: id, Status: state.RefundSucceeded, Amount: amount, Currency: "INR"}
}

func TestImportProviderRefundBooksARefundMadeAtTheProvider(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, a := paidPayment(t, e, 1000)
		before := e.count("SELECT COUNT(*) FROM jobs WHERE kind = 'refund.submit'")
		res, err := e.svc.ImportProviderRefund(ctx, a.ID, dashboardRefund("rfnd_dash_1", 400))
		if err != nil {
			t.Fatal(err)
		}
		r := res.Refund
		if !res.Created || r.InitiatedBy != "provider" || r.ProviderRefundID != "rfnd_dash_1" || r.Amount != 400 || r.Currency != "INR" ||
			r.Status != "succeeded" || r.AttemptID != a.ID || r.PaymentID != p.ID || r.ProviderRef == "" {
			t.Fatalf("refund %+v created=%v", r, res.Created)
		}
		got := e.get(p.ID)
		if got.AmountRefunded != 400 || !got.HasFlag("refunded_partial") || got.Version != p.Version+1 {
			t.Errorf("payment after the import: %+v", got)
		}
		var ledger []store.LedgerEntry
		_ = e.db.Read(ctx, func(q store.Querier) (err error) { ledger, err = store.ListLedgerEntries(ctx, q, p.ID); return })
		if len(ledger) != 2 || ledger[1].Kind != "refund" || ledger[1].Amount != -400 || ledger[1].RefundID != r.ID {
			t.Errorf("ledger %+v", ledger)
		}
		ev := e.emitted()
		if !slices.Contains(ev, "refund.created") || ev[len(ev)-1] != "refund.succeeded" {
			t.Errorf("events %v", ev)
		}
		// Nothing may be sent to the provider: it made this refund itself.
		if after := e.count("SELECT COUNT(*) FROM jobs WHERE kind = 'refund.submit'"); after != before {
			t.Errorf("a refund.submit job was queued for a provider refund (%d -> %d)", before, after)
		}

		// The same refund again, by webhook or by reconciliation, changes nothing.
		again, err := e.svc.ImportProviderRefund(ctx, a.ID, dashboardRefund("rfnd_dash_1", 400))
		if err != nil || again.Created || again.Refund.ID != r.ID || again.Changed {
			t.Fatalf("repeat: %+v %v", again, err)
		}
		if e.count("SELECT COUNT(*) FROM refunds") != 1 || e.get(p.ID).AmountRefunded != 400 || len(e.emitted()) != len(ev) {
			t.Error("a repeated import changed state")
		}
	})
}

func TestImportProviderRefundIsOneRowEvenUnderConcurrentDelivery(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, a := paidPayment(t, e, 1000)
		errs := make(chan error, 6)
		for i := 0; i < 6; i++ {
			go func() {
				_, err := e.svc.ImportProviderRefund(ctx, a.ID, dashboardRefund("rfnd_race", 250))
				errs <- err
			}()
		}
		for i := 0; i < 6; i++ {
			if err := <-errs; err != nil {
				t.Errorf("import: %v", err)
			}
		}
		if n := e.count("SELECT COUNT(*) FROM refunds"); n != 1 {
			t.Errorf("%d refund rows", n)
		}
		if got := e.get(p.ID); got.AmountRefunded != 250 {
			t.Errorf("refunded %d, want 250 once", got.AmountRefunded)
		}
	})
}

func TestImportProviderRefundAdoptsOurOwnPendingRefund(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, a := paidPayment(t, e, 1000)
		// We asked for a refund; the provider made it, and its webhook arrived
		// before we stored its id. That is our refund, not a second one.
		mine, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 300})
		if err != nil {
			t.Fatal(err)
		}
		res, err := e.svc.ImportProviderRefund(ctx, a.ID, dashboardRefund("rfnd_ours", 300))
		if err != nil {
			t.Fatal(err)
		}
		if res.Created || res.Refund.ID != mine.ID || res.Refund.InitiatedBy != "api" || res.Refund.ProviderRefundID != "rfnd_ours" || res.Refund.Status != "succeeded" {
			t.Errorf("adoption: %+v created=%v", res.Refund, res.Created)
		}
		if e.count("SELECT COUNT(*) FROM refunds") != 1 || e.get(p.ID).AmountRefunded != 300 {
			t.Error("our refund was counted twice")
		}
		// A different amount is somebody else's refund.
		res, err = e.svc.ImportProviderRefund(ctx, a.ID, dashboardRefund("rfnd_other", 100))
		if err != nil || !res.Created || res.Refund.InitiatedBy != "provider" {
			t.Errorf("a refund of another amount: %+v %v", res.Refund, err)
		}
	})
}

func TestImportProviderRefundFlagsOverRefunds(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, a := paidPayment(t, e, 1000)
		if _, err := e.svc.ImportProviderRefund(ctx, a.ID, dashboardRefund("rfnd_1", 600)); err != nil {
			t.Fatal(err)
		}
		if len(e.alerts.kinds()) != 0 {
			t.Fatalf("an ordinary provider refund alerted: %v", e.alerts.kinds())
		}
		// The provider says another 700 left: more than was received. The truth is recorded.
		res, err := e.svc.ImportProviderRefund(ctx, a.ID, dashboardRefund("rfnd_2", 700))
		if err != nil {
			t.Fatal(err)
		}
		got := e.get(p.ID)
		if res.Refund.Status != "succeeded" || got.AmountRefunded != 1300 || !got.HasFlag("refund_exceeds_balance") || !got.HasFlag("refunded_full") {
			t.Errorf("over-refund: refund %+v payment %+v", res.Refund, got)
		}
		if ev := e.emitted(); ev[len(ev)-1] != "payment.needs_review" && !slices.Contains(ev, "payment.needs_review") {
			t.Errorf("events %v", ev)
		}
		if !slices.Contains(e.alerts.kinds(), "refund.exceeds_balance") {
			t.Fatalf("no alert: %v", e.alerts.kinds())
		}
		if al := e.alerts.last(); al.Refs["payment"] != p.ID {
			t.Errorf("alert %+v", al)
		}
	})
}

func TestImportProviderRefundRefusesWhatItCannotBook(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(1000)
		unpaid := e.attempt(p.ID)
		if _, err := e.svc.ImportProviderRefund(ctx, unpaid.ID, dashboardRefund("rfnd_x", 100)); !errors.Is(err, core.ErrNotRefundable) {
			t.Errorf("an attempt that was never captured: %v", err)
		}
		_, a := paidPayment(t, e, 1000)
		bad := []core.ProviderRefund{
			{ProviderRefundID: "", Status: state.RefundSucceeded, Amount: 100, Currency: "INR"},
			{ProviderRefundID: "r", Status: state.RefundSucceeded, Amount: 0, Currency: "INR"},
			{ProviderRefundID: "r", Status: state.RefundSucceeded, Amount: -5, Currency: "INR"},
			{ProviderRefundID: "r", Status: "weird", Amount: 5, Currency: "INR"},
			{ProviderRefundID: "r", Status: state.RefundSucceeded, Amount: 5, Currency: "USD"},
		}
		for _, in := range bad {
			if _, err := e.svc.ImportProviderRefund(ctx, a.ID, in); !errors.Is(err, core.ErrInvalid) {
				t.Errorf("%+v: %v", in, err)
			}
		}
		if _, err := e.svc.ImportProviderRefund(ctx, "att_missing", dashboardRefund("r", 5)); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("unknown attempt: %v", err)
		}
		if e.count("SELECT COUNT(*) FROM refunds") != 0 {
			t.Error("a refused import left a refund behind")
		}
	})
}

func TestImportedPendingRefundCompletesLater(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, a := paidPayment(t, e, 1000)
		in := core.ProviderRefund{ProviderRefundID: "rfnd_slow", Status: state.RefundPending, Amount: 200, Currency: "INR"}
		res, err := e.svc.ImportProviderRefund(ctx, a.ID, in)
		if err != nil || res.Refund.Status != "pending" || e.get(p.ID).AmountRefunded != 0 {
			t.Fatalf("pending import: %+v %v", res.Refund, err)
		}
		if bal, _ := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 900}); bal.ID != "" {
			t.Error("a pending provider refund must reduce the refundable balance")
		}
		in.Status = state.RefundSucceeded
		res, err = e.svc.ImportProviderRefund(ctx, a.ID, in)
		if err != nil || res.Created || res.Refund.Status != "succeeded" || e.get(p.ID).AmountRefunded != 200 {
			t.Fatalf("completion: %+v %v", res.Refund, err)
		}
	})
}
