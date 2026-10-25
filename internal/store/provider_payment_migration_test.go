package store_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// R-A3. The migration that makes a provider payment settle one attempt refuses
// a database where two non-duplicate captured attempts of an account already
// share one, rather than dropping or rewriting either row.
func TestProviderPaymentUniquenessMigrationRefusesViolatingData(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		before := fstest.MapFS{}
		for _, dir := range []string{"sqlite", "postgres"} {
			entries, _ := fs.ReadDir(migrations.FS, dir)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "0031_") {
					continue
				}
				b, _ := fs.ReadFile(migrations.FS, dir+"/"+e.Name())
				before[dir+"/"+e.Name()] = &fstest.MapFile{Data: b}
			}
		}
		if err := db.Migrate(ctx, before); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"att_1", "att_2"} {
			if err := db.Write(ctx, func(tx store.Tx) error {
				_, err := tx.ExecContext(ctx, `INSERT INTO payment_attempts (id, org_id, mode, payment_id, provider_account_id, provider_ref,
					provider_payment_id, status, duplicate, created_at, updated_at) VALUES (?, 'o', 'test', ?, 'pa_1', ?, 'pay_x', 'captured', 0, 1, 1)`,
					id, "pay_"+id, "ref_"+id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		err := db.Migrate(ctx, migrations.FS)
		if err == nil {
			t.Fatal("the migration accepted two captured attempts of one provider payment")
		}
		var n int
		_ = db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM payment_attempts").Scan(&n)
		})
		if n != 2 {
			t.Errorf("%d attempts left after the refused migration, want 2", n)
		}
	})
}
