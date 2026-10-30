package settings_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var (
	ctx   = context.Background()
	admin = audit.Actor{Type: audit.ActorUser, ID: "usr_1"}
)

func setup(t *testing.T, db *store.DB) (*settings.Service, string) {
	t.Helper()
	err := db.Write(ctx, func(tx store.Tx) error {
		if err := store.SeedOrg(ctx, tx, "Paymux", time.Now()); err != nil {
			return err
		}
		return store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: store.DefaultOrgID, Name: "A", Slug: "a", CreatedAt: time.Now()})
	})
	if err != nil {
		t.Fatal(err)
	}
	return settings.New(db), "app_1"
}

func resolve(t *testing.T, s *settings.Service, app, key string) settings.Value {
	t.Helper()
	v, err := s.Resolve(ctx, app, key)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", key, err)
	}
	return v
}

func TestResolveFallsBackToBuiltInDefault(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, app := setup(t, db)
		v := resolve(t, s, app, "duplicate_payment_action")
		if v.Source != settings.SourceDefault || string(v.Raw) != `"review"` {
			t.Fatalf("got %+v", v)
		}
		var ttl int
		if err := resolve(t, s, app, "payment_ttl").Decode(&ttl); err != nil || ttl != 60 {
			t.Fatalf("payment_ttl=%d err=%v", ttl, err)
		}
	})
}

func TestResolveOrder(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, app := setup(t, db)
		if err := s.SeedOrg(ctx, store.DefaultOrgID); err != nil {
			t.Fatal(err)
		}
		if v := resolve(t, s, app, "payment_ttl"); v.Source != settings.SourceOrg || string(v.Raw) != "60" {
			t.Fatalf("seeded: %+v", v)
		}
		if err := s.SetOrg(ctx, admin, store.DefaultOrgID, "payment_ttl", json.RawMessage(`120`)); err != nil {
			t.Fatal(err)
		}
		if v := resolve(t, s, app, "payment_ttl"); v.Source != settings.SourceOrg || string(v.Raw) != "120" {
			t.Fatalf("org: %+v", v)
		}
		if err := s.SetApp(ctx, admin, store.DefaultOrgID, app, "payment_ttl", json.RawMessage(` 30 `)); err != nil {
			t.Fatal(err)
		}
		if v := resolve(t, s, app, "payment_ttl"); v.Source != settings.SourceApp || string(v.Raw) != "30" {
			t.Fatalf("app: %+v", v)
		}
		if err := s.ResetApp(ctx, admin, store.DefaultOrgID, app, "payment_ttl"); err != nil {
			t.Fatal(err)
		}
		if v := resolve(t, s, app, "payment_ttl"); v.Source != settings.SourceOrg || string(v.Raw) != "120" {
			t.Fatalf("after reset: %+v", v)
		}
		if v, err := s.ResolveOrg(ctx, store.DefaultOrgID, "payment_ttl"); err != nil || string(v.Raw) != "120" {
			t.Fatalf("ResolveOrg: %+v %v", v, err)
		}
	})
}

func TestSeedOrgKeepsEditedValues(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, app := setup(t, db)
		_ = s.SeedOrg(ctx, store.DefaultOrgID)
		_ = s.SetOrg(ctx, admin, store.DefaultOrgID, "recon_time", json.RawMessage(`"04:30"`))
		if err := s.SeedOrg(ctx, store.DefaultOrgID); err != nil {
			t.Fatal(err)
		}
		if v := resolve(t, s, app, "recon_time"); string(v.Raw) != `"04:30"` {
			t.Fatalf("reseed overwrote: %+v", v)
		}
	})
}

