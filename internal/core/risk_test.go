package core_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

func storedRisk(t *testing.T, e *env, attemptID string) connector.PaymentRisk {
	t.Helper()
	var a store.Attempt
	err := e.db.Read(ctx, func(q store.Querier) (err error) {
		a, err = store.GetAttempt(ctx, q, attemptID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var r connector.PaymentRisk
	if err := json.Unmarshal([]byte(a.RiskDetails), &r); err != nil {
		t.Fatalf("risk_details %q: %v", a.RiskDetails, err)
	}
	return r
}

func TestLateRiskScoreIsSavedOnASettledAttempt(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(100)
		a := e.attempt(p.ID)
		captured := core.Snapshot{Status: state.AttemptCaptured, Amount: 100, Currency: "INR", ProviderPaymentID: "pay_1",
			Risk: connector.PaymentRisk{IPAddress: "203.0.113.24", CVVCheck: "match"}}
		e.apply(a.ID, captured)

		score := 19
		captured.Risk = connector.PaymentRisk{Score: &score}
		if !e.apply(a.ID, captured).Changed {
			t.Fatal("a late risk score was not saved")
		}
		r := storedRisk(t, e, a.ID)
		if r.Score == nil || *r.Score != 19 {
			t.Fatalf("score = %v", r.Score)
		}
		if r.IPAddress != "203.0.113.24" || r.CVVCheck != "match" {
			t.Fatalf("a partial update lost earlier facts: %+v", r)
		}
		if e.apply(a.ID, captured).Changed {
			t.Fatal("a repeat with the same risk data reported a change")
		}
	})
}

func TestRiskDataIsSanitized(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(100)
		a := e.attempt(p.ID)
		score := 150
		e.apply(a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 100, Currency: "INR", ProviderPaymentID: "pay_1",
			Risk: connector.PaymentRisk{Score: &score, IPAddress: "not-an-ip", City: "Testville\n", Country: "usa",
				AuthorizationCode: strings.Repeat("x", 200), CVVCheck: "match"}})
		r := storedRisk(t, e, a.ID)
		if r.Score != nil || r.IPAddress != "" || r.Country != "" || r.AuthorizationCode != "" {
			t.Fatalf("unsafe values kept: %+v", r)
		}
		if r.City != "Testville" || r.CVVCheck != "match" {
			t.Fatalf("safe values lost: %+v", r)
		}
	})
}

func TestRiskDataStaysOutOfTheAttemptAPI(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(100)
		a := e.attempt(p.ID)
		e.apply(a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 100, Currency: "INR", ProviderPaymentID: "pay_1",
			Risk: connector.PaymentRisk{IPAddress: "203.0.113.24"}})
		views, err := e.svc.PaymentAttempts(ctx, e.scope, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(views)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "203.0.113.24") {
			t.Fatalf("the API exposes the buyer IP: %s", raw)
		}
	})
}

func riskQueue(t *testing.T, e *env, accountID string) []store.RiskCheckItem {
	t.Helper()
	var items []store.RiskCheckItem
	err := e.db.Read(ctx, func(q store.Querier) (err error) {
		items, err = store.ListAttemptsToRiskCheck(ctx, q, accountID, time.Now().Add(100*365*24*time.Hour), 10)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestAScoreReceivedWithThePaymentNeedsNoLaterFetch(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(100)
		a := e.attempt(p.ID)
		score := 19
		snap := captured(100)
		snap.Risk = connector.PaymentRisk{Score: &score}
		e.apply(a.ID, snap)
		if items := riskQueue(t, e, a.ProviderAccountID); len(items) != 0 {
			t.Fatalf("a scored attempt is still queued: %+v", items)
		}
	})
}

func TestAPaymentReceivedWithoutAScoreStaysQueued(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(100)
		a := e.attempt(p.ID)
		snap := captured(100)
		snap.Risk = connector.PaymentRisk{CVVCheck: "match"}
		e.apply(a.ID, snap)
		if items := riskQueue(t, e, a.ProviderAccountID); len(items) != 1 {
			t.Fatalf("queue = %+v", items)
		}

		score := 19
		snap.Risk = connector.PaymentRisk{Score: &score}
		e.apply(a.ID, snap)
		if items := riskQueue(t, e, a.ProviderAccountID); len(items) != 0 {
			t.Fatalf("a score arriving on a repeat left it queued: %+v", items)
		}
	})
}
