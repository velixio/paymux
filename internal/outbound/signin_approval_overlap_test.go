package outbound_test

import (
	"database/sql"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func selectAll(db *store.DB, table string) (*sql.Rows, error) {
	var rows *sql.Rows
	err := db.Read(ctx, func(q store.Querier) (err error) {
		rows, err = q.QueryContext(ctx, `SELECT * FROM `+table)
		return err
	})
	return rows, err
}

// Requests started together queue ticks that run at the same time.
func TestTicksRunningTogetherSendEachNotificationOnce(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.user("usr_2")
		h.user("usr_3")
		h.begin("usr_1", "b1")
		h.begin("usr_2", "b2")
		h.begin("usr_3", "b3")
		if _, err := h.q.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		h.tick()
		if n := len(h.tg.Sent()); n != 3 {
			t.Fatalf("sent %d messages for 3 requests: %+v", n, h.tg.Sent())
		}
	})
}
