package store_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func entity(id, gstin string) store.TaxEntity {
	return store.TaxEntity{ID: id, OrgID: store.DefaultOrgID, LegalName: "Legal " + id, GSTIN: gstin, PAN: "AAPFU0939F", Address: "addr",
		StateCode: "27", InvoicePrefix: "INV", CreditNotePrefix: "CN", CreatedAt: t0, UpdatedAt: t0}
}

func inv(id, entityID, appID, mode, number string, issued time.Time, flags ...string) store.Invoice {
	return store.Invoice{ID: id, OrgID: store.DefaultOrgID, Mode: mode, AppID: appID, TaxEntityID: entityID, PaymentID: "pay_" + id, AttemptID: "att_" + id,
		Number: number, SeriesPrefix: "INV", FiscalYear: "2627", IssuedAt: issued, SupplyType: "b2c", PlaceOfSupply: "27", Currency: "INR",
		Supplier: json.RawMessage(`{}`), Recipient: json.RawMessage(`{}`), Lines: json.RawMessage(`[]`), Total: 100, Flags: flags,
		TemplateVersion: "v1", AccessTokenHash: "h" + id}
}

func TestTaxEntityListAndUpdate(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		err := db.Write(ctx, func(tx store.Tx) error {
			a, b := entity("te_a", "27AAPFU0939F1ZV"), entity("te_b", "29AAPFU0939F1ZP")
			b.CreatedAt = t0.Add(time.Second)
			if err := store.InsertTaxEntity(ctx, tx, a); err != nil {
				return err
			}
			return store.InsertTaxEntity(ctx, tx, b)
		})
		if err != nil {
			t.Fatal(err)
		}
		var list []store.TaxEntity
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			list, err = store.ListTaxEntities(ctx, q, store.DefaultOrgID)
			return
		})
		if len(list) != 2 || list[0].ID != "te_a" || list[1].ID != "te_b" {
			t.Fatalf("list %+v", list)
		}
		e := list[0]
		e.TradeName, e.Address, e.LUTNumber = "Trade", "new address", "AD270226000001A"
		e.LUTValidFrom, e.LUTValidTo = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC)
		e.InvoicePrefix, e.CreditNotePrefix, e.LogoBlobID = "VS", "VSC", "blob_x"
		e.SignatoryName, e.FooterNote, e.SupportEmail, e.TurnoverBand = "Sig", "foot", "h@example.com", "5cr"
		if err := db.Write(ctx, func(tx store.Tx) error { return store.UpdateTaxEntity(ctx, tx, e, t0.Add(time.Hour)) }); err != nil {
			t.Fatal(err)
		}
		var got store.TaxEntity
		_ = db.Read(ctx, func(q store.Querier) (err error) { got, err = store.GetTaxEntity(ctx, q, "te_a"); return })
		e.UpdatedAt = t0.Add(time.Hour)
		if !reflect.DeepEqual(got, e) {
			t.Fatalf("updated\n got %+v\nwant %+v", got, e)
		}
		// clearing the LUT
		e.LUTNumber, e.LUTValidFrom, e.LUTValidTo = "", time.Time{}, time.Time{}
		_ = db.Write(ctx, func(tx store.Tx) error { return store.UpdateTaxEntity(ctx, tx, e, t0) })
		_ = db.Read(ctx, func(q store.Querier) (err error) { got, err = store.GetTaxEntity(ctx, q, "te_a"); return })
		if got.LUTNumber != "" || !got.LUTValidFrom.IsZero() {
			t.Fatalf("LUT not cleared: %+v", got)
		}
		err = db.Write(ctx, func(tx store.Tx) error { return store.UpdateTaxEntity(ctx, tx, entity("te_none", "x"), t0) })
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing entity: %v", err)
		}
		// GSTIN of another entity is a conflict
		e2 := list[1]
		e2.GSTIN = list[0].GSTIN
		err = db.Write(ctx, func(tx store.Tx) error { return store.UpdateTaxEntity(ctx, tx, e2, t0) })
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate gstin: %v", err)
		}
	})
}

