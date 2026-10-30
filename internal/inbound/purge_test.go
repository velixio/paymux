package inbound_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/inbound"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var purgeNow = time.UnixMilli(1_800_000_000_000).UTC()

func days(n int) time.Time { return purgeNow.AddDate(0, 0, -n) }

func seedEvents(t *testing.T, db *store.DB) {
	t.Helper()
	rows := []store.ProviderEventRow{
		{ID: "pev_v500", ProviderEventID: "ev500", Verified: true, ReceivedAt: days(500)},
		{ID: "pev_v100", ProviderEventID: "ev100", Verified: true, ReceivedAt: days(100)},
		{ID: "pev_v10", ProviderEventID: "ev10", Verified: true, ReceivedAt: days(10)},
		{ID: "pev_u100", ProviderEventID: "unsigned:ev100", Verified: false, ReceivedAt: days(100)},
		{ID: "pev_f40", ProviderEventID: "unverified:aa", Verified: false, ReceivedAt: days(40)},
		{ID: "pev_f3", ProviderEventID: "unverified:bb", Verified: false, ReceivedAt: days(3)},
	}
	if err := db.Write(ctx, func(tx store.Tx) error {
		for _, r := range rows {
			r.ProviderAccountID, r.Headers, r.Body = "pa_1", []byte("{}"), []byte("{}")
			if _, err := db.InsertProviderEvent(ctx, tx, r); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func left(t *testing.T, e *env) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	_ = e.db.Read(ctx, func(q store.Querier) error {
		rows, err := q.QueryContext(ctx, "SELECT id FROM provider_events")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			got[id] = true
		}
		return nil
	})
	return got
}

// provider_event_retention applies to every stored provider event, verified or
// not; forged events (kept only as evidence) go after 30 days at the latest.
func TestProviderEventsArePurgedByRetention(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention int
		want      []string
	}{
		{"400 days", 400, []string{"pev_v100", "pev_v10", "pev_u100", "pev_f3"}},
		{"30 days", 30, []string{"pev_v10", "pev_f3"}},
		{"forever", 0, []string{"pev_v500", "pev_v100", "pev_v10", "pev_u100", "pev_f3"}}, // forged evidence still goes after 30 days
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
				e := newEnv(t, db)
				seedEvents(t, db)
				p := inbound.NewPurger(inbound.PurgeOptions{DB: db, Batch: 2, Now: func() time.Time { return purgeNow },
					Retention: func(_ context.Context) (int, error) { return tc.retention, nil }})
				if _, err := p.Run(ctx); err != nil {
					t.Fatal(err)
				}
				got := left(t, e)
				if len(got) != len(tc.want) {
					t.Fatalf("left %v, want %v", got, tc.want)
				}
				for _, id := range tc.want {
					if !got[id] {
						t.Errorf("%s was purged: left %v", id, fmt.Sprint(got))
					}
				}
			})
		})
	}
}
