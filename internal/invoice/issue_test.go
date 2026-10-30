package invoice_test

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/invoice"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/internal/tax"
)

// golden is the expected content of an issued invoice.
type golden struct {
	number, supply, pos, currency string
	taxable, cgst, sgst, igst     int64
	total                         int64
	flags                         []string
	fx                            string
	lut                           string
	recipientState                string
}

func check(t *testing.T, inv store.Invoice, g golden) {
	t.Helper()
	if inv.Number != g.number || inv.SupplyType != g.supply || inv.PlaceOfSupply != g.pos || inv.Currency != g.currency ||
		inv.Taxable != g.taxable || inv.CGST != g.cgst || inv.SGST != g.sgst || inv.IGST != g.igst || inv.Total != g.total ||
		inv.FXRate != g.fx || inv.LUTNumber != g.lut {
		t.Errorf("invoice = %+v\nwant %+v", struct {
			Number, Supply, POS, Cur string
			T, C, S, I, Tot          int64
			FX, LUT                  string
		}{inv.Number, inv.SupplyType, inv.PlaceOfSupply, inv.Currency, inv.Taxable, inv.CGST, inv.SGST, inv.IGST, inv.Total, inv.FXRate, inv.LUTNumber}, g)
	}
	want := slices.Clone(g.flags)
	got := slices.Clone(inv.Flags)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		t.Errorf("flags = %v, want %v", got, want)
	}
	if inv.Taxable+inv.CGST+inv.SGST+inv.IGST != inv.Total {
		t.Errorf("components do not add up: %+v", inv)
	}
	var rcp invoice.Recipient
	_ = json.Unmarshal(inv.Recipient, &rcp)
	if g.recipientState != "" && rcp.StateCode != g.recipientState {
		t.Errorf("recipient state = %q want %q", rcp.StateCode, g.recipientState)
	}
}

func only(t *testing.T, e *env, paymentID string) store.Invoice {
	t.Helper()
	invs := e.invoices(paymentID)
	if len(invs) != 1 {
		t.Fatalf("invoices = %d, want 1", len(invs))
	}
	return invs[0]
}

func TestGoldenIntraStateB2C(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, a := e.pay(118000, "INR", intraB2C, "pa_1")
		inv := only(t, e, p.ID)
		check(t, inv, golden{number: "INV/2627/000001", supply: "b2c", pos: "27", currency: "INR",
			taxable: 100000, cgst: 9000, sgst: 9000, total: 118000, recipientState: "27"})
		if inv.AttemptID != a.ID || inv.PaymentRef != a.ProviderRef || inv.PaymentMethod != "upi" || inv.TemplateVersion != "v1" ||
			inv.GSTRateBP != 1800 || inv.InterState || inv.SeriesPrefix != "INV" || inv.FiscalYear != "2627" || inv.AppID != "app_1" || inv.Mode != "live" {
			t.Errorf("metadata = %+v", inv)
		}
		var lines []invoice.Line
		_ = json.Unmarshal(inv.Lines, &lines)
		if len(lines) != 1 || lines[0].SAC != "998315" || lines[0].Qty != 1 || lines[0].Description != "Voicestudio Pro - Oct 2026" ||
			lines[0].CGSTRateBP != 900 || lines[0].SGSTRateBP != 900 || lines[0].IGSTRateBP != 0 {
			t.Errorf("lines = %+v", lines)
		}
		// Events and jobs commit with the payment: invoice.issued with hosted URLs, and a render job.
		evs := e.eventsOf("invoice.issued")
		if len(evs) != 1 || evs[0].ObjectID != inv.ID || evs[0].AppID != "app_1" || evs[0].Mode != "live" {
			t.Fatalf("events = %v", e.emittedTypes())
		}
		raw, _ := json.Marshal(evs[0].Data)
		var data struct {
			Number    string `json:"number"`
			HostedURL string `json:"hosted_url"`
			PDFURL    string `json:"pdf_url"`
		}
		_ = json.Unmarshal(raw, &data)
		if data.Number != inv.Number || !strings.HasPrefix(data.HostedURL, baseURL+"/i/"+inv.ID+"?t=") ||
			!strings.HasPrefix(data.PDFURL, baseURL+"/i/"+inv.ID+".pdf?t=") || inv.AccessTokenHash == "" ||
			strings.Contains(string(raw), inv.AccessTokenHash) {
			t.Errorf("event data = %s", raw)
		}
		var pending int
		if err := db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE kind = 'invoice.render'").Scan(&pending)
		}); err != nil || pending != 1 {
			t.Errorf("render jobs = %d err=%v", pending, err)
		}
	})
}