func TestSearchInvoicesAndCreditNotes(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		oct, nov := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
		err := db.Write(ctx, func(tx store.Tx) error {
			for _, e := range []store.TaxEntity{entity("te_a", "27AAPFU0939F1ZV"), entity("te_b", "29AAPFU0939F1ZP")} {
				if err := store.InsertTaxEntity(ctx, tx, e); err != nil {
					return err
				}
			}
			for _, i := range []store.Invoice{
				inv("inv_1", "te_a", "app_a", "live", "INV/2627/000001", oct),
				inv("inv_2", "te_a", "app_b", "live", "INV/2627/000002", oct.Add(time.Hour), "fx_pending"),
				inv("inv_3", "te_b", "app_a", "live", "INV/2627/000003", nov, "pos_defaulted", "export_igst"),
				inv("inv_4", "te_a", "app_a", "test", "TEST/2627/000001", nov),
				func() store.Invoice {
					i := inv("inv_x", "te_a", "app_a", "live", "INV/2627/000099", nov)
					i.OrgID = "org_other"
					return i
				}(),
			} {
				if err := store.InsertInvoice(ctx, tx, i); err != nil {
					return err
				}
			}
			for _, c := range []store.CreditNote{
				{ID: "crn_1", OrgID: store.DefaultOrgID, Mode: "live", AppID: "app_a", TaxEntityID: "te_a", InvoiceID: "inv_1", PaymentID: "pay_inv_1", RefundID: "ref_1",
					Number: "CN/2627/000001", SeriesPrefix: "CN", FiscalYear: "2627", IssuedAt: nov, Reason: "r", Currency: "INR", Lines: json.RawMessage(`[]`), Total: 10,
					TemplateVersion: "v1", AccessTokenHash: "hc1"},
				{ID: "crn_x", OrgID: "org_other", Mode: "live", AppID: "app_a", TaxEntityID: "te_a", InvoiceID: "inv_1", PaymentID: "pay_inv_1", RefundID: "ref_x",
					Number: "CN/2627/000099", SeriesPrefix: "CN", FiscalYear: "2627", IssuedAt: nov, Reason: "r", Currency: "INR", Lines: json.RawMessage(`[]`), Total: 10,
					TemplateVersion: "v1", AccessTokenHash: "hcx"},
			} {
				if err := store.InsertCreditNote(ctx, tx, c); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		ids := func(f store.DocFilter, after string, limit int) []string {
			var out []string
			_ = db.Read(ctx, func(q store.Querier) error {
				rows, err := store.SearchInvoices(ctx, q, f, after, limit)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range rows {
					out = append(out, r.ID)
				}
				return nil
			})
			return out
		}
		for name, c := range map[string]struct {
			f    store.DocFilter
			want []string
		}{
			"live all":     {store.DocFilter{OrgID: store.DefaultOrgID, Mode: "live"}, []string{"inv_3", "inv_2", "inv_1"}},
			"test mode":    {store.DocFilter{OrgID: store.DefaultOrgID, Mode: "test"}, []string{"inv_4"}},
			"entity":       {store.DocFilter{OrgID: store.DefaultOrgID, Mode: "live", EntityID: "te_a"}, []string{"inv_2", "inv_1"}},
			"app":          {store.DocFilter{OrgID: store.DefaultOrgID, Mode: "live", AppID: "app_a"}, []string{"inv_3", "inv_1"}},
			"month":        {store.DocFilter{OrgID: store.DefaultOrgID, Mode: "live", From: oct.AddDate(0, 0, -9), To: oct.AddDate(0, 1, -9)}, []string{"inv_2", "inv_1"}},
			"flag":         {store.DocFilter{OrgID: store.DefaultOrgID, Mode: "live", Flag: "fx_pending"}, []string{"inv_2"}},
			"flag export":  {store.DocFilter{OrgID: store.DefaultOrgID, Mode: "live", Flag: "export_igst"}, []string{"inv_3"}},
			"flag no such": {store.DocFilter{OrgID: store.DefaultOrgID, Mode: "live", Flag: "pos"}, nil},
		} {
			if got := ids(c.f, "", 10); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: got %v want %v", name, got, c.want)
			}
		}
		if got := ids(store.DocFilter{OrgID: store.DefaultOrgID, Mode: "live"}, "inv_3", 10); !reflect.DeepEqual(got, []string{"inv_2", "inv_1"}) {
			t.Errorf("cursor: %v", got)
		}
		if got := ids(store.DocFilter{OrgID: store.DefaultOrgID, Mode: "live"}, "", 2); len(got) != 2 {
			t.Errorf("limit: %v", got)
		}
		var cns []store.CreditNote
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			cns, err = store.SearchCreditNotes(ctx, q, store.DocFilter{OrgID: store.DefaultOrgID, Mode: "live", EntityID: "te_a"}, "", 10)
			return
		})
		if len(cns) != 1 || cns[0].ID != "crn_1" {
			t.Fatalf("credit notes %+v", cns)
		}
	})
}

