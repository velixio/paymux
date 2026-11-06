package outbound_test

import (
	"errors"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/outbound"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// waiting leaves one delivery that failed its first attempt and waits an hour.
func waiting(t *testing.T, db *store.DB) (*env, *sink, string) {
	t.Helper()
	e := newEnv(t, db, true)
	e.svc.RetrySchedule = func() []time.Duration { return []time.Duration{0, time.Hour, time.Hour} }
	s := newSink(t, 500)
	e.endpoint("app_1", s.srv.URL)
	e.emit("app_1", "x.y")
	e.run()
	if len(s.hits()) != 1 {
		t.Fatalf("hits = %d", len(s.hits()))
	}
	return e, s, e.str(`SELECT id FROM webhook_deliveries`)
}

func TestResendEventSendsWaitingDeliveryNow(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e, s, _ := waiting(t, db)
		s.setCode(200)
		res, err := e.svc.ResendEvent(ctx, e.str(`SELECT id FROM events`))
		if err != nil {
			t.Fatal(err)
		}
		if res.Sent != 1 || res.Skipped != 0 {
			t.Fatalf("result = %+v", res)
		}
		e.run()
		if len(s.hits()) != 2 {
			t.Fatalf("hits = %d, want the resend to go now", len(s.hits()))
		}
		e.clk.Add(2 * time.Hour)
		e.run()
		if len(s.hits()) != 2 {
			t.Fatalf("hits = %d, the replaced retry still fired", len(s.hits()))
		}
	})
}

func TestReplayWaitingDeliveryNow(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e, s, id := waiting(t, db)
		s.setCode(200)
		if err := e.svc.ReplayDelivery(ctx, id); err != nil {
			t.Fatal(err)
		}
		e.run()
		e.clk.Add(2 * time.Hour)
		e.run()
		if len(s.hits()) != 2 {
			t.Fatalf("hits = %d", len(s.hits()))
		}
		if st := e.str(`SELECT status FROM webhook_deliveries`); st != "delivered" {
			t.Fatalf("status %s", st)
		}
	})
}

func TestDoubleRetryOneSend(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e, s, id := waiting(t, db)
		s.setCode(200)
		for range 2 {
			if err := e.svc.ReplayDelivery(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		e.run()
		e.clk.Add(2 * time.Hour)
		e.run()
		if len(s.hits()) != 2 {
			t.Fatalf("hits = %d, want the first attempt plus one send", len(s.hits()))
		}
	})
}

func TestRetryAfterFailureStillSchedules(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e, s, id := waiting(t, db)
		if err := e.svc.ReplayDelivery(ctx, id); err != nil {
			t.Fatal(err)
		}
		e.run() // fails again, with a retry scheduled
		if len(s.hits()) != 2 {
			t.Fatalf("hits = %d", len(s.hits()))
		}
		s.setCode(200)
		e.clk.Add(2 * time.Hour)
		e.run()
		if len(s.hits()) != 3 {
			t.Fatalf("hits = %d, the next retry did not run", len(s.hits()))
		}
		if st := e.str(`SELECT status FROM webhook_deliveries`); st != "delivered" {
			t.Fatalf("status %s", st)
		}
	})
}

func TestReplayInFlightRefused(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e, _, id := waiting(t, db)
		e.clk.Add(2 * time.Hour)
		if _, err := db.ClaimJobs(ctx, e.clk.Now(), time.Minute, 10); err != nil { // a worker holds the retry
			t.Fatal(err)
		}
		if err := e.svc.ReplayDelivery(ctx, id); !errors.Is(err, outbound.ErrDeliveryInFlight) {
			t.Fatalf("err = %v", err)
		}
		res, err := e.svc.ResendEvent(ctx, e.str(`SELECT id FROM events`))
		if err != nil {
			t.Fatal(err)
		}
		if res.Sent != 0 || res.Skipped != 1 {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestResendTargetsMatchResend(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		e.svc.RetrySchedule = func() []time.Duration { return []time.Duration{0, time.Hour} }
		ok, bad := newSink(t, 200), newSink(t, 500)
		okID, _ := e.endpoint("app_1", ok.srv.URL)
		badID, _ := e.endpoint("app_1", bad.srv.URL)
		e.emit("app_1", "x.y")
		e.run()
		eventID := e.str(`SELECT id FROM events`)
		newID, _ := e.endpoint("app_1", newSink(t, 200).srv.URL) // added after the event
		e.endpoint("app_1", newSink(t, 200).srv.URL, "other.type")
		disabled, _ := e.endpoint("app_1", newSink(t, 200).srv.URL)
		otherMode, _ := e.endpoint("app_1", newSink(t, 200).srv.URL)
		if err := db.Write(ctx, func(tx store.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE webhook_endpoints SET enabled = 0 WHERE id = ?`, disabled); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE webhook_endpoints SET mode = 'other' WHERE id = ?`, otherMode)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		targets, err := e.svc.ResendTargets(ctx, eventID)
		if err != nil {
			t.Fatal(err)
		}
		byID := map[string]outbound.ResendTarget{}
		for _, target := range targets {
			byID[target.EndpointID] = target
		}
		if len(targets) != 3 {
			t.Fatalf("targets = %+v", targets)
		}
		if got := byID[okID]; got.Status != "delivered" || got.LastStatus != 200 || got.New {
			t.Errorf("delivered = %+v", got)
		}
		if got := byID[badID]; got.Status != "pending" || got.LastStatus != 500 || got.NextAttemptAt.IsZero() || got.InFlight || got.DeliveryID == "" {
			t.Errorf("waiting = %+v", got)
		}
		if got := byID[newID]; !got.New || got.DeliveryID != "" || got.URL == "" {
			t.Errorf("new = %+v", got)
		}
		res, err := e.svc.ResendEvent(ctx, eventID)
		if err != nil {
			t.Fatal(err)
		}
		if res.Sent != len(targets) {
			t.Errorf("sent %d, targets %d", res.Sent, len(targets))
		}
	})
}

func TestResendTargetsMarksInFlight(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e, _, _ := waiting(t, db)
		e.clk.Add(2 * time.Hour)
		if _, err := db.ClaimJobs(ctx, e.clk.Now(), time.Minute, 10); err != nil {
			t.Fatal(err)
		}
		targets, err := e.svc.ResendTargets(ctx, e.str(`SELECT id FROM events`))
		if err != nil {
			t.Fatal(err)
		}
		if len(targets) != 1 || !targets[0].InFlight {
			t.Fatalf("targets = %+v", targets)
		}
	})
}
