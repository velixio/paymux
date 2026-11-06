package store_test

import (
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func upsertTry(t *testing.T, db *store.DB, p store.ProviderPaymentAttempt) {
	t.Helper()
	if p.ProviderAccountID == "" {
		p.ProviderAccountID = "pa_1"
	}
	if p.AttemptID == "" {
		p.AttemptID = "att_1"
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = t0
	}
	p.UpdatedAt = t0
	if err := db.Write(ctx, func(tx store.Tx) error { return store.UpsertProviderPaymentAttempt(ctx, tx, p) }); err != nil {
		t.Fatal(err)
	}
}

func listTries(t *testing.T, db *store.DB) []store.ProviderPaymentAttempt {
	t.Helper()
	var out []store.ProviderPaymentAttempt
	if err := db.Read(ctx, func(q store.Querier) (err error) {
		out, err = store.ListProviderPaymentAttempts(ctx, q, "pay_1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUpsertTryKeepsTriesOfOnePaymentApart(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedPayment(t, db)
		upsertTry(t, db, store.ProviderPaymentAttempt{ProviderPaymentID: "pi_1", TryID: "ch_2", Status: "failed", CreatedAt: t0.Add(time.Minute)})
		upsertTry(t, db, store.ProviderPaymentAttempt{ProviderPaymentID: "pi_1", TryID: "ch_1", Status: "failed", CreatedAt: t0})
		got := listTries(t, db)
		if len(got) != 2 || got[0].TryID != "ch_1" || got[1].TryID != "ch_2" {
			t.Fatalf("tries = %+v, want ch_1 then ch_2", got)
		}
	})
}

func TestUpsertTryDefaultsTryIDToPaymentID(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedPayment(t, db)
		upsertTry(t, db, store.ProviderPaymentAttempt{ProviderPaymentID: "pay_a", Status: "failed"})
		upsertTry(t, db, store.ProviderPaymentAttempt{ProviderPaymentID: "pay_a", Status: "failed"})
		got := listTries(t, db)
		if len(got) != 1 || got[0].TryID != "pay_a" {
			t.Fatalf("tries = %+v, want one row keyed pay_a", got)
		}
	})
}

func TestUpsertTryKeepsFailureOnLaterUpdate(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedPayment(t, db)
		upsertTry(t, db, store.ProviderPaymentAttempt{ProviderPaymentID: "pay_a", Status: "failed", FailureCode: "insufficient_funds", FailureMessage: "No funds."})
		upsertTry(t, db, store.ProviderPaymentAttempt{ProviderPaymentID: "pay_a", Status: "captured"})
		got := listTries(t, db)
		if len(got) != 1 || got[0].Status != "captured" || got[0].FailureCode != "insufficient_funds" || got[0].FailureMessage != "No funds." {
			t.Fatalf("tries = %+v, want the failure kept", got)
		}
	})
}

func TestUpsertTryRoundTripsDetails(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedPayment(t, db)
		want := store.ProviderPaymentAttempt{ProviderPaymentID: "pay_a", Status: "failed",
			MethodDetails: `{"card":{"last4":"4242"}}`, RiskDetails: `{"cvv_check":"match"}`, ThreeDS: "passed"}
		upsertTry(t, db, want)
		upsertTry(t, db, store.ProviderPaymentAttempt{ProviderPaymentID: "pay_a", Status: "failed"}) // a poorer snapshot
		got := listTries(t, db)
		if len(got) != 1 || got[0].MethodDetails != want.MethodDetails || got[0].RiskDetails != want.RiskDetails || got[0].ThreeDS != "passed" {
			t.Fatalf("tries = %+v, want the details kept", got)
		}
	})
}

func TestFindAttemptByPaymentIDWithSeveralTries(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedPayment(t, db)
		upsertTry(t, db, store.ProviderPaymentAttempt{ProviderPaymentID: "pi_shared", TryID: "ch_1", Status: "failed"})
		upsertTry(t, db, store.ProviderPaymentAttempt{ProviderPaymentID: "pi_shared", TryID: "ch_2", Status: "failed"})
		if err := db.Read(ctx, func(q store.Querier) error {
			a, err := store.FindAttempt(ctx, q, "pa_1", "", "", "pi_shared")
			if err != nil {
				return err
			}
			if a.ID != "att_1" {
				t.Fatalf("attempt = %s", a.ID)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
