package store_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func migrated(t *testing.T, db *store.DB) {
	t.Helper()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookEndpointCRUD(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		migrated(t, db)
		now := time.UnixMilli(1_700_000_000_000).UTC()
		ep := store.EndpointRow{ID: "whe_1", OrgID: "org_1", AppID: "app_1", URL: "https://example.com/h",
			SigningSecretEnc: []byte{1, 2, 3}, EventTypes: []string{"payment.succeeded"}, Enabled: true, CreatedAt: now, UpdatedAt: now}
		off := ep
		off.ID, off.Enabled, off.EventTypes = "whe_2", false, nil
		other := ep
		other.ID, other.AppID = "whe_3", "app_2"
		err := db.Write(ctx, func(tx store.Tx) error {
			for _, e := range []store.EndpointRow{ep, off, other} {
				if err := db.InsertEndpoint(ctx, tx, e); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := db.GetEndpoint(ctx, "whe_1")
		if err != nil || !reflect.DeepEqual(got, ep) {
			t.Fatalf("get: %+v %v", got, err)
		}
		if _, err := db.GetEndpoint(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		list, err := db.ListEndpoints(ctx, "app_1")
		if err != nil || len(list) != 2 {
			t.Fatalf("list: %v %v", list, err)
		}
		var enabled []store.EndpointRow
		_ = db.Write(ctx, func(tx store.Tx) error {
			enabled, err = db.ListEnabledEndpoints(ctx, tx, "app_1")
			return err
		})
		if err != nil || len(enabled) != 1 || enabled[0].ID != "whe_1" {
			t.Fatalf("enabled: %v %v", enabled, err)
		}

		url, evs, en := "https://example.com/new", []string{"a", "b"}, false
		later := now.Add(time.Hour)
		if err := db.UpdateEndpoint(ctx, "whe_1", store.EndpointUpdate{URL: &url, EventTypes: &evs, Enabled: &en}, later); err != nil {
			t.Fatal(err)
		}
		got, _ = db.GetEndpoint(ctx, "whe_1")
		if got.URL != url || !reflect.DeepEqual(got.EventTypes, evs) || got.Enabled || !got.UpdatedAt.Equal(later) {
			t.Fatalf("after update: %+v", got)
		}
		if err := db.UpdateEndpoint(ctx, "nope", store.EndpointUpdate{Enabled: &en}, later); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("update missing: %v", err)
		}
	})
}

func TestEventAndDeliveryRows(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		migrated(t, db)
		now := time.UnixMilli(1_700_000_000_000).UTC()
		ev := store.EventRow{ID: "evt_1", OrgID: "org_1", AppID: "app_1", Mode: "test", Type: "payment.succeeded",
			ObjectType: "payment", ObjectID: "pay_1", ObjectVersion: 3, Data: json.RawMessage(`{"id":"pay_1"}`), CreatedAt: now}
		ep := store.EndpointRow{ID: "whe_1", OrgID: "org_1", AppID: "app_1", URL: "https://example.com/h",
			SigningSecretEnc: []byte{9}, Enabled: true, CreatedAt: now, UpdatedAt: now}
		err := db.Write(ctx, func(tx store.Tx) error {
			if err := db.InsertEvent(ctx, tx, ev); err != nil {
				return err
			}
			if err := db.InsertEndpoint(ctx, tx, ep); err != nil {
				return err
			}
			return db.InsertDelivery(ctx, tx, store.DeliveryRow{ID: "whd_1", EventID: "evt_1", EndpointID: "whe_1",
				Status: "pending", NextAttemptAt: now, CreatedAt: now, UpdatedAt: now})
		})
		if err != nil {
			t.Fatal(err)
		}
		// the (event, endpoint) pair is unique
		err = db.Write(ctx, func(tx store.Tx) error {
			return db.InsertDelivery(ctx, tx, store.DeliveryRow{ID: "whd_dup", EventID: "evt_1", EndpointID: "whe_1",
				Status: "pending", CreatedAt: now, UpdatedAt: now})
		})
		if err == nil {
			t.Fatal("duplicate delivery accepted")
		}

		d, err := db.GetDelivery(ctx, "whd_1")
		if err != nil {
			t.Fatal(err)
		}
		if d.Delivery.Status != "pending" || d.Delivery.AttemptCount != 0 || d.Event.Type != "payment.succeeded" ||
			string(d.Event.Data) != `{"id":"pay_1"}` || d.Event.ObjectVersion != 3 || d.Endpoint.URL != ep.URL {
			t.Fatalf("detail: %+v", d)
		}
		if _, err := db.GetDelivery(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}

		next := now.Add(time.Minute)
		err = db.Write(ctx, func(tx store.Tx) error {
			return db.UpdateDelivery(ctx, tx, "whd_1", store.DeliveryUpdate{Status: "pending", AttemptCount: 1,
				NextAttemptAt: next, LastStatus: 500, LastError: "http 500"}, now)
		})
		if err != nil {
			t.Fatal(err)
		}
		d, _ = db.GetDelivery(ctx, "whd_1")
		if d.Delivery.AttemptCount != 1 || d.Delivery.LastStatus != 500 || d.Delivery.LastError != "http 500" ||
			!d.Delivery.NextAttemptAt.Equal(next) || !d.Delivery.DeliveredAt.IsZero() {
			t.Fatalf("after fail: %+v", d.Delivery)
		}
		err = db.Write(ctx, func(tx store.Tx) error {
			return db.UpdateDelivery(ctx, tx, "whd_1", store.DeliveryUpdate{Status: "failed", AttemptCount: 9, LastStatus: 0, LastError: "x"}, now.Add(time.Hour))
		})
		if err != nil {
			t.Fatal(err)
		}
		ids, err := db.ListFailedDeliveryIDs(ctx, "whe_1", now)
		if err != nil || !reflect.DeepEqual(ids, []string{"whd_1"}) {
			t.Fatalf("failed ids: %v %v", ids, err)
		}
		ids, _ = db.ListFailedDeliveryIDs(ctx, "whe_1", now.Add(2*time.Hour))
		if len(ids) != 0 {
			t.Fatalf("since filter: %v", ids)
		}
	})
}

func TestInsertProviderEvent(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		migrated(t, db)
		now := time.UnixMilli(1_700_000_000_000).UTC()
		row := store.ProviderEventRow{ID: "pe_1", ProviderAccountID: "pa_1", ProviderEventID: "ev_1",
			Headers: json.RawMessage(`{"X-A":["b"]}`), Body: []byte("raw\x00body"), Verified: true, ReceivedAt: now}
		insert := func(r store.ProviderEventRow) bool {
			var ok bool
			if err := db.Write(ctx, func(tx store.Tx) (err error) {
				ok, err = db.InsertProviderEvent(ctx, tx, r)
				return
			}); err != nil {
				t.Fatal(err)
			}
			return ok
		}
		if !insert(row) {
			t.Fatal("first insert not reported")
		}
		dup := row
		dup.ID = "pe_2"
		if insert(dup) {
			t.Fatal("duplicate reported as inserted")
		}
		dup.ProviderAccountID = "pa_2"
		if !insert(dup) {
			t.Fatal("same event id on another account must insert")
		}
		got, err := db.GetProviderEvent(ctx, "pa_1", "ev_1")
		if err != nil || got.ID != "pe_1" || string(got.Body) != "raw\x00body" || !got.Verified ||
			string(got.Headers) != `{"X-A":["b"]}` || !got.ReceivedAt.Equal(now) {
			t.Fatalf("get: %+v %v", got, err)
		}
	})
}
