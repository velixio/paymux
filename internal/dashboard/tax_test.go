package dashboard

import (
	"archive/zip"
	"bytes"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestReportsPageDownloadsAndZip(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, ent := e.taxApp("alpha", supplierGSTIN)
		e.pay(app, 118000, "INR", intraB2C)
		e.pay(app, 118000, "INR", interB2C)
		e.runJobs()
		set, err := e.inv.Reports(ctx, ent.ID, "2027-01")
		if err != nil || len(set.Files) < 5 {
			t.Fatalf("setup: %d files, %v", len(set.Files), err)
		}
		c := e.roleClient("viewer")

		// without a choice the page just shows the pickers
		body := e.pageOK(c, "/dashboard/reports")
		has(t, body, `name="month"`, `name="entity"`, ent.GSTIN)
		lacks(t, body, "/dashboard/reports/zip")

		q := "entity=" + ent.ID + "&month=2027-01"
		body = e.pageOK(c, "/dashboard/reports?"+q)
		for _, f := range set.Files {
			has(t, body, f.Name, "name="+f.Name)
		}
		has(t, body, "/dashboard/reports/zip?"+strings.ReplaceAll(q, "&", "&amp;"))

		f := set.Files[0]
		r, got := e.get(c, "/dashboard/reports/file?"+q+"&name="+f.Name)
		if r.StatusCode != 200 || got != string(f.CSV) || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/csv") ||
			!strings.Contains(r.Header.Get("Content-Disposition"), f.Name) {
			t.Fatalf("file: %d %q", r.StatusCode, r.Header.Get("Content-Type"))
		}
		r, z := e.get(c, "/dashboard/reports/zip?"+q)
		if r.StatusCode != 200 || r.Header.Get("Content-Type") != "application/zip" {
			t.Fatalf("zip: %d %q", r.StatusCode, r.Header.Get("Content-Type"))
		}
		zr, err := zip.NewReader(bytes.NewReader([]byte(z)), int64(len(z)))
		if err != nil || len(zr.File) != len(set.Files) {
			t.Fatalf("zip content: %v (%d files)", err, len(zr.File))
		}
		want := map[string]string{}
		for _, f := range set.Files {
			want[f.Name] = string(f.CSV)
		}
		for _, zf := range zr.File {
			rc, _ := zf.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			if string(b) != want[zf.Name] {
				t.Errorf("zip entry %s differs", zf.Name)
			}
		}
		if !strings.Contains(r.Header.Get("Content-Disposition"), "2027-01") {
			t.Errorf("zip name %q", r.Header.Get("Content-Disposition"))
		}

		for name, path := range map[string]string{
			"bad month":      "/dashboard/reports?entity=" + ent.ID + "&month=jan",
			"unknown entity": "/dashboard/reports?entity=te_nope&month=2027-01",
			"zip bad month":  "/dashboard/reports/zip?entity=" + ent.ID + "&month=x",
			"file no entity": "/dashboard/reports/file?month=2027-01&name=" + f.Name,
		} {
			if r, _ := e.get(c, path); r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		if r, _ := e.get(c, "/dashboard/reports/file?"+q+"&name=../../etc/passwd"); r.StatusCode != 404 {
			t.Errorf("unknown file: %d", r.StatusCode)
		}
	})
}

func (e *env) createEntityForm(tok string, over map[string]string) map[string]string {
	f := map[string]string{"legal_name": "Voice Studio Pvt Ltd", "trade_name": "Voicestudio", "gstin": supplierGSTIN, "address": "1 MG Road, Mumbai 400001",
		"invoice_prefix": "VS", "credit_note_prefix": "VSC", "signatory_name": "A. Signatory", "footer_note": "Thank you", "support_email": "help@example.com",
		"turnover_band": "up to 5 crore", "csrf": tok}
	for k, v := range over {
		f[k] = v
	}
	return f
}

