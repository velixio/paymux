package store_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestTaxEntityDSCRoundTrip(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		if err := db.Write(ctx, func(tx store.Tx) error { return store.InsertTaxEntity(ctx, tx, entity("te_a", "27AAPFU0939F1ZV")) }); err != nil {
			t.Fatal(err)
		}
		read := func() (store.EntityDSC, bool) {
			var d store.EntityDSC
			var ok bool
			if err := db.Read(ctx, func(q store.Querier) (err error) { d, ok, err = store.GetTaxEntityDSC(ctx, q, "te_a"); return }); err != nil {
				t.Fatal(err)
			}
			return d, ok
		}
		if _, ok := read(); ok {
			t.Fatal("a new entity has a DSC")
		}
		want := store.EntityDSC{Enc: []byte{1, 2, 3, 0, 255}, Subject: "CN=Test Signatory", Fingerprint: "ab12",
			NotAfter: t0.AddDate(3, 0, 0), UploadedAt: t0}
		if err := db.Write(ctx, func(tx store.Tx) error { return store.SetTaxEntityDSC(ctx, tx, "te_a", want, t0) }); err != nil {
			t.Fatal(err)
		}
		got, ok := read()
		if !ok || !bytes.Equal(got.Enc, want.Enc) || got.Subject != want.Subject || got.Fingerprint != "ab12" ||
			!got.NotAfter.Equal(want.NotAfter) || !got.UploadedAt.Equal(t0) {
			t.Fatalf("got %+v ok=%v", got, ok)
		}
		// The entity itself is untouched and its listing does not carry the sealed blob.
		var list []store.TaxEntity
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			list, err = store.ListTaxEntities(ctx, q, store.DefaultOrgID)
			return
		})
		if len(list) != 1 || list[0].DSCSubject != "CN=Test Signatory" || !list[0].DSCNotAfter.Equal(want.NotAfter) {
			t.Errorf("entity = %+v", list)
		}
		// Replace, then clear.
		want.Enc, want.Fingerprint = []byte{9}, "cd34"
		_ = db.Write(ctx, func(tx store.Tx) error { return store.SetTaxEntityDSC(ctx, tx, "te_a", want, t0.Add(time.Hour)) })
		if got, _ := read(); !bytes.Equal(got.Enc, []byte{9}) || got.Fingerprint != "cd34" {
			t.Errorf("replace: %+v", got)
		}
		if err := db.Write(ctx, func(tx store.Tx) error { return store.ClearTaxEntityDSC(ctx, tx, "te_a", t0) }); err != nil {
			t.Fatal(err)
		}
		if _, ok := read(); ok {
			t.Error("still there after Clear")
		}
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			list, err = store.ListTaxEntities(ctx, q, store.DefaultOrgID)
			return
		})
		if list[0].DSCSubject != "" || !list[0].DSCNotAfter.IsZero() {
			t.Errorf("entity after clear = %+v", list[0])
		}
		// An unknown entity.
		err := db.Write(ctx, func(tx store.Tx) error { return store.SetTaxEntityDSC(ctx, tx, "te_none", want, t0) })
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown entity: %v", err)
		}
	})
}