func TestGoldenInterStateB2BAndB2C(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		buyer := validGSTIN(t, "29ABCDE1234F1Z")
		b2b := &core.CustomerInput{Name: "Acme", Email: "ap@acme.example.com",
			Billing: store.Billing{LegalName: "Acme Pvt Ltd", GSTIN: buyer, Line1: "Brigade Rd", City: "Bengaluru", PostalCode: "560001", Country: "IN"}}
		p1, _ := e.pay(118000, "INR", b2b, "pa_1")
		check(t, only(t, e, p1.ID), golden{number: "INV/2627/000001", supply: "b2b", pos: "29", currency: "INR",
			taxable: 100000, igst: 18000, total: 118000, recipientState: "29"})
		p2, _ := e.pay(59000, "INR", interB2C, "pa_1")
		check(t, only(t, e, p2.ID), golden{number: "INV/2627/000002", supply: "b2c", pos: "29", currency: "INR",
			taxable: 50000, igst: 9000, total: 59000})
		var rcp invoice.Recipient
		_ = json.Unmarshal(only(t, e, p1.ID).Recipient, &rcp)
		if rcp.Name != "Acme Pvt Ltd" || rcp.GSTIN != buyer || rcp.Address != "Brigade Rd, Bengaluru, 560001" || rcp.StateName != "Karnataka" {
			t.Errorf("recipient = %+v", rcp)
		}
	})
}

func TestGoldenExportLUTAndFXPending(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		// No FX rate for the day: issued, flagged fx_pending, INR value comes later.
		p, _ := e.pay(5000, "USD", usBuyer, "pa_1")
		inv := only(t, e, p.ID)
		check(t, inv, golden{number: "INV/2627/000001", supply: "export_lut", pos: "OUTSIDE_INDIA", currency: "USD",
			taxable: 5000, total: 5000, flags: []string{"fx_pending"}, lut: "AD270226000001A"})
		// With a rate, no flag and the rate is stored on the invoice.
		e.setFX("USD", "2026-10-17", "83.2500")
		p2, _ := e.pay(5000, "USD", usBuyer, "pa_1")
		check(t, only(t, e, p2.ID), golden{number: "INV/2627/000002", supply: "export_lut", pos: "OUTSIDE_INDIA", currency: "USD",
			taxable: 5000, total: 5000, fx: "83.2500", lut: "AD270226000001A"})
	})
}

func TestGoldenExportIGSTWhenLUTExpired(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(en *store.TaxEntity, _ *store.AppTaxProfile) {
			en.LUTValidTo = time.Date(2026, 9, 30, 0, 0, 0, 0, tax.IST)
		})
		e.setFX("USD", "2026-10-17", "83.0000")
		p, _ := e.pay(5000, "USD", usBuyer, "pa_1")
		check(t, only(t, e, p.ID), golden{number: "INV/2627/000001", supply: "export_igst", pos: "OUTSIDE_INDIA", currency: "USD",
			taxable: 4237, igst: 763, total: 5000, flags: []string{"export_igst"}, fx: "83.0000"})
	})
}

func TestGoldenPOSDefaulted(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", noState, "pa_1")
		check(t, only(t, e, p.ID), golden{number: "INV/2627/000001", supply: "b2c", pos: "27", currency: "INR",
			taxable: 100000, cgst: 9000, sgst: 9000, total: 118000, flags: []string{"pos_defaulted"}})
	})
}

func TestBadBillingDoesNotBlockThePayment(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		bad := &core.CustomerInput{Name: "X", Billing: store.Billing{GSTIN: "29ABCDE1234F1ZZ", Country: "IN"}}
		p, _ := e.pay(11800, "INR", bad, "pa_1")
		if p.Status != "succeeded" {
			t.Fatalf("payment %s", p.Status)
		}
		inv := only(t, e, p.ID)
		if !inv.HasFlag("recipient_invalid") || !inv.HasFlag("pos_defaulted") || inv.SupplyType != "b2c" {
			t.Errorf("invoice = %+v", inv)
		}
	})
}

func TestTaxExclusivePricing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(_ *store.TaxEntity, p *store.AppTaxProfile) { p.PricesIncludeTax = false })
		pay, _ := e.pay(100000, "INR", intraB2C, "pa_1")
		check(t, only(t, e, pay.ID), golden{number: "INV/2627/000001", supply: "b2c", pos: "27", currency: "INR",
			taxable: 100000, cgst: 9000, sgst: 9000, total: 118000})
	})
}

