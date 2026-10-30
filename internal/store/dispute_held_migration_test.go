package store_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/velixio/paymux/internal/migrations"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// A-13. Disputes that exist before amount_held booked their full amount, so
// they are backfilled with it and their release matches their hold.
func TestAmountHeldIsBackfilledWithTheDisputeAmount(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		before := fstest.MapFS{}
		for _, dir := range []string{"sqlite", "postgres"} {
			entries, _ := fs.ReadDir(migrations.FS, dir)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "0032_") {
					continue
				}
				b, _ := fs.ReadFile(migrations.FS, dir+"/"+e.Name())
				before[dir+"/"+e.Name()] = &fstest.MapFile{Data: b}
			}
		}
		if err := db.Migrate(ctx, before); err != nil {
			t.Fatal(err)
		}
		if err := db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO disputes (id, org_id, app_id, mode, payment_id, attempt_id, provider_account_id,
				provider_dispute_id, amount, currency, status, source, created_at, updated_at)
				VALUES ('dsp_1', 'o', 'a', 'test', 'p', 'att', 'pa', 'pd', 777, 'INR', 'needs_response', 'webhook', 1, 1)`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(ctx, migrations.FS); err != nil {
			t.Fatal(err)
		}
		var held int64
		if err := db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT amount_held FROM disputes WHERE id = 'dsp_1'").Scan(&held)
		}); err != nil || held != 777 {
			t.Errorf("amount_held = %d, %v; want 777", held, err)
		}
	})
}