func TestSetValidation(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, app := setup(t, db)
		org := store.DefaultOrgID
		bad := []struct {
			key, val string
			appLevel bool
		}{
			{"payment_ttl", `5`, false},
			{"payment_ttl", `10081`, false},
			{"payment_ttl", `"60"`, false},
			{"payment_ttl", `60.5`, false},
			{"duplicate_payment_action", `"refund_all"`, false},
			{"duplicate_payment_action", `review`, false},
			{"pay_page_retry_buttons", `{"try_again":true}`, false},
			{"sac_code", `"99831"`, true},
			{"gst_rate_bp", `2801`, true},
			{"gst_rate_bp", `-1`, true},
			{"invoice_series_prefix", `"toolong"`, false},
			{"invoice_series_prefix", `"ab"`, false},
			{"routing", `{"rules":[{"name":"x","when":{},"action":{"kind":"bogus"}}]}`, false},
			{"routing", `{"rules":[{"name":"x","when":{},"action":{"kind":"route"}}]}`, false},
			{"routing", `{"rules":[{"name":"x","when":{"colour":["red"]},"action":{"kind":"reject"}}]}`, false},
			{"routing", `{"rules":[{"name":"x","when":{},"action":{"kind":"route","targets":[{}]}}]}`, false},
			{"routing", `{"catch_all":{"name":"c","when":{},"action":{"kind":"reject","message":"no"}}}`, false},
			{"routing", `[]`, false},
			{"dunning_schedule", `[24,24,24,24,24,24,24]`, false},
			{"dunning_schedule", `[0]`, false},
			{"dunning_schedule", `[721]`, false},
			{"predebit_lead", `24`, false},
			{"predebit_lead", `169`, false},
			{"mandate_headroom", `0.9`, false},
			{"mandate_headroom", `5.1`, false},
			{"mandate_validity", `31`, false},
			{"recon_time", `"25:00"`, false},
			{"recon_time", `"3am"`, false},
			{"webhook_retry_schedule", `[]`, false},
			{"webhook_retry_schedule", `[0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15]`, false},
			{"provider_event_retention", `29`, false},
			{"document_retention", `7`, false},
			{"upi_execution_windows", `[{"start":"13:00","end":"10:00"}]`, false},
		}
		for _, c := range bad {
			var err error
			if c.appLevel {
				err = s.SetApp(ctx, admin, store.DefaultOrgID, app, c.key, json.RawMessage(c.val))
			} else {
				err = s.SetOrg(ctx, admin, org, c.key, json.RawMessage(c.val))
			}
			if !errors.Is(err, settings.ErrInvalid) {
				t.Errorf("%s=%s: want ErrInvalid, got %v", c.key, c.val, err)
			}
		}
		good := []struct{ key, val string }{
			{"payment_ttl", `10`}, {"payment_ttl", `10080`}, {"duplicate_payment_action", `"auto_refund"`},
			{"billing_details", `"required"`}, {"invoice_delivery", `"both"`}, {"dispute_credit_note", `"on_loss"`},
			{"dunning_schedule", `[]`}, {"dunning_schedule", `[24,72,120]`}, {"dunning_final_action", `"keep_past_due"`},
			{"predebit_lead", `25`}, {"mandate_headroom", `1`}, {"mandate_headroom", `2.5`}, {"mandate_validity", `30`},
			{"recon_time", `"23:59"`}, {"provider_event_retention", `0`}, {"provider_event_retention", `30`},
			{"document_retention", `0`}, {"document_retention", `8`}, {"invoice_series_prefix", `"A1"`},
			{"routing", `{"rules":[{"name":"inr","when":{"currency":["INR"],"amount_min":100},"action":{"kind":"route","targets":[{"connector":"razorpay"},{"account_id":"pa_1"}]}}],"catch_all":{"name":"c","when":{},"action":{"kind":"reject","message":"not accepted"}}}`},
			{"routing", `{"rules":[]}`},
			{"webhook_retry_schedule", `[0]`}, {"pay_page_retry_buttons", `{"try_again":false,"pay_another_way":true}`},
		}
		for _, c := range good {
			if err := s.SetOrg(ctx, admin, org, c.key, json.RawMessage(c.val)); err != nil {
				t.Errorf("%s=%s: %v", c.key, c.val, err)
			}
		}
		// Nothing invalid reached the audit log.
		var rows []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) { rows, err = store.ListAudit(ctx, q, org, "", 500); return })
		if len(rows) != len(good) {
			t.Errorf("audit rows=%d, want %d (one per accepted change)", len(rows), len(good))
		}
	})
}

