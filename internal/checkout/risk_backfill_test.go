package checkout_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/checkout"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/fakeconn"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

type riskEnv struct {
	*env
	risk *fakeconn.State
	pa   store.ProviderAccount
}

func newRiskEnv(e *env) riskEnv {
	e.t.Helper()
	st := fakeconn.RegisterRisk("riskfake")
	pa, err := e.conns.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{Connector: st.Name, Name: "risk",
		Mode: "test", Config: json.RawMessage(fakeconn.ValidConfig), AppID: "app_1"})
	if err != nil {
		e.t.Fatal(err)
	}
	return riskEnv{env: e, risk: st, pa: pa}
}

// settled stores an attempt as it was before risk reviews existed.
func (e riskEnv) settled(id, providerPaymentID string) store.Attempt {
	e.t.Helper()
	p := e.payment2()
	a := store.Attempt{ID: id, OrgID: p.OrgID, Mode: p.Mode, PaymentID: p.ID, ProviderAccountID: e.pa.ID, ProviderRef: "ref_" + id,
		ProviderPaymentID: providerPaymentID, Status: "captured", AmountReceived: p.Amount, CurrencyReceived: p.Currency,
		CreatedAt: e.clk.Now(), UpdatedAt: e.clk.Now()}
	if err := e.db.Write(ctx, func(tx store.Tx) error { return store.InsertAttempt(ctx, tx, a) }); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e riskEnv) provider(providerPaymentID string, status connector.PaymentStatus, score int) {
	e.risk.Locked(func() {
		e.risk.Payments[providerPaymentID] = connector.PaymentSnapshot{ProviderPaymentID: providerPaymentID, Status: status,
			Amount: 1, Currency: "INR", Risk: connector.PaymentRisk{Score: &score}}
	})
}

func (e riskEnv) stored(id string) store.Attempt {
	e.t.Helper()
	var a store.Attempt
	if err := e.db.Read(ctx, func(q store.Querier) (err error) { a, err = store.GetAttempt(ctx, q, id); return err }); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e riskEnv) backfill() {
	e.t.Helper()
	if err := e.flow.BackfillRisk(ctx); err != nil {
		e.t.Fatal(err)
	}
}

func TestBackfillFillsAnExistingPaymentOnce(t *testing.T) {
	each(t, func(t *testing.T, base *env) {
		e := newRiskEnv(base)
		e.settled("att_old", "pay_old")
		e.provider("pay_old", connector.PaymentSucceeded, 19)
		e.clk.Add(checkout.RiskSettleDelay + time.Second)

		e.backfill()
		if got := e.stored("att_old").RiskDetails; got != `{"score":19}` {
			t.Fatalf("risk_details = %s", got)
		}
		fetches := len(e.risk.Fetches)
		if fetches != 1 || e.risk.Fetches[0].ProviderPaymentID != "pay_old" {
			t.Fatalf("fetches = %+v", e.risk.Fetches)
		}
		e.backfill()
		if len(e.risk.Fetches) != fetches {
			t.Fatal("a checked attempt was fetched again")
		}
	})
}

func TestBackfillNeverChangesPaymentState(t *testing.T) {
	each(t, func(t *testing.T, base *env) {
		e := newRiskEnv(base)
		a := e.settled("att_state", "pay_state")
		before := e.get(a.PaymentID)
		events := len(e.emitted())
		e.provider("pay_state", connector.PaymentFailed, 88)
		e.clk.Add(checkout.RiskSettleDelay + time.Second)

		e.backfill()
		after, stored := e.get(a.PaymentID), e.stored(a.ID)
		if stored.Status != "captured" || after.Status != before.Status || after.Version != before.Version {
			t.Fatalf("state moved: attempt %s, payment %s v%d (was %s v%d)", stored.Status, after.Status, after.Version, before.Status, before.Version)
		}
		if len(e.emitted()) != events {
			t.Fatalf("events emitted: %v", e.emitted())
		}
		if stored.RiskDetails != `{"score":88}` {
			t.Fatalf("risk_details = %s", stored.RiskDetails)
		}
	})
}

func TestBackfillWaitsForARecentPaymentToBeScored(t *testing.T) {
	each(t, func(t *testing.T, base *env) {
		e := newRiskEnv(base)
		e.settled("att_new", "pay_new")
		e.provider("pay_new", connector.PaymentSucceeded, 5)
		e.backfill()
		if len(e.risk.Fetches) != 0 {
			t.Fatal("a just-settled attempt was fetched before the score could be ready")
		}
	})
}

