package store_test

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestDocumentNumbersAreGaplessUnderConcurrency(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		const n = 20
		var mu sync.Mutex
		var got []int64
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := db.Write(ctx, func(tx store.Tx) error {
					v, err := store.NextDocumentNumber(ctx, tx, "te_1", "invoice", "INV", "2627")
					mu.Lock()
					got = append(got, v)
					mu.Unlock()
					return err
				})
				if err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		for i, v := range got {
			if v != int64(i+1) {
				t.Fatalf("numbers = %v", got)
			}
		}
	})
}

func TestDocumentNumberRollbackReturnsTheNumber(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		next := func(commit bool) (v int64) {
			_ = db.Write(ctx, func(tx store.Tx) error {
				var err error
				v, err = store.NextDocumentNumber(ctx, tx, "te_1", "invoice", "INV", "2627")
				if !commit {
					return context.Canceled
				}
				return err
			})
			return v
		}
		if a, b, c := next(true), next(false), next(true); a != 1 || b != 2 || c != 2 {
			t.Fatalf("got %d %d %d, want 1 2 2 (a rolled-back number is reused)", a, b, c)
		}
		// Another series (kind, prefix or fiscal year) counts independently.
		_ = db.Write(ctx, func(tx store.Tx) error {
			for _, c := range []struct{ kind, prefix, fy string }{{"credit_note", "INV", "2627"}, {"invoice", "VS", "2627"}, {"invoice", "INV", "2728"}} {
				if v, err := store.NextDocumentNumber(ctx, tx, "te_1", c.kind, c.prefix, c.fy); err != nil || v != 1 {
					t.Errorf("%+v: v=%d err=%v", c, v, err)
				}
			}
			return nil
		})
	})
}

func TestTaxEntityProfileFXRoundTrip(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
		e := store.TaxEntity{ID: "te_1", OrgID: "org", LegalName: "Acme Pvt Ltd", GSTIN: "29AAAAA0000A1Z5", StateCode: "29",
			LUTNumber: "AD290226000001Z", LUTValidFrom: now, LUTValidTo: now.AddDate(1, 0, 0), InvoicePrefix: "INV", CreditNotePrefix: "CN",
			CreatedAt: now, UpdatedAt: now}
		err := db.Write(ctx, func(tx store.Tx) error {
			if err := store.InsertTaxEntity(ctx, tx, e); err != nil {
				return err
			}
			if err := store.UpsertAppTaxProfile(ctx, tx, store.AppTaxProfile{AppID: "app_1", TaxEntityID: "te_1", IssueInvoices: true,
				SACCode: "998315", GSTRateBP: 1800, PricesIncludeTax: true}, now); err != nil {
				return err
			}
			if err := store.UpsertAppTaxProfile(ctx, tx, store.AppTaxProfile{AppID: "app_1", TaxEntityID: "te_1", IssueInvoices: false,
				SACCode: "997331", GSTRateBP: 1800, InvoicePrefix: "VS"}, now); err != nil {
				return err
			}
			return store.UpsertFXRate(ctx, tx, store.FXRate{OrgID: "org", Currency: "USD", Date: "2026-10-17", Rate: "83.2500", Source: "manual"}, now)
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			got, err := store.GetTaxEntity(ctx, q, "te_1")
			if err != nil || got.LegalName != e.LegalName || !got.LUTValidTo.Equal(e.LUTValidTo) || got.LUTNumber != e.LUTNumber {
				t.Errorf("entity = %+v err=%v", got, err)
			}
			if by, err := store.FindTaxEntityByGSTIN(ctx, q, "org", e.GSTIN); err != nil || by.ID != "te_1" {
				t.Errorf("by gstin: %+v %v", by, err)
			}
			p, err := store.GetAppTaxProfile(ctx, q, "app_1")
			if err != nil || p.IssueInvoices || p.SACCode != "997331" || p.InvoicePrefix != "VS" || p.PricesIncludeTax {
				t.Errorf("profile = %+v err=%v", p, err)
			}
			r, err := store.GetFXRate(ctx, q, "org", "USD", "2026-10-17")
			if err != nil || r.Rate != "83.2500" {
				t.Errorf("fx = %+v err=%v", r, err)
			}
			if _, err := store.GetFXRate(ctx, q, "org", "USD", "2026-10-18"); err != store.ErrNotFound {
				t.Errorf("missing fx err = %v", err)
			}
			return nil
		})
		dup := e
		dup.ID = "te_2"
		err = db.Write(ctx, func(tx store.Tx) error { return store.InsertTaxEntity(ctx, tx, dup) })
		if err == nil {
			t.Error("duplicate GSTIN accepted")
		}
	})
}