func TestNoInvoiceWhenNotDue(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		// Merchant of record: the connector issues the document.
		p, _ := e.pay(118000, "INR", intraB2C, "pa_mor")
		if n := len(e.invoices(p.ID)); n != 0 {
			t.Errorf("MoR payment got %d invoices", n)
		}
		// Duplicate attempt: second success on a succeeded payment.
		res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 118000, Currency: "INR", Customer: intraB2C})
		if err != nil {
			t.Fatal(err)
		}
		a1, _ := e.core.CreateAttempt(ctx, res.Payment.ID, "pa_1")
		a2, _ := e.core.CreateAttempt(ctx, res.Payment.ID, "pa_1")
		snap := core.Snapshot{Status: "captured", Amount: 118000, Currency: "INR", Method: "upi"}
		if _, err := e.core.ApplyAttemptSnapshot(ctx, a1.ID, snap); err != nil {
			t.Fatal(err)
		}
		if _, err := e.core.ApplyAttemptSnapshot(ctx, a2.ID, snap); err != nil {
			t.Fatal(err)
		}
		invs := e.invoices(res.Payment.ID)
		if len(invs) != 1 || invs[0].AttemptID != a1.ID {
			t.Errorf("duplicate got its own invoice: %+v", invs)
		}
		// issue_invoices off: none.
		if err := db.Write(ctx, func(tx store.Tx) error {
			return store.UpsertAppTaxProfile(ctx, tx, store.AppTaxProfile{AppID: "app_1", TaxEntityID: "te_1", IssueInvoices: false,
				SACCode: "998315", GSTRateBP: 1800, PricesIncludeTax: true}, e.now)
		}); err != nil {
			t.Fatal(err)
		}
		p3, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		if n := len(e.invoices(p3.ID)); n != 0 {
			t.Errorf("issue_invoices=off still produced %d invoices", n)
		}
		// An app without any profile issues nothing either.
		if err := db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "DELETE FROM app_tax_profiles")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		p4, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		if n := len(e.invoices(p4.ID)); n != 0 {
			t.Errorf("no profile still produced %d invoices", n)
		}
	})
}

func TestTestModeUsesItsOwnSeries(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		live, _ := e.pay(11800, "INR", intraB2C, "pa_1")
		e.scope = core.Scope{AppID: "app_1", Mode: "test"}
		test, _ := e.pay(11800, "INR", intraB2C, "pa_test")
		e.scope = core.Scope{AppID: "app_1", Mode: "live"}
		live2, _ := e.pay(11800, "INR", intraB2C, "pa_1")
		if n := only(t, e, test.ID).Number; n != "TEST/2627/000001" {
			t.Errorf("test number = %s", n)
		}
		if a, b := only(t, e, live.ID).Number, only(t, e, live2.ID).Number; a != "INV/2627/000001" || b != "INV/2627/000002" {
			t.Errorf("live numbers = %s, %s (test mode must not consume them)", a, b)
		}
	})
}

func TestPerAppPrefixGivesItsOwnSeries(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(_ *store.TaxEntity, p *store.AppTaxProfile) { p.InvoicePrefix = "VS" })
		p, _ := e.pay(11800, "INR", intraB2C, "pa_1")
		if n := only(t, e, p.ID).Number; n != "VS/2627/000001" {
			t.Errorf("number = %s", n)
		}
	})
}

func TestFiscalYearRollover(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.setNow(time.Date(2027, 3, 31, 18, 29, 59, 0, time.UTC)) // 23:59:59 IST, last second of 2026-27
		p1, _ := e.pay(11800, "INR", intraB2C, "pa_1")
		e.setNow(time.Date(2027, 3, 31, 18, 30, 0, 0, time.UTC)) // 00:00:00 IST on 1 April
		p2, _ := e.pay(11800, "INR", intraB2C, "pa_1")
		p3, _ := e.pay(11800, "INR", intraB2C, "pa_1")
		got := []string{only(t, e, p1.ID).Number, only(t, e, p2.ID).Number, only(t, e, p3.ID).Number}
		want := []string{"INV/2627/000001", "INV/2728/000001", "INV/2728/000002"}
		if !slices.Equal(got, want) {
			t.Errorf("numbers = %v want %v", got, want)
		}
	})
}

func TestGaplessUnder20ConcurrentIssuers(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		const n = 20
		type pair struct{ pay, att string }
		pairs := make([]pair, n)
		for i := range pairs {
			res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 11800, Currency: "INR", Customer: intraB2C})
			if err != nil {
				t.Fatal(err)
			}
			a, err := e.core.CreateAttempt(ctx, res.Payment.ID, "pa_1")
			if err != nil {
				t.Fatal(err)
			}
			pairs[i] = pair{res.Payment.ID, a.ID}
		}
		var wg sync.WaitGroup
		for _, pr := range pairs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := e.core.ApplyAttemptSnapshot(ctx, pr.att, core.Snapshot{Status: "captured", Amount: 11800, Currency: "INR"}); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		var nums []int
		for _, pr := range pairs {
			inv := only(t, e, pr.pay)
			seq, err := strconv.Atoi(strings.TrimLeft(strings.TrimPrefix(inv.Number, "INV/2627/"), "0"))
			if err != nil {
				t.Fatal(err)
			}
			nums = append(nums, seq)
		}
		slices.Sort(nums)
		for i, v := range nums {
			if v != i+1 {
				t.Fatalf("numbers not gapless: %v", nums)
			}
		}
	})
}
