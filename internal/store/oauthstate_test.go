package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// An OAuth state is single use and short lived, and it remembers who started
// the authorization and for which account.
func TestOAuthStateIsSingleUseAndExpires(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		st := store.OAuthState{Hash: "h1", OrgID: "org_a", UserID: "usr_1", AccountID: "pa_1", ExpiresAt: now.Add(10 * time.Minute)}
		if err := db.Write(ctx, func(tx store.Tx) error { return store.InsertOAuthState(ctx, tx, st, now) }); err != nil {
			t.Fatal(err)
		}
		var got store.OAuthState
		err := db.Write(ctx, func(tx store.Tx) (err error) {
			got, err = store.ConsumeOAuthState(ctx, tx, "h1", now.Add(time.Minute))
			return
		})
		if err != nil || got.UserID != "usr_1" || got.AccountID != "pa_1" || got.OrgID != "org_a" {
			t.Fatalf("consume: %+v %v", got, err)
		}
		// Used once.
		err = db.Write(ctx, func(tx store.Tx) error {
			_, err := store.ConsumeOAuthState(ctx, tx, "h1", now.Add(time.Minute))
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("replay: %v", err)
		}
		// Unknown.
		err = db.Write(ctx, func(tx store.Tx) error { _, err := store.ConsumeOAuthState(ctx, tx, "nope", now); return err })
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown: %v", err)
		}
		// Expired: refused, and gone.
		st.Hash = "h2"
		_ = db.Write(ctx, func(tx store.Tx) error { return store.InsertOAuthState(ctx, tx, st, now) })
		err = db.Write(ctx, func(tx store.Tx) error {
			_, err := store.ConsumeOAuthState(ctx, tx, "h2", now.Add(11*time.Minute))
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expired: %v", err)
		}
		// Inserting purges what has expired.
		st.Hash = "h3"
		_ = db.Write(ctx, func(tx store.Tx) error { return store.InsertOAuthState(ctx, tx, st, now) })
		st.Hash = "h4"
		_ = db.Write(ctx, func(tx store.Tx) error { return store.InsertOAuthState(ctx, tx, st, now.Add(time.Hour)) })
		var n int
		_ = db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM oauth_states").Scan(&n)
		})
		if n != 1 {
			t.Errorf("%d states kept; the expired one should have been purged", n)
		}
	})
}
