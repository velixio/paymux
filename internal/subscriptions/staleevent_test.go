package subscriptions_test

import (
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
)

// providerSays sets what the provider's API answers for a mandate.
func (e *env) providerSays(providerID string, snap connector.MandateSnapshot) {
	e.r.mu.Lock()
	defer e.r.mu.Unlock()
	e.r.mandates[providerID] = snap
}

// A webhook is a hint: what it says is checked against the provider, so a stale
// or replayed event cannot put a mandate back to a state it has left.
func TestStaleMandateEventDoesNotOverrideTheProvider(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		m := e.mandate(sub.MandateID)
		// The customer revoked the mandate; the provider says so. An old
		// "active" event arrives afterwards (a delayed or replayed delivery).
		e.providerSays(m.ProviderMandateID, connector.MandateSnapshot{ProviderMandateID: m.ProviderMandateID, Status: "revoked", RawStatus: "cancelled"})
		stale := connector.MandateSnapshot{ProviderMandateID: m.ProviderMandateID, Status: "active", RawStatus: "confirmed", MaxAmount: m.MaxAmount, Currency: "INR", Method: m.Method}
		if err := e.subs.HandleNotification(ctx, m.ProviderAccountID, connector.Notification{Kind: connector.NotifyMandate, EventID: "old", Mandate: &stale}); err != nil {
			t.Fatal(err)
		}
		if got := e.mandate(m.ID).Status; got != "revoked" {
			t.Errorf("mandate is %s after a stale 'active' event; the provider says revoked", got)
		}
	})
}

// And an event claiming a revocation that the provider does not confirm changes nothing.
func TestUnconfirmedMandateRevocationChangesNothing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		m := e.mandate(sub.MandateID)
		e.providerSays(m.ProviderMandateID, connector.MandateSnapshot{ProviderMandateID: m.ProviderMandateID, Status: "active", RawStatus: "confirmed", MaxAmount: m.MaxAmount, Currency: "INR", Method: m.Method})
		claim := connector.MandateSnapshot{ProviderMandateID: m.ProviderMandateID, Status: "revoked", RawStatus: "cancelled"}
		if err := e.subs.HandleNotification(ctx, m.ProviderAccountID, connector.Notification{Kind: connector.NotifyMandate, EventID: "claim", Mandate: &claim}); err != nil {
			t.Fatal(err)
		}
		if got := e.mandate(m.ID).Status; got != "active" {
			t.Errorf("mandate is %s; the provider says it is active", got)
		}
	})
}

// When the provider cannot be asked (no such mandate yet), the event's own
// snapshot is applied as before.
func TestMandateEventIsAppliedWhenTheProviderCannotBeAsked(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		m := e.mandate(sub.MandateID)
		e.r.mu.Lock()
		delete(e.r.mandates, m.ProviderMandateID)
		e.r.mu.Unlock()
		claim := connector.MandateSnapshot{ProviderMandateID: m.ProviderMandateID, Status: "revoked", RawStatus: "cancelled"}
		if err := e.subs.HandleNotification(ctx, m.ProviderAccountID, connector.Notification{Kind: connector.NotifyMandate, EventID: "claim", Mandate: &claim}); err != nil {
			t.Fatal(err)
		}
		if got := e.mandate(m.ID).Status; got != "revoked" {
			t.Errorf("mandate is %s", got)
		}
	})
}
