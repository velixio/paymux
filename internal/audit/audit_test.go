package audit_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var ctx = context.Background()

func TestRecordWritesRow(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		err := db.Write(ctx, func(tx store.Tx) error {
			return audit.Record(ctx, tx, audit.Entry{
				OrgID: "org_1", Actor: audit.Actor{Type: audit.ActorAPIKey, ID: "key_1"},
				Action: "refund.create", Target: "ref_1", IP: "203.0.113.9",
				Before: nil, After: map[string]any{"amount": 100},
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		var rows []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			rows, err = store.ListAudit(ctx, q, "org_1", "", 10)
			return
		})
		if len(rows) != 1 {
			t.Fatalf("rows=%d", len(rows))
		}
		r := rows[0]
		if r.ActorType != "api_key" || r.ActorID != "key_1" || r.Action != "refund.create" || r.Target != "ref_1" ||
			r.IP != "203.0.113.9" || r.Before != "" || r.After != `{"amount":100}` || !strings.HasPrefix(r.ID, "aud_") || r.At.IsZero() {
			t.Fatalf("row %+v", r)
		}
	})
}

func TestRecordRejectsBadEntries(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		for name, e := range map[string]audit.Entry{
			"no org":    {Actor: audit.Actor{Type: audit.ActorSystem}, Action: "x"},
			"no action": {OrgID: "o", Actor: audit.Actor{Type: audit.ActorSystem}},
			"bad actor": {OrgID: "o", Actor: audit.Actor{Type: "robot"}, Action: "x"},
		} {
			err := db.Write(ctx, func(tx store.Tx) error { return audit.Record(ctx, tx, e) })
			if err == nil {
				t.Errorf("%s: expected error", name)
			}
		}
	})
}

func TestRedact(t *testing.T) {
	in := map[string]any{
		"name":     "acme",
		"password": "hunter2",
		"nested":   map[string]any{"api_key": "pmx_live_sk_x", "keep": 1, "list": []any{map[string]any{"client_secret": "s"}}},
		"Secret":   "z",
	}
	out := audit.Redact(in)
	for _, leak := range []string{"hunter2", "pmx_live_sk_x", `"s"`, `"z"`} {
		if strings.Contains(out, leak) {
			t.Errorf("redacted output leaks %q: %s", leak, out)
		}
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatal(err)
	}
	if back["name"] != "acme" || back["nested"].(map[string]any)["keep"] != float64(1) {
		t.Errorf("non-secret fields lost: %s", out)
	}
	if audit.Redact(nil) != "" {
		t.Error("nil must redact to empty")
	}
}
