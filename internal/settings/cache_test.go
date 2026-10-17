package settings_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// countingReader counts the queries made through it.
type countingReader struct {
	q store.Queryer
	n *int
}

func (c *countingReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	*c.n++
	return c.q.QueryContext(ctx, query, args...)
}

func (c *countingReader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	*c.n++
	return c.q.QueryRowContext(ctx, query, args...)
}

// writeBehind changes an org value straight in the store, the way another
// process would, so only the cache can explain a stale read.
func writeBehind(t *testing.T, db *store.DB, key, raw string) {
	t.Helper()
	err := db.Write(ctx, func(tx store.Tx) error { return store.SetOrgSetting(ctx, tx, store.DefaultOrgID, key, raw, time.Now()) })
	if err != nil {
		t.Fatal(err)
	}
}

func ttlOf(t *testing.T, s *settings.Service, app string) int {
	t.Helper()
	var n int
	if err := resolve(t, s, app, "payment_ttl").Decode(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestResolveIsCachedUntilTheTTLPasses(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, app := setup(t, db)
		now := time.Now()
		s.SetClock(func() time.Time { return now })
		s.SetCacheTTL(5 * time.Second)

		if got := ttlOf(t, s, app); got != 60 {
			t.Fatalf("default payment_ttl %d", got)
		}
		writeBehind(t, db, "payment_ttl", "90")
		if got := ttlOf(t, s, app); got != 60 {
			t.Errorf("a write behind the cache's back showed through inside the TTL: %d", got)
		}
		now = now.Add(4 * time.Second)
		if got := ttlOf(t, s, app); got != 60 {
			t.Errorf("still inside the TTL: %d", got)
		}
		now = now.Add(2 * time.Second)
		if got := ttlOf(t, s, app); got != 90 {
			t.Errorf("after the TTL the value is read again: %d", got)
		}
	})
}

func TestSettingChangesInvalidateTheCache(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, app := setup(t, db)
		s.SetCacheTTL(time.Hour)
		if got := ttlOf(t, s, app); got != 60 {
			t.Fatal(got)
		}
		// An org change is visible at once, for the org and for the app.
		if err := s.SetOrg(ctx, admin, store.DefaultOrgID, "payment_ttl", json.RawMessage(`90`)); err != nil {
			t.Fatal(err)
		}
		if got := ttlOf(t, s, app); got != 90 {
			t.Errorf("after SetOrg: %d", got)
		}
		var org int
		if v, err := s.ResolveOrg(ctx, store.DefaultOrgID, "payment_ttl"); err != nil || v.Decode(&org) != nil || org != 90 {
			t.Errorf("ResolveOrg after SetOrg: %d %v", org, err)
		}
		if err := s.SetApp(ctx, admin, store.DefaultOrgID, app, "payment_ttl", json.RawMessage(`120`)); err != nil {
			t.Fatal(err)
		}
		if got := ttlOf(t, s, app); got != 120 {
			t.Errorf("after SetApp: %d", got)
		}
		if err := s.ResetApp(ctx, admin, store.DefaultOrgID, app, "payment_ttl"); err != nil {
			t.Fatal(err)
		}
		if got := ttlOf(t, s, app); got != 90 {
			t.Errorf("after ResetApp: %d", got)
		}
	})
}

func TestCachedResolveWithUsesTheCallersReader(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, app := setup(t, db)
		s.SetCacheTTL(time.Hour)
		err := db.Write(ctx, func(tx store.Tx) error {
			v, err := s.ResolveCachedWith(ctx, tx, app, "payment_ttl")
			if err != nil {
				return err
			}
			var n int
			if err := v.Decode(&n); err != nil || n != 60 {
				t.Errorf("miss: %d %v", n, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// The loaded value is now served without touching the reader at all.
		var calls int
		var v settings.Value
		err = db.Read(ctx, func(q store.Querier) (err error) {
			v, err = s.ResolveCachedWith(ctx, &countingReader{q: q, n: &calls}, app, "payment_ttl")
			return
		})
		if err != nil || calls != 0 {
			t.Fatalf("a hit must not query: %d queries, %v", calls, err)
		}
		var n int
		if v.Decode(&n) != nil || n != 60 {
			t.Errorf("hit: %d", n)
		}
		// Uncached resolution still sees a transaction's own writes.
		err = db.Write(ctx, func(tx store.Tx) error {
			if err := store.SetOrgSetting(ctx, tx, store.DefaultOrgID, "payment_ttl", "75", time.Now()); err != nil {
				return err
			}
			v, err := settings.ResolveWith(ctx, tx, app, "payment_ttl")
			if err != nil || string(v.Raw) != "75" {
				t.Errorf("ResolveWith in the writing tx: %s %v", v.Raw, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestCacheDisabledWithZeroTTL(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, app := setup(t, db)
		s.SetCacheTTL(0)
		ttlOf(t, s, app)
		writeBehind(t, db, "payment_ttl", "90")
		if got := ttlOf(t, s, app); got != 90 {
			t.Errorf("TTL 0 must read every time: %d", got)
		}
	})
}

func TestCacheErrorsAreNotCached(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, _ := setup(t, db)
		s.SetCacheTTL(time.Hour)
		if _, err := s.Resolve(ctx, "app_missing", "payment_ttl"); err == nil {
			t.Fatal("unknown app resolved")
		}
		err := db.Write(ctx, func(tx store.Tx) error {
			return store.InsertApp(ctx, tx, store.App{ID: "app_missing", OrgID: store.DefaultOrgID, Name: "M", Slug: "m", CreatedAt: time.Now()})
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := ttlOf(t, s, "app_missing"); got != 60 {
			t.Errorf("after the app exists: %d", got)
		}
	})
}

func TestCacheIsSafeUnderConcurrentSets(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, app := setup(t, db)
		s.SetCacheTTL(time.Hour)
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					if _, err := s.Resolve(ctx, app, "payment_ttl"); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
		for i := 0; i < 10; i++ {
			if err := s.SetOrg(ctx, admin, store.DefaultOrgID, "payment_ttl", json.RawMessage(`100`)); err != nil {
				t.Fatal(err)
			}
		}
		wg.Wait()
		// After the last Set returns, every reader sees it, whatever raced.
		if got := ttlOf(t, s, app); got != 100 {
			t.Errorf("final value %d", got)
		}
	})
}