func TestTaxEntityCreateEditAndLogo(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		viewer := e.roleClient("viewer")
		vb := e.pageOK(viewer, "/dashboard/tax-entities")
		lacks(t, vb, `action="/dashboard/tax-entities"`)
		if r, _ := e.postMultipart(viewer, "/dashboard/tax-entities", e.createEntityForm(csrfOf(t, vb), nil), "", "", nil); r.StatusCode != 403 {
			t.Fatalf("viewer create: %d", r.StatusCode)
		}

		admin := e.roleClient("admin")
		body := e.pageOK(admin, "/dashboard/tax-entities")
		has(t, body, `action="/dashboard/tax-entities"`)
		tok := csrfOf(t, body)
		logo := pngBytes(t, 16, 16, false)
		over := map[string]string{"lut_number": "AD270226000001A", "lut_from": "2026-04-01", "lut_to": "2027-03-31"}
		r, b := e.postMultipart(admin, "/dashboard/tax-entities", e.createEntityForm(tok, over), "logo", "logo.png", logo)
		if r.StatusCode != 303 {
			t.Fatalf("create: %d\n%s", r.StatusCode, b)
		}
		ent, err := e.inv.FindTaxEntity(ctx, supplierGSTIN)
		if err != nil {
			t.Fatal(err)
		}
		if ent.StateCode != "27" || ent.InvoicePrefix != "VS" || ent.LUTNumber != "AD270226000001A" || ent.LogoBlobID == "" ||
			ent.LUTValidFrom.In(taxIST).Format("2006-01-02") != "2026-04-01" || ent.LUTValidTo.In(taxIST).Format("2006-01-02") != "2027-03-31" {
			t.Fatalf("entity %+v", ent)
		}
		if r.Header.Get("Location") != "/dashboard/tax-entities/"+ent.ID {
			t.Errorf("redirect %s", r.Header.Get("Location"))
		}
		var blob store.Blob
		_ = db.Read(ctx, func(q store.Querier) (err error) { blob, err = store.GetBlob(ctx, q, ent.LogoBlobID); return })
		if blob.ContentType != "image/png" || !bytes.Equal(blob.Data, logo) {
			t.Fatalf("logo blob %+v", blob.ContentType)
		}
		has(t, e.pageOK(admin, "/dashboard/tax-entities"), "Voice Studio Pvt Ltd", supplierGSTIN, "2027-03-31")

		// validation
		for name, o := range map[string]map[string]string{
			"bad gstin":     {"gstin": "27AAPFU0939F1Z0"},
			"duplicate":     {},
			"no legal name": {"legal_name": " "},
			"bad prefix":    {"invoice_prefix": "lowercase"},
			"bad lut date":  {"lut_number": "AD1", "lut_from": "01/04/2026", "lut_to": "2027-03-31"},
			"half a lut":    {"lut_number": "AD1"},
			"reversed lut":  {"lut_number": "AD1", "lut_from": "2027-03-31", "lut_to": "2026-04-01"},
		} {
			if name != "duplicate" {
				o["gstin"] = firstOr(o["gstin"], gstin(t, "29AAPFU0939F1Z"))
			}
			if r, _ := e.postMultipart(admin, "/dashboard/tax-entities", e.createEntityForm(tok, o), "", "", nil); r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		if r, b := e.postMultipart(admin, "/dashboard/tax-entities", e.createEntityForm(tok, map[string]string{"gstin": gstin(t, "29AAPFU0939F1Z")}), "logo", "x.gif", []byte("GIF89a....")); r.StatusCode != 400 || !strings.Contains(b, "PNG or JPEG") {
			t.Errorf("bad logo: %d", r.StatusCode)
		}

		// edit
		page := "/dashboard/tax-entities/" + ent.ID
		body = e.pageOK(admin, page)
		has(t, body, `action="`+page+`"`, `value="Voicestudio"`, `value="AD270226000001A"`, `value="2027-03-31"`, `value="VS"`, supplierGSTIN, "logo")
		tok = csrfOf(t, body)
		edit := e.createEntityForm(tok, map[string]string{"trade_name": "VS Pay", "address": "New HQ", "lut_number": "", "lut_from": "", "lut_to": "", "invoice_prefix": "VSI"})
		if r, b := e.postMultipart(admin, page, edit, "", "", nil); r.StatusCode != 303 {
			t.Fatalf("edit: %d\n%s", r.StatusCode, b)
		}
		got, _ := e.inv.FindTaxEntity(ctx, ent.ID)
		if got.TradeName != "VS Pay" || got.Address != "New HQ" || got.InvoicePrefix != "VSI" || got.LUTNumber != "" || !got.LUTValidTo.IsZero() || got.LogoBlobID != ent.LogoBlobID {
			t.Fatalf("after edit %+v", got)
		}
		// the logo is removed on request, the GSTIN cannot change
		rm := e.createEntityForm(tok, map[string]string{"remove_logo": "1"})
		if r, _ := e.postMultipart(admin, page, rm, "", "", nil); r.StatusCode != 303 {
			t.Fatalf("remove logo: %d", r.StatusCode)
		}
		if got, _ = e.inv.FindTaxEntity(ctx, ent.ID); got.LogoBlobID != "" {
			t.Error("logo not removed")
		}
		if r, b := e.postMultipart(admin, page, e.createEntityForm(tok, map[string]string{"gstin": gstin(t, "29AAPFU0939F1Z")}), "", "", nil); r.StatusCode != 400 || !strings.Contains(b, "cannot change") {
			t.Errorf("gstin change: %d", r.StatusCode)
		}
		if r, _ := e.get(admin, "/dashboard/tax-entities/te_nope"); r.StatusCode != 404 {
			t.Errorf("unknown: %d", r.StatusCode)
		}
		var aud []store.AuditRow
		for _, act := range []string{"tax_entity.create", "tax_entity.update"} {
			_ = db.Read(ctx, func(q store.Querier) (err error) {
				aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, act, 10)
				return
			})
			if len(aud) == 0 || aud[0].ActorID != "usr_admin" {
				t.Errorf("%s audit: %+v", act, aud)
			}
		}
	})
}