func TestBackfillSkipsConnectorsWithoutRiskData(t *testing.T) {
	each(t, func(t *testing.T, base *env) {
		e := newRiskEnv(base)
		other := base.account("plain")
		p := e.payment2()
		a := store.Attempt{ID: "att_plain", OrgID: p.OrgID, Mode: p.Mode, PaymentID: p.ID, ProviderAccountID: other.ID, ProviderRef: "ref_plain",
			ProviderPaymentID: "pay_plain", Status: "captured", CreatedAt: e.clk.Now(), UpdatedAt: e.clk.Now()}
		if err := e.db.Write(ctx, func(tx store.Tx) error { return store.InsertAttempt(ctx, tx, a) }); err != nil {
			t.Fatal(err)
		}
		before := e.fetches()
		e.clk.Add(checkout.RiskSettleDelay + time.Second)
		e.backfill()
		if e.fetches() != before {
			t.Fatal("fetched from a connector that reports no risk data")
		}
	})
}

func TestBackfillKeepsAttemptsForLaterWhenTheProviderIsDown(t *testing.T) {
	for name, fetchErr := range map[string]error{
		"temporary":    connector.ErrTemporary,
		"rate limited": fmt.Errorf("wrapped: %w", connector.ErrRateLimited{RetryAfter: time.Minute}),
		"credentials":  connector.ErrInvalidCredentials,
	} {
		t.Run(name, func(t *testing.T) {
			each(t, func(t *testing.T, base *env) {
				e := newRiskEnv(base)
				e.settled("att_1", "pay_1")
				e.settled("att_2", "pay_2")
				e.provider("pay_1", connector.PaymentSucceeded, 7)
				e.provider("pay_2", connector.PaymentSucceeded, 7)
				e.risk.Locked(func() { e.risk.FetchErr = fetchErr })
				e.clk.Add(checkout.RiskSettleDelay + time.Second)

				e.backfill()
				if len(e.risk.Fetches) != 1 {
					t.Fatalf("kept calling a failing provider: %d fetches", len(e.risk.Fetches))
				}
				e.risk.Locked(func() { e.risk.FetchErr = nil })
				e.backfill()
				if e.stored("att_1").RiskDetails == "{}" || e.stored("att_2").RiskDetails == "{}" {
					t.Fatal("attempts were not retried after the provider recovered")
				}
			})
		})
	}
}

func TestBackfillDoesNotRetryAPaymentTheProviderCannotReturn(t *testing.T) {
	for name, fetchErr := range map[string]error{"missing": connector.ErrNotFound, "other": errors.New("cannot decode")} {
		t.Run(name, func(t *testing.T) {
			each(t, func(t *testing.T, base *env) {
				e := newRiskEnv(base)
				e.settled("att_gone", "pay_gone")
				e.risk.Locked(func() { e.risk.FetchErr = fetchErr })
				e.clk.Add(checkout.RiskSettleDelay + time.Second)
				e.backfill()
				e.backfill()
				if len(e.risk.Fetches) != 1 {
					t.Fatalf("fetches = %d", len(e.risk.Fetches))
				}
			})
		})
	}
}

func TestBackfillIgnoresAnotherPaymentsData(t *testing.T) {
	each(t, func(t *testing.T, base *env) {
		e := newRiskEnv(base)
		e.settled("att_mine", "pay_mine")
		score := 99
		e.risk.Locked(func() {
			e.risk.FetchFn = func(connector.PaymentRef) (connector.PaymentSnapshot, error) {
				return connector.PaymentSnapshot{ProviderPaymentID: "pay_other", Status: connector.PaymentSucceeded,
					Risk: connector.PaymentRisk{Score: &score}}, nil
			}
		})
		e.clk.Add(checkout.RiskSettleDelay + time.Second)
		e.backfill()
		if got := e.stored("att_mine").RiskDetails; got != "{}" {
			t.Fatalf("stored another payment's risk: %s", got)
		}
	})
}

func TestBackfillFetchesABoundedBatch(t *testing.T) {
	each(t, func(t *testing.T, base *env) {
		e := newRiskEnv(base)
		for i := range checkout.RiskBatch + 5 {
			id := fmt.Sprintf("pay_%d", i)
			e.settled(fmt.Sprintf("att_%d", i), id)
			e.provider(id, connector.PaymentSucceeded, 1)
		}
		e.clk.Add(checkout.RiskSettleDelay + time.Second)
		e.backfill()
		if len(e.risk.Fetches) != checkout.RiskBatch {
			t.Fatalf("fetches = %d, want %d", len(e.risk.Fetches), checkout.RiskBatch)
		}
		e.backfill()
		if len(e.risk.Fetches) != checkout.RiskBatch+5 {
			t.Fatalf("fetches after the second run = %d", len(e.risk.Fetches))
		}
	})
}

