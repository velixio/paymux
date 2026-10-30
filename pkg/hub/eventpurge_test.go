package hub

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/migrations"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// The purge job reads provider_event_retention (400 days by default) and
// removes verified and unverified provider events past it.
func TestProviderEventPurgeJobAppliesTheSetting(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		set := settings.New(db)
		if err := db.Write(ctx, func(tx store.Tx) error { return store.SeedOrg(ctx, tx, "Paymux", time.Now()) }); err != nil {
			t.Fatal(err)
		}
		if err := set.SeedOrg(ctx, store.DefaultOrgID); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := db.Write(ctx, func(tx store.Tx) error {
			for id, row := range map[string]store.ProviderEventRow{
				"old":    {ID: "pev_old", ProviderEventID: "e1", Verified: true, ReceivedAt: now.AddDate(0, 0, -500)},
				"new":    {ID: "pev_new", ProviderEventID: "e2", Verified: true, ReceivedAt: now.AddDate(0, 0, -5)},
				"forged": {ID: "pev_forged", ProviderEventID: "unverified:aa", ReceivedAt: now.AddDate(0, 0, -45)},
			} {
				_ = id
				row.ProviderAccountID, row.Headers, row.Body = "pa_1", []byte("{}"), []byte("{}")
				if _, err := db.InsertProviderEvent(ctx, tx, row); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		q := jobs.New(db, jobs.Options{Logger: slog.New(slog.DiscardHandler)})
		if err := registerProviderEventPurge(q, db, set); err != nil {
			t.Fatal(err)
		}
		if err := db.Write(ctx, func(tx store.Tx) error { return q.Enqueue(ctx, tx, jobs.Job{Kind: providerEventPurgeKind}) }); err != nil {
			t.Fatal(err)
		}
		if n, err := q.RunOnce(ctx); err != nil || n < 1 {
			t.Fatalf("ran %d: %v", n, err)
		}
		var left int
		_ = db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM provider_events").Scan(&left)
		})
		if left != 1 {
			t.Errorf("%d events left, want only the recent verified one", left)
		}
	})
}

var _ = migrations.FS
