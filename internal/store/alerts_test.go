package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func rec(t *testing.T, db *store.DB, a store.Alert, at time.Time) store.Alert {
	t.Helper()
	var out store.Alert
	if err := db.Write(ctx, func(tx store.Tx) (err error) { out, err = store.RecordAlert(ctx, tx, a, at); return }); err != nil {
		t.Fatal(err)
	}
	return out
}

func alert(kind, key string) store.Alert {
	return store.Alert{ID: "alt_" + kind + key, OrgID: store.DefaultOrgID, Severity: "warning", Kind: kind, Message: "msg " + kind,
		Refs: map[string]string{"provider_account": "pa_1"}, Fingerprint: "fp_" + kind + key}
}

func TestRecordAlertCountsRepeatsOfTheSameOpenCondition(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		first := rec(t, db, alert("provider.silent", "1"), t0)
		if first.Occurrences != 1 || !first.RaisedAt.Equal(t0) || !first.LastRaisedAt.Equal(t0) || first.Refs["provider_account"] != "pa_1" || first.Open() != true {
			t.Fatalf("first %+v", first)
		}
		again := alert("provider.silent", "1")
		again.ID, again.Message, again.Severity = "alt_other_id", "silent for 7h", "critical"
		second := rec(t, db, again, t0.Add(time.Hour))
		if second.ID != first.ID || second.Occurrences != 2 || !second.RaisedAt.Equal(t0) || !second.LastRaisedAt.Equal(t0.Add(time.Hour)) ||
			second.Message != "silent for 7h" || second.Severity != "critical" {
			t.Fatalf("a repeat updates the open alert: %+v", second)
		}
		other := rec(t, db, alert("recon.failed", "1"), t0.Add(2*time.Hour))
		if other.ID == first.ID {
			t.Error("a different condition is a different alert")
		}
		var n int
		_ = db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM alerts`).Scan(&n) })
		if n != 2 {
			t.Errorf("%d rows, want 2", n)
		}
		// Severity never drops: a later info raise of a critical alert keeps critical.
		low := alert("provider.silent", "1")
		low.Severity = "info"
		if got := rec(t, db, low, t0.Add(3*time.Hour)); got.Severity != "critical" {
			t.Errorf("severity fell to %s", got.Severity)
		}
	})
}

func TestAcknowledgedAlertsStayAndANewRaiseStartsANewOne(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		a := rec(t, db, alert("provider.silent", "1"), t0)
		var changed bool
		err := db.Write(ctx, func(tx store.Tx) (err error) {
			changed, err = store.AcknowledgeAlert(ctx, tx, store.DefaultOrgID, a.ID, "usr_op", t0.Add(time.Hour))
			return
		})
		if err != nil || !changed {
			t.Fatalf("ack: %v %v", changed, err)
		}
		err = db.Write(ctx, func(tx store.Tx) (err error) {
			changed, err = store.AcknowledgeAlert(ctx, tx, store.DefaultOrgID, a.ID, "usr_other", t0.Add(2*time.Hour))
			return
		})
		if err != nil || changed {
			t.Fatalf("a second acknowledgement changes nothing: %v %v", changed, err)
		}
		var got store.Alert
		_ = db.Read(ctx, func(q store.Querier) (err error) { got, err = store.GetAlert(ctx, q, store.DefaultOrgID, a.ID); return })
		if got.Open() || got.AcknowledgedBy != "usr_op" || !got.AcknowledgedAt.Equal(t0.Add(time.Hour)) {
			t.Fatalf("acknowledged %+v", got)
		}
		again := alert("provider.silent", "1")
		again.ID = "alt_second_generation"
		fresh := rec(t, db, again, t0.Add(3*time.Hour))
		if fresh.ID == a.ID || fresh.Occurrences != 1 || !fresh.Open() {
			t.Fatalf("the condition came back after acknowledgement: %+v", fresh)
		}
		err = db.Write(ctx, func(tx store.Tx) error {
			_, err := store.AcknowledgeAlert(ctx, tx, store.DefaultOrgID, "alt_missing", "usr_op", t0)
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown alert: %v", err)
		}
		err = db.Write(ctx, func(tx store.Tx) error {
			_, err := store.AcknowledgeAlert(ctx, tx, "org_other", fresh.ID, "usr_op", t0)
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("another org's alert: %v", err)
		}
	})
}

func TestListAndCountAlerts(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		a := rec(t, db, alert("a", "1"), t0)
		b := alert("b", "1")
		b.Severity = "critical"
		rec(t, db, b, t0.Add(time.Hour))
		c := rec(t, db, alert("c", "1"), t0.Add(2*time.Hour))
		_ = db.Write(ctx, func(tx store.Tx) error {
			_, err := store.AcknowledgeAlert(ctx, tx, store.DefaultOrgID, a.ID, "usr_op", t0.Add(3*time.Hour))
			return err
		})
		kinds := func(f store.AlertFilter) (out []string) {
			f.OrgID = store.DefaultOrgID
			var as []store.Alert
			_ = db.Read(ctx, func(q store.Querier) (err error) { as, err = store.ListAlerts(ctx, q, f); return })
			for _, x := range as {
				out = append(out, x.Kind)
			}
			return
		}
		eq := func(name string, got []string, want ...string) {
			t.Helper()
			if len(got) != len(want) {
				t.Errorf("%s: %v, want %v", name, got, want)
				return
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("%s: %v, want %v", name, got, want)
					return
				}
			}
		}
		eq("open, most recently raised first", kinds(store.AlertFilter{State: "open"}), "c", "b")
		eq("acknowledged", kinds(store.AlertFilter{State: "acknowledged"}), "a")
		eq("all", kinds(store.AlertFilter{State: "all"}), "c", "b", "a")
		eq("by kind", kinds(store.AlertFilter{State: "all", Kind: "b"}), "b")
		eq("by severity", kinds(store.AlertFilter{State: "all", Severity: "critical"}), "b")
		eq("limit", kinds(store.AlertFilter{State: "all", Limit: 2}), "c", "b")
		if got, _ := listFor(t, db, "org_other"); len(got) != 0 {
			t.Errorf("another org sees %d alerts", len(got))
		}
		var open, critical int
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			open, critical, err = store.CountOpenAlerts(ctx, q, store.DefaultOrgID)
			return
		})
		if open != 2 || critical != 1 {
			t.Errorf("open=%d critical=%d, want 2 and 1", open, critical)
		}
		_ = c
	})
}

func listFor(t *testing.T, db *store.DB, org string) ([]store.Alert, error) {
	var as []store.Alert
	err := db.Read(ctx, func(q store.Querier) (err error) {
		as, err = store.ListAlerts(ctx, q, store.AlertFilter{OrgID: org, State: "all"})
		return
	})
	return as, err
}