func TestFXRatesAndDomainsLists(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		err := db.Write(ctx, func(tx store.Tx) error {
			for _, r := range []store.FXRate{
				{OrgID: store.DefaultOrgID, Currency: "USD", Date: "2026-10-01", Rate: "83.25", Source: "manual"},
				{OrgID: store.DefaultOrgID, Currency: "USD", Date: "2026-10-03", Rate: "83.50", Source: "manual"},
				{OrgID: store.DefaultOrgID, Currency: "EUR", Date: "2026-10-03", Rate: "90.10", Source: "provider"},
			} {
				if err := store.UpsertFXRate(ctx, tx, r, t0); err != nil {
					return err
				}
			}
			for i, h := range []string{"pay.alpha.example", "checkout.alpha.example"} {
				d := store.Domain{ID: "dom_" + string(rune('a'+i)), OrgID: store.DefaultOrgID, AppID: "app_a", Host: h, Status: "pending", Token: "tok" + h, IsPrimary: i == 0,
					CreatedAt: t0.Add(time.Duration(i) * time.Second)}
				if err := store.InsertDomain(ctx, tx, d); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var rates []store.FXRate
		var doms []store.Domain
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			if rates, err = store.ListFXRates(ctx, q, store.DefaultOrgID, 2); err != nil {
				return
			}
			doms, err = store.ListDomains(ctx, q, "app_a")
			return
		})
		// newest date first, then currency
		if len(rates) != 2 || rates[0].Date != "2026-10-03" || rates[1].Date != "2026-10-03" || rates[0].Currency != "EUR" || rates[1].Currency != "USD" {
			t.Fatalf("rates %+v", rates)
		}
		if len(doms) != 2 || doms[0].Host != "pay.alpha.example" || !doms[0].IsPrimary || doms[1].Token != "tokcheckout.alpha.example" {
			t.Fatalf("domains %+v", doms)
		}
	})
}

func TestInvoicesPendingFXAllCurrencies(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		when := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
		err := db.Write(ctx, func(tx store.Tx) error {
			if err := store.InsertTaxEntity(ctx, tx, entity("te_a", "27AAPFU0939F1ZV")); err != nil {
				return err
			}
			usd, eur, done := inv("inv_1", "te_a", "app_a", "live", "INV/2627/000001", when, "fx_pending"), inv("inv_2", "te_a", "app_a", "live", "INV/2627/000002", when, "fx_pending"),
				inv("inv_3", "te_a", "app_a", "live", "INV/2627/000003", when, "fx_pending")
			usd.Currency, eur.Currency, done.Currency = "USD", "EUR", "USD"
			other := inv("inv_4", "te_a", "app_a", "live", "INV/2627/000004", when, "fx_pending")
			other.Currency, other.OrgID = "USD", "org_other"
			for _, i := range []store.Invoice{usd, eur, done, other} {
				if err := store.InsertInvoice(ctx, tx, i); err != nil {
					return err
				}
			}
			return store.SetInvoiceFX(ctx, tx, "inv_3", "83.0", "manual", nil) // rate known: no longer pending
		})
		if err != nil {
			t.Fatal(err)
		}
		var got []store.Invoice
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			got, err = store.AllInvoicesPendingFX(ctx, q, store.DefaultOrgID)
			return
		})
		if len(got) != 2 || got[0].ID != "inv_1" || got[1].ID != "inv_2" {
			t.Fatalf("%+v", got)
		}
	})
}

