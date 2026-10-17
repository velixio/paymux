package store_test

import (
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/migrations"
)

func TestScheduleUpdatedAt(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		if err := db.Migrate(ctx, migrations.FS); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := db.ScheduleUpdatedAt(ctx, "x"); err != nil || ok {
			t.Fatalf("unknown schedule: ok=%v err=%v", ok, err)
		}
		now := time.UnixMilli(1_700_000_000_000)
		err := db.Write(ctx, func(tx store.Tx) error {
			if _, _, err := db.ClaimDueSchedule(ctx, tx, "x", now, now.Add(time.Minute)); err != nil {
				return err
			}
			return db.SetScheduleNext(ctx, tx, "x", now.Add(2*time.Minute), now.Add(time.Second))
		})
		if err != nil {
			t.Fatal(err)
		}
		got, ok, err := db.ScheduleUpdatedAt(ctx, "x")
		if err != nil || !ok || !got.Equal(now.Add(time.Second)) {
			t.Fatalf("got %v ok=%v err=%v", got, ok, err)
		}
	})
}