func firstOr(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

var taxIST = time.FixedZone("IST", 5*3600+1800)

func TestLUTExpiryBanner(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db) // the clock is 2027-01-15
		mk := func(gst, lutTo string) store.TaxEntity {
			to, _ := time.ParseInLocation("2006-01-02", lutTo, taxIST)
			ent, err := e.inv.AddTaxEntity(ctx, audit.System, invoice.TaxEntityInput{LegalName: "L " + gst, GSTIN: gst, LUTNumber: "AD" + lutTo,
				LUTFrom: to.AddDate(-1, 0, 0), LUTTo: to})
			if err != nil {
				t.Fatal(err)
			}
			return ent
		}
		c := e.roleClient("viewer")
		mk(supplierGSTIN, "2027-03-31") // 75 days away
		body := e.pageOK(c, "/dashboard/tax-entities")
		lacks(t, body, "banner-warning")
		lacks(t, e.pageOK(c, "/dashboard/overview"), "banner-warning")

		soon := mk(gstin(t, "29AAPFU0939F1Z"), "2027-02-10") // 26 days away
		for _, path := range []string{"/dashboard/tax-entities", "/dashboard/overview"} {
			body = e.pageOK(c, path)
			has(t, body, "banner-warning", "LUT", "AD2027-02-10", "expires")
		}
		_ = soon
		gone := mk(gstin(t, "07AAPFU0939F1Z"), "2026-12-31") // expired
		body = e.pageOK(c, "/dashboard/tax-entities")
		has(t, body, "AD2026-12-31", "expired")
		_ = gone
		// an entity without a LUT raises nothing
		if _, err := e.inv.AddTaxEntity(ctx, audit.System, invoice.TaxEntityInput{LegalName: "No LUT", GSTIN: gstin(t, "33AAPFU0939F1Z")}); err != nil {
			t.Fatal(err)
		}
		lacks(t, e.pageOK(c, "/dashboard/tax-entities"), "No LUT</strong> LUT")
	})
}

func TestAppTaxProfileTab(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		ent, err := e.inv.AddTaxEntity(ctx, audit.System, invoice.TaxEntityInput{LegalName: "Voice Studio", GSTIN: supplierGSTIN})
		if err != nil {
			t.Fatal(err)
		}
		admin := e.roleClient("admin")
		page := "/dashboard/apps/" + app.ID
		body := e.pageOK(admin, page+"?tab=tax")
		has(t, body, `name="tax_entity_id"`, ent.ID, `name="sac_code"`, `name="gst_rate"`, `name="issue_invoices"`, `name="prices_include_tax"`)
		tok := csrfOf(t, body)
		form := url.Values{"tax_entity_id": {ent.ID}, "issue_invoices": {"1"}, "sac_code": {"998314"}, "gst_rate": {"5"}, "prices_include_tax": {"1"},
			"invoice_prefix": {"VS"}, "credit_note_prefix": {"VSC"}, "csrf": {tok}}
		if r, b := e.post(admin, page+"/tax", form); r.StatusCode != 303 {
			t.Fatalf("save: %d\n%s", r.StatusCode, b)
		}
		var prof store.AppTaxProfile
		_ = db.Read(ctx, func(q store.Querier) (err error) { prof, err = store.GetAppTaxProfile(ctx, q, app.ID); return })
		if prof.TaxEntityID != ent.ID || !prof.IssueInvoices || prof.SACCode != "998314" || prof.GSTRateBP != 500 || !prof.PricesIncludeTax ||
			prof.InvoicePrefix != "VS" || prof.CreditNotePrefix != "VSC" {
			t.Fatalf("profile %+v", prof)
		}
		body = e.pageOK(admin, page+"?tab=tax")
		has(t, body, `value="998314"`, `value="5"`, `name="issue_invoices" value="1" checked`)

		// issuing off, tax-exclusive, fractional rate
		form.Del("issue_invoices")
		form.Del("prices_include_tax")
		form.Set("gst_rate", "12.5")
		if r, _ := e.post(admin, page+"/tax", form); r.StatusCode != 303 {
			t.Fatalf("second save: %d", r.StatusCode)
		}
		_ = db.Read(ctx, func(q store.Querier) (err error) { prof, err = store.GetAppTaxProfile(ctx, q, app.ID); return })
		if prof.IssueInvoices || prof.PricesIncludeTax || prof.GSTRateBP != 1250 {
			t.Fatalf("profile %+v", prof)
		}

		for name, o := range map[string]map[string]string{
			"bad sac":        {"sac_code": "12"},
			"rate too high":  {"gst_rate": "40"},
			"rate not a num": {"gst_rate": "eighteen"},
			"unknown entity": {"tax_entity_id": "te_nope"},
			"bad prefix":     {"invoice_prefix": "waytoolongprefix"},
		} {
			f := url.Values{}
			for k, v := range form {
				f[k] = v
			}
			for k, v := range o {
				f.Set(k, v)
			}
			if r, _ := e.post(admin, page+"/tax", f); r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		viewer := e.roleClient("viewer")
		vb := e.pageOK(viewer, page+"?tab=tax")
		lacks(t, vb, `action="`+page+`/tax"`)
		has(t, vb, "998314")
		form.Set("csrf", csrfOf(t, vb))
		if r, _ := e.post(viewer, page+"/tax", form); r.StatusCode != 403 {
			t.Errorf("viewer: %d", r.StatusCode)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "app_tax_profile.set", 10)
			return
		})
		if len(aud) != 2 || aud[0].ActorID != "usr_admin" {
			t.Errorf("audit %+v", aud)
		}
	})
}

