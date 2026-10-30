package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestInvoiceEInvoiceFieldsRoundTrip(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		_ = db.Write(ctx, func(tx store.Tx) error {
			if err := store.InsertTaxEntity(ctx, tx, entity("te_a", "27AAPFU0939F1ZV")); err != nil {
				return err
			}
			if err := store.InsertInvoice(ctx, tx, inv("inv_a", "te_a", "app_a", "live", "INV/2627/000001", t0)); err != nil {
				return err
			}
			return store.InsertInvoice(ctx, tx, inv("inv_b", "te_a", "app_a", "live", "INV/2627/000002", t0))
		})
		get := func(id string) (i store.Invoice) {
			_ = db.Read(ctx, func(q store.Querier) (err error) { i, err = store.GetInvoice(ctx, q, id); return })
			return
		}
		if i := get("inv_a"); i.IRN != "" || i.AckNo != "" || !i.AckDate.IsZero() || i.SignedQR != "" || !i.EInvoiceAt.IsZero() {
			t.Fatalf("a fresh invoice has no e-invoice data: %+v", i)
		}

		irn := strings.Repeat("ab12", 16)
		ack := t0.Add(2 * time.Hour)
		err := db.Write(ctx, func(tx store.Tx) error {
			return store.SetInvoiceEInvoice(ctx, tx, "inv_a", irn, "112010012345678", ack, "aaa.bbb.ccc", t0.Add(3*time.Hour))
		})
		if err != nil {
			t.Fatal(err)
		}
		i := get("inv_a")
		if i.IRN != irn || i.AckNo != "112010012345678" || !i.AckDate.Equal(ack) || i.SignedQR != "aaa.bbb.ccc" || !i.EInvoiceAt.Equal(t0.Add(3*time.Hour)) {
			t.Fatalf("stored: %+v", i)
		}
		// The same IRN on another invoice is refused: a pasted-twice mistake.
		err = db.Write(ctx, func(tx store.Tx) error {
			return store.SetInvoiceEInvoice(ctx, tx, "inv_b", irn, "1", ack, "x.y.z", t0)
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate IRN: %v", err)
		}
		if b := get("inv_b"); b.IRN != "" {
			t.Error("a refused IRN was stored")
		}
		// Correcting the same invoice's data is fine.
		err = db.Write(ctx, func(tx store.Tx) error {
			return store.SetInvoiceEInvoice(ctx, tx, "inv_a", irn, "999", ack, "aaa.bbb.ddd", t0.Add(4*time.Hour))
		})
		if err != nil || get("inv_a").AckNo != "999" {
			t.Fatalf("correction: %v %+v", err, get("inv_a"))
		}
		err = db.Write(ctx, func(tx store.Tx) error {
			return store.SetInvoiceEInvoice(ctx, tx, "inv_missing", strings.Repeat("cd", 32), "1", ack, "x.y.z", t0)
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown invoice: %v", err)
		}

		// The PDF can be dropped so that it is rendered again with the QR code.
		_ = db.Write(ctx, func(tx store.Tx) error { return store.SetInvoicePDF(ctx, tx, "inv_a", "blob_1") })
		if get("inv_a").PDFBlobID != "blob_1" {
			t.Fatal("pdf not set")
		}
		if err := db.Write(ctx, func(tx store.Tx) error { return store.ClearInvoicePDF(ctx, tx, "inv_a") }); err != nil {
			t.Fatal(err)
		}
		if get("inv_a").PDFBlobID != "" {
			t.Error("pdf not cleared")
		}
	})
}