func TestBackfillRunsOnItsSchedule(t *testing.T) {
	each(t, func(t *testing.T, base *env) {
		e := newRiskEnv(base)
		if err := e.flow.Register(e.q); err != nil {
			t.Fatal(err)
		}
		e.settled("att_sched", "pay_sched")
		e.provider("pay_sched", connector.PaymentSucceeded, 3)
		// A new schedule first runs at its next slot, so the queue has to see it once.
		if _, err := e.q.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		e.clk.Add(checkout.RiskSettleDelay + time.Minute)
		for range 5 {
			if _, err := e.q.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.stored("att_sched").RiskDetails; got != `{"score":3}` {
			t.Fatalf("risk_details = %s", got)
		}
	})
}

// pending stores an attempt waiting on the provider, known only by its order.
func (e riskEnv) pending(id, order string) store.Attempt {
	e.t.Helper()
	p := e.payment2()
	a := store.Attempt{ID: id, OrgID: p.OrgID, Mode: p.Mode, PaymentID: p.ID, ProviderAccountID: e.pa.ID, ProviderRef: "ref_" + id,
		ProviderOrderID: order, Status: "pending", CreatedAt: e.clk.Now(), UpdatedAt: e.clk.Now()}
	if err := e.db.Write(ctx, func(tx store.Tx) error { return store.InsertAttempt(ctx, tx, a) }); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func TestAReceivedPaymentGetsItsRiskAtOnce(t *testing.T) {
	each(t, func(t *testing.T, base *env) {
		e := newRiskEnv(base)
		a := e.pending("att_now", "ord_now")
		p := e.get(a.PaymentID)
		score := 19
		e.risk.Locked(func() {
			// An order lookup carries no review; only the payment itself does.
			e.risk.FetchFn = func(ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
				snap := connector.PaymentSnapshot{ProviderRef: a.ProviderRef, ProviderOrderID: "ord_now", ProviderPaymentID: "pay_now",
					Status: connector.PaymentSucceeded, Amount: p.Amount, Currency: p.Currency}
				if ref.ProviderPaymentID == "pay_now" {
					snap.Risk = connector.PaymentRisk{Score: &score}
				}
				return snap, nil
			}
		})
		if _, err := e.flow.SyncAttempt(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		if got := e.stored(a.ID).RiskDetails; got != `{"score":19}` {
			t.Fatalf("risk_details right after receipt = %s", got)
		}
		fetches := len(e.risk.Fetches)
		e.clk.Add(checkout.RiskSettleDelay + time.Second)
		e.backfill()
		if len(e.risk.Fetches) != fetches {
			t.Fatal("a payment already scored at receipt was fetched again")
		}
	})
}

func TestAPaymentNotYetScoredIsAskedAgainLater(t *testing.T) {
	each(t, func(t *testing.T, base *env) {
		e := newRiskEnv(base)
		a := e.pending("att_late", "ord_late")
		p := e.get(a.PaymentID)
		var score *int
		e.risk.Locked(func() {
			e.risk.FetchFn = func(connector.PaymentRef) (connector.PaymentSnapshot, error) {
				return connector.PaymentSnapshot{ProviderRef: a.ProviderRef, ProviderOrderID: "ord_late", ProviderPaymentID: "pay_late",
					Status: connector.PaymentSucceeded, Amount: p.Amount, Currency: p.Currency, Risk: connector.PaymentRisk{Score: score}}, nil
			}
		})
		if _, err := e.flow.SyncAttempt(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		if got := e.stored(a.ID).RiskDetails; got != "{}" {
			t.Fatalf("risk_details = %s", got)
		}
		late := 42
		e.risk.Locked(func() { score = &late })
		e.clk.Add(checkout.RiskSettleDelay + time.Second)
		e.backfill()
		if got := e.stored(a.ID).RiskDetails; got != `{"score":42}` {
			t.Fatalf("risk_details after the later check = %s", got)
		}
	})
}

func TestARiskFetchFailureDoesNotFailTheReceivedPayment(t *testing.T) {
	each(t, func(t *testing.T, base *env) {
		e := newRiskEnv(base)
		a := e.pending("att_err", "ord_err")
		p := e.get(a.PaymentID)
		e.risk.Locked(func() {
			e.risk.FetchFn = func(ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
				if ref.ProviderPaymentID != "" {
					return connector.PaymentSnapshot{}, connector.ErrTemporary
				}
				return connector.PaymentSnapshot{ProviderRef: a.ProviderRef, ProviderOrderID: "ord_err", ProviderPaymentID: "pay_err",
					Status: connector.PaymentSucceeded, Amount: p.Amount, Currency: p.Currency}, nil
			}
		})
		if _, err := e.flow.SyncAttempt(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		if got := e.stored(a.ID).Status; got != "captured" {
			t.Fatalf("status = %s", got)
		}
	})
}