func TestFXRatesPage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, _ := e.taxApp("alpha", supplierGSTIN)
		p := e.pay(app, 1200, "USD", usBuyer) // no rate yet: fx_pending
		inv := e.invoicesOf(p.ID)[0]
		if !inv.HasFlag(invoice.FlagFXPending) {
			t.Fatalf("setup: %+v", inv.Flags)
		}
		viewer := e.roleClient("viewer")
		vb := e.pageOK(viewer, "/dashboard/fx")
		lacks(t, vb, `action="/dashboard/fx"`)
		has(t, vb, "pending")
		if r, _ := e.post(viewer, "/dashboard/fx", url.Values{"currency": {"USD"}, "date": {"2027-01-15"}, "rate": {"83.25"}, "csrf": {csrfOf(t, vb)}}); r.StatusCode != 403 {
			t.Fatalf("viewer: %d", r.StatusCode)
		}

		admin := e.roleClient("admin")
		body := e.pageOK(admin, "/dashboard/fx")
		has(t, body, `action="/dashboard/fx"`, `name="currency"`, `name="date"`, `name="rate"`, `value="2027-01-15"`) // today, in IST
		tok := csrfOf(t, body)
		if r, b := e.post(admin, "/dashboard/fx", url.Values{"currency": {"usd"}, "date": {"2027-01-15"}, "rate": {"83.2500"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("save: %d\n%s", r.StatusCode, b)
		}
		var rates []store.FXRate
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			rates, err = store.ListFXRates(ctx, q, store.DefaultOrgID, 10)
			return
		})
		if len(rates) != 1 || rates[0].Currency != "USD" || rates[0].Rate != "83.2500" || rates[0].Source != "manual" {
			t.Fatalf("rates %+v", rates)
		}
		has(t, e.pageOK(admin, "/dashboard/fx"), "USD", "2027-01-15", "83.2500", "manual")
		// the invoice that was waiting for it is released and re-rendered
		if inv = e.invoicesOf(p.ID)[0]; inv.HasFlag(invoice.FlagFXPending) || inv.FXRate != "83.2500" {
			t.Fatalf("pending invoice not released: %+v", inv)
		}
		for name, f := range map[string]url.Values{
			"home currency": {"currency": {"INR"}, "date": {"2027-01-15"}, "rate": {"1"}},
			"bad date":      {"currency": {"USD"}, "date": {"15/01/2027"}, "rate": {"83"}},
			"bad rate":      {"currency": {"USD"}, "date": {"2027-01-15"}, "rate": {"-1"}},
			"no currency":   {"date": {"2027-01-15"}, "rate": {"83"}},
		} {
			f.Set("csrf", tok)
			if r, _ := e.post(admin, "/dashboard/fx", f); r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "fx_rate.set", 10)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "usr_admin" {
			t.Errorf("audit %+v", aud)
		}
	})
}