func TestListFXRatesFor(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		err := db.Write(ctx, func(tx store.Tx) error {
			for _, r := range []store.FXRate{
				{OrgID: store.DefaultOrgID, Currency: "USD", Date: "2026-10-03", Rate: "83.50", Source: "manual"},
				{OrgID: store.DefaultOrgID, Currency: "USD", Date: "2026-10-01", Rate: "83.25", Source: "manual"},
				{OrgID: store.DefaultOrgID, Currency: "EUR", Date: "2026-10-02", Rate: "90.10", Source: "provider"},
			} {
				if err := store.UpsertFXRate(ctx, tx, r, t0); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var usd, none []store.FXRate
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			if usd, err = store.ListFXRatesFor(ctx, q, store.DefaultOrgID, "USD"); err != nil {
				return
			}
			none, err = store.ListFXRatesFor(ctx, q, "org_other", "USD")
			return
		})
		if len(usd) != 2 || usd[0].Date != "2026-10-01" || usd[1].Date != "2026-10-03" || usd[1].Rate != "83.50" || len(none) != 0 {
			t.Fatalf("usd %+v other %+v", usd, none)
		}
	})
}

func TestSearchInvoicesQ(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		oct := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
		err := db.Write(ctx, func(tx store.Tx) error {
			if err := store.InsertTaxEntity(ctx, tx, entity("te_a", "27AAPFU0939F1ZV")); err != nil {
				return err
			}
			for i, c := range []struct{ id, number, buyer string }{
				{"inv_1", "INV/2627/000001", "Asha Traders"},
				{"inv_2", "INV/2627/000002", "Bob Ltd"},
				{"inv_3", "INV/2627/000013", "50% Off_Co"},
			} {
				x := inv(c.id, "te_a", "app_a", "live", c.number, oct.Add(time.Duration(i)*time.Hour))
				x.Recipient = json.RawMessage(`{"name":"` + c.buyer + `"}`)
				if err := store.InsertInvoice(ctx, tx, x); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		ids := func(q string) []string {
			var out []string
			_ = db.Read(ctx, func(qr store.Querier) error {
				rows, err := store.SearchInvoices(ctx, qr, store.DocFilter{OrgID: store.DefaultOrgID, Mode: "live", Q: q}, "", 10)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range rows {
					out = append(out, r.ID)
				}
				return nil
			})
			return out
		}
		for name, c := range map[string]struct {
			q    string
			want []string
		}{
			"exact number":       {"INV/2627/000002", []string{"inv_2"}},
			"prefix":             {"INV/2627/00000", []string{"inv_2", "inv_1"}},
			"prefix any case":    {"inv/2627/0000", []string{"inv_3", "inv_2", "inv_1"}},
			"buyer substring":    {"traders", []string{"inv_1"}},
			"buyer any case":     {"BOB", []string{"inv_2"}},
			"percent literal":    {"%", []string{"inv_3"}},
			"underscore literal": {"_", []string{"inv_3"}},
			"backslash literal":  {`\`, nil},
			"no match":           {"zzz", nil},
			"empty is no filter": {"", []string{"inv_3", "inv_2", "inv_1"}},
		} {
			if got := ids(c.q); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s (%q): got %v want %v", name, c.q, got, c.want)
			}
		}
	})
}