func TestSetLevelsAndUnknownKeys(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, app := setup(t, db)
		if err := s.SetApp(ctx, admin, store.DefaultOrgID, app, "recon_time", json.RawMessage(`"04:00"`)); !errors.Is(err, settings.ErrWrongLevel) {
			t.Errorf("org-only key at app level: %v", err)
		}
		if err := s.SetOrg(ctx, admin, store.DefaultOrgID, "sac_code", json.RawMessage(`"998315"`)); !errors.Is(err, settings.ErrWrongLevel) {
			t.Errorf("app-only key at org level: %v", err)
		}
		if err := s.SetOrg(ctx, admin, store.DefaultOrgID, "nope", json.RawMessage(`1`)); !errors.Is(err, settings.ErrUnknownKey) {
			t.Errorf("unknown key set: %v", err)
		}
		if _, err := s.Resolve(ctx, app, "nope"); !errors.Is(err, settings.ErrUnknownKey) {
			t.Errorf("unknown key resolve: %v", err)
		}
		if err := s.SetApp(ctx, admin, store.DefaultOrgID, "app_missing", "payment_ttl", json.RawMessage(`30`)); err == nil {
			t.Error("unknown app accepted")
		}
		if err := s.SetApp(ctx, admin, store.DefaultOrgID, app, "sac_code", json.RawMessage(`"998316"`)); err != nil {
			t.Errorf("app-only key at app level: %v", err)
		}
	})
}

func TestSetWritesAuditEntry(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, app := setup(t, db)
		_ = s.SeedOrg(ctx, store.DefaultOrgID)
		if err := s.SetApp(ctx, admin, store.DefaultOrgID, app, "payment_ttl", json.RawMessage(`45`)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Millisecond) // audit ids only order across milliseconds
		if err := s.SetApp(ctx, admin, store.DefaultOrgID, app, "payment_ttl", json.RawMessage(`50`)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Millisecond)
		if err := s.ResetApp(ctx, admin, store.DefaultOrgID, app, "payment_ttl"); err != nil {
			t.Fatal(err)
		}
		var rows []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			rows, err = store.ListAudit(ctx, q, store.DefaultOrgID, "", 10)
			return
		})
		if len(rows) != 3 {
			t.Fatalf("rows=%+v", rows)
		}
		// Newest first: reset, update 45->50, first update (before is the inherited org value 60).
		want := []struct{ action, before, after string }{
			{"setting.reset", "50", ""}, {"setting.update", "45", "50"}, {"setting.update", "60", "45"},
		}
		for i, w := range want {
			r := rows[i]
			if r.Action != w.action || r.Before != w.before || r.After != w.after || r.ActorID != "usr_1" ||
				r.Target != "app:app_1/payment_ttl" {
				t.Errorf("row %d = %+v, want %+v", i, r, w)
			}
		}
	})
}

func TestEveryDefaultIsValid(t *testing.T) {
	for _, d := range settings.Defs() {
		if err := d.Validate(json.RawMessage(d.Default)); err != nil {
			t.Errorf("default for %s fails its own validation: %v", d.Key, err)
		}
		if d.Levels == 0 {
			t.Errorf("%s has no level", d.Key)
		}
	}
	// Spec section 8a rows that this package owns.
	for _, key := range []string{"routing", "duplicate_payment_action", "billing_details", "payment_ttl",
		"pay_page_retry_buttons", "invoice_delivery", "issue_invoices", "sac_code", "gst_rate_bp", "prices_include_tax",
		"invoice_series_prefix", "credit_note_series_prefix", "dispute_credit_note", "dunning_schedule",
		"dunning_final_action", "predebit_lead", "mandate_headroom", "mandate_afa_limit", "mandate_validity",
		"upi_execution_windows", "webhook_retry_schedule", "recon_time", "provider_event_retention", "document_retention"} {
		if _, ok := settings.Lookup(key); !ok {
			t.Errorf("missing setting %s", key)
		}
	}
}

func TestAppSettingMutationsAreOrgScoped(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, app := setup(t, db)
		err := db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO orgs (id, name, created_at) VALUES ('org_other', 'Other', 1)")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetApp(ctx, admin, "org_other", app, "payment_ttl", json.RawMessage(`30`)); !errors.Is(err, settings.ErrUnknownApp) {
			t.Errorf("cross-org SetApp: %v", err)
		}
		if err := s.SetApp(ctx, admin, store.DefaultOrgID, app, "payment_ttl", json.RawMessage(`30`)); err != nil {
			t.Fatal(err)
		}
		if err := s.ResetApp(ctx, admin, "org_other", app, "payment_ttl"); !errors.Is(err, settings.ErrUnknownApp) {
			t.Errorf("cross-org ResetApp: %v", err)
		}
		if v := resolve(t, s, app, "payment_ttl"); v.Source != settings.SourceApp {
			t.Errorf("override removed by another org: %+v", v)
		}
	})
}
