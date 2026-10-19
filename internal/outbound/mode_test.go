package outbound_test

import (
	"encoding/json"
	"testing"

	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func (e *env) emitMode(app, typ, mode string) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		return e.svc.Emit(ctx, tx, events.Event{OrgID: "org_1", AppID: app, Mode: mode, Type: typ,
			ObjectType: "payment", ObjectID: "pay_1", ObjectVersion: 1, Data: map[string]any{"id": "pay_1"}})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) endpointMode(app, url, mode string) string {
	e.t.Helper()
	ep, _, err := e.svc.CreateEndpoint(ctx, outbound.CreateEndpointInput{OrgID: "org_1", AppID: app, URL: url, Mode: mode})
	if err != nil {
		e.t.Fatal(err)
	}
	if ep.Mode != mode && !(mode == "" && ep.Mode == "live") {
		e.t.Fatalf("endpoint mode = %q, want %q", ep.Mode, mode)
	}
	return ep.ID
}

// A test-mode event goes only to test-mode endpoints, a live one only to live
// ones: a test payment never reaches a production receiver.
func TestDeliveryIsFilteredByMode(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		testSink, liveSink := newSink(t, 200), newSink(t, 200)
		testEP := e.endpointMode("app_1", testSink.srv.URL, "test")
		liveEP := e.endpointMode("app_1", liveSink.srv.URL, "live")
		e.emitMode("app_1", "payment.succeeded", "test")
		e.emitMode("app_1", "payment.succeeded", "live")
		e.run()
		if n := e.count("SELECT COUNT(*) FROM webhook_deliveries WHERE endpoint_id = ?", testEP); n != 1 {
			t.Errorf("test endpoint deliveries = %d", n)
		}
		if n := e.count("SELECT COUNT(*) FROM webhook_deliveries WHERE endpoint_id = ?", liveEP); n != 1 {
			t.Errorf("live endpoint deliveries = %d", n)
		}
		check := func(s *sink, want bool) {
			t.Helper()
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.got) != 1 {
				t.Fatalf("got %d requests", len(s.got))
			}
			var b map[string]any
			_ = json.Unmarshal(s.got[0].Body, &b)
			if v, ok := b["livemode"].(bool); !ok || v != want {
				t.Errorf("livemode = %v (%v), want %v: %s", b["livemode"], ok, want, s.got[0].Body)
			}
		}
		check(testSink, false)
		check(liveSink, true)
	})
}

func TestEndpointModeDefaultsToLiveAndIsChecked(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		if id := e.endpointMode("app_1", "https://hooks.example.com/a", ""); id == "" {
			t.Fatal("no endpoint")
		}
		if _, _, err := e.svc.CreateEndpoint(ctx, outbound.CreateEndpointInput{OrgID: "org_1", AppID: "app_1", URL: "https://hooks.example.com/b", Mode: "staging"}); err == nil {
			t.Error("an unknown mode was accepted")
		}
		eps, err := e.svc.ListEndpoints(ctx, "app_1")
		if err != nil || len(eps) != 1 || eps[0].Mode != "live" {
			t.Errorf("%+v %v", eps, err)
		}
	})
}
