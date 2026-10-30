package alertcenter_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/alertcenter"
	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var ctx = context.Background()

type mailer struct {
	mu sync.Mutex
	n  int
}

func (m *mailer) Send(context.Context, notify.Message) error {
	m.mu.Lock()
	m.n++
	m.mu.Unlock()
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func setup(t *testing.T, db *store.DB) (*alertcenter.Service, *clock) {
	t.Helper()
	if err := db.Write(ctx, func(tx store.Tx) error { return store.SeedOrg(ctx, tx, "Paymux", time.Now()) }); err != nil {
		t.Fatal(err)
	}
	clk := &clock{time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	return alertcenter.New(db, clk.now), clk
}

func TestRaisesAreRecordedAndRepeatsCounted(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		svc, clk := setup(t, db)
		m := &mailer{}
		raiser := notify.NewAlerts(notify.AlertsOptions{Mailer: m, Emails: []string{"ops@example.com"}, Now: clk.now, Recorder: svc})
		al := notify.Alert{Severity: notify.SeverityCritical, Kind: "provider.silent", Message: "No webhooks from pa_1 in 6h", Refs: map[string]string{"provider_account": "pa_1"}}
		for i := 0; i < 3; i++ {
			clk.t = clk.t.Add(time.Minute)
			if err := raiser.Raise(ctx, al); err != nil {
				t.Fatal(err)
			}
		}
		raiser.Raise(ctx, notify.Alert{Kind: "recon.failed", Message: "Reconciliation failed", Refs: map[string]string{"provider_account": "pa_2"}})

		open, err := svc.List(ctx, store.DefaultOrgID, alertcenter.Filter{})
		if err != nil || len(open) != 2 {
			t.Fatalf("open alerts: %d %v", len(open), err)
		}
		var silent store.Alert
		for _, a := range open {
			if a.Kind == "provider.silent" {
				silent = a
			}
		}
		if silent.Occurrences != 3 || silent.Severity != "critical" || silent.Message != al.Message || silent.Refs["provider_account"] != "pa_1" || !strings.HasPrefix(silent.ID, "alt_") {
			t.Fatalf("alert %+v", silent)
		}
		if m.n != 2 { // one per condition: the 10-minute rate limit of delivery is unchanged
			t.Errorf("%d emails, want 2", m.n)
		}
		o, c, _ := svc.Counts(ctx, store.DefaultOrgID)
		if o != 2 || c != 1 {
			t.Errorf("counts %d/%d", o, c)
		}
	})
}

func TestAcknowledgeIsAuditedOnceAndScopedToTheOrg(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		svc, clk := setup(t, db)
		if err := svc.Record(ctx, notify.Alert{Severity: notify.SeverityWarning, Kind: "domain.failed", Message: "pay.shop.example.org failed", Refs: map[string]string{"domain": "d1"}}, clk.t); err != nil {
			t.Fatal(err)
		}
		open, _ := svc.List(ctx, store.DefaultOrgID, alertcenter.Filter{})
		id := open[0].ID
		actor := audit.Actor{Type: audit.ActorUser, ID: "usr_op"}
		got, err := svc.Acknowledge(ctx, actor, store.DefaultOrgID, id)
		if err != nil || got.Open() || got.AcknowledgedBy != "usr_op" {
			t.Fatalf("ack: %+v %v", got, err)
		}
		if _, err := svc.Acknowledge(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_other"}, store.DefaultOrgID, id); err != nil {
			t.Fatalf("acknowledging twice is harmless: %v", err)
		}
		again, _ := svc.List(ctx, store.DefaultOrgID, alertcenter.Filter{State: "all"})
		if again[0].AcknowledgedBy != "usr_op" {
			t.Errorf("the first acknowledgement stands: %+v", again[0])
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "alert.acknowledge", 10)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "usr_op" || aud[0].Target != id {
			t.Errorf("audit %+v: one entry, for the real change", aud)
		}
		if _, err := svc.Acknowledge(ctx, actor, store.DefaultOrgID, "alt_missing"); !errors.Is(err, alertcenter.ErrNotFound) {
			t.Errorf("unknown alert: %v", err)
		}
		if _, err := svc.Acknowledge(ctx, actor, "org_other", id); !errors.Is(err, alertcenter.ErrNotFound) {
			t.Errorf("another org's alert: %v", err)
		}
		// The same condition after acknowledgement is a new open alert.
		_ = svc.Record(ctx, notify.Alert{Severity: notify.SeverityWarning, Kind: "domain.failed", Message: "again", Refs: map[string]string{"domain": "d1"}}, clk.t.Add(time.Hour))
		if open, _ := svc.List(ctx, store.DefaultOrgID, alertcenter.Filter{}); len(open) != 1 || open[0].ID == id {
			t.Errorf("open after a new raise: %+v", open)
		}
	})
}

func TestFilterStatesAndValidation(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		svc, clk := setup(t, db)
		_ = svc.Record(ctx, notify.Alert{Kind: "a", Message: "m"}, clk.t)
		for _, st := range []string{"", "open", "acknowledged", "all"} {
			if _, err := svc.List(ctx, store.DefaultOrgID, alertcenter.Filter{State: st}); err != nil {
				t.Errorf("state %q: %v", st, err)
			}
		}
		if _, err := svc.List(ctx, store.DefaultOrgID, alertcenter.Filter{State: "bogus"}); !errors.Is(err, alertcenter.ErrInvalid) {
			t.Errorf("bogus state: %v", err)
		}
		if _, err := svc.List(ctx, store.DefaultOrgID, alertcenter.Filter{Severity: "loud"}); !errors.Is(err, alertcenter.ErrInvalid) {
			t.Errorf("bogus severity: %v", err)
		}
		kinds, err := svc.Kinds(ctx, store.DefaultOrgID)
		if err != nil || len(kinds) != 1 || kinds[0] != "a" {
			t.Errorf("kinds %v %v", kinds, err)
		}
	})
}

func TestARecordingFailureIsAnErrorForTheRecorderButNotForRaise(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		svc, clk := setup(t, db)
		if err := svc.Record(ctx, notify.Alert{Kind: "x", Severity: "nonsense"}, clk.t); err == nil {
			t.Error("a bad severity should be refused")
		}
		m := &mailer{}
		raiser := notify.NewAlerts(notify.AlertsOptions{Mailer: m, Emails: []string{"ops@example.com"}, Recorder: svc})
		if err := raiser.Raise(ctx, notify.Alert{Kind: "x", Severity: "nonsense", Message: "m"}); err != nil {
			t.Errorf("Raise must not fail because the alert could not be recorded: %v", err)
		}
		if m.n != 1 {
			t.Error("and it must still be delivered")
		}
	})
}
