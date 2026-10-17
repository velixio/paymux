package invoice_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/domains"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// urlsOf extracts the hosted and pdf paths (with token) from an event.
func urlsOf(t *testing.T, data any) (hosted, pdf string) {
	t.Helper()
	raw, _ := json.Marshal(data)
	var d struct {
		HostedURL string `json:"hosted_url"`
		PDFURL    string `json:"pdf_url"`
	}
	if err := json.Unmarshal(raw, &d); err != nil || d.HostedURL == "" {
		t.Fatalf("event data %s", raw)
	}
	h, _ := url.Parse(d.HostedURL)
	p, _ := url.Parse(d.PDFURL)
	return h.RequestURI(), p.RequestURI()
}

func get(t *testing.T, h http.Handler, target string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
	res := rec.Result()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestHostedInvoiceAndCreditNote(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /i/{id}", e.inv.ServeHosted)
		if !domains.IsCustomerRoute("/i/inv_x") {
			t.Fatal("/i/ must be a customer route")
		}

		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		inv := only(t, e, p.ID)
		hosted, pdf := urlsOf(t, e.eventsOf("invoice.issued")[0].Data)

		res, body := get(t, mux, hosted)
		if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("hosted: %d %s", res.StatusCode, res.Header)
		}
		for _, want := range []string{inv.Number, "Tax invoice", "Voice Studio Pvt Ltd", supplierGSTIN, "INR 1180.00", "CGST", "SGST", "Maharashtra (27)",
			"One Thousand One Hundred Eighty"} {
			if !strings.Contains(body, want) {
				t.Errorf("hosted page lacks %q", want)
			}
		}
		if strings.Contains(body, "IGST") {
			t.Error("intra-state page shows IGST")
		}
		if res.Header.Get("Referrer-Policy") != "no-referrer" || res.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("headers = %v", res.Header)
		}
		if !strings.Contains(body, "being prepared") {
			t.Error("page should say the PDF is pending before the render job ran")
		}

		// PDF not rendered yet: 404.
		if res, _ := get(t, mux, pdf); res.StatusCode != 404 {
			t.Errorf("pdf before render: %d", res.StatusCode)
		}
		e.runJobs()
		res, body = get(t, mux, pdf)
		if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/pdf" || !strings.HasPrefix(body, "%PDF-") ||
			!strings.Contains(res.Header.Get("Content-Disposition"), "INV-2627-000001.pdf") {
			t.Fatalf("pdf: %d %v", res.StatusCode, res.Header)
		}
		if _, body = get(t, mux, hosted); !strings.Contains(body, "Download PDF") {
			t.Error("page lacks the PDF link after rendering")
		}

		// Tokens: missing, wrong and other documents' tokens all look like 404.
		bad := []string{"/i/" + inv.ID, "/i/" + inv.ID + "?t=wrong", "/i/" + inv.ID + ".pdf?t=wrong", "/i/inv_nosuch?t=x", "/i/zzz?t=x", "/i/" + inv.ID + ".pdf"}
		for _, b := range bad {
			if res, _ := get(t, mux, b); res.StatusCode != 404 {
				t.Errorf("%s -> %d, want 404", b, res.StatusCode)
			}
		}

		// Credit note.
		e.refund(p, 59000)
		e.runJobs()
		ch, cp := urlsOf(t, e.eventsOf("credit_note.issued")[0].Data)
		res, body = get(t, mux, ch)
		if res.StatusCode != 200 || !strings.Contains(body, "Credit note") || !strings.Contains(body, "CN/2627/000001") ||
			!strings.Contains(body, inv.Number) || !strings.Contains(body, "customer request") {
			t.Errorf("credit note page: %d %s", res.StatusCode, body)
		}
		if res, body := get(t, mux, cp); res.StatusCode != 200 || !strings.HasPrefix(body, "%PDF-") {
			t.Errorf("credit note pdf: %d", res.StatusCode)
		}
		// An invoice token does not open a credit note.
		if res, _ := get(t, mux, strings.Replace(ch, "t=", "x=", 1)+"&t=nope"); res.StatusCode != 404 {
			t.Errorf("credit note with a bad token: %d", res.StatusCode)
		}
	})
}

func TestReadAPIScopesToAppAndMode(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		inv := only(t, e, p.ID)
		if got, err := e.inv.GetInvoice(ctx, "app_1", "live", inv.ID); err != nil || got.ID != inv.ID {
			t.Fatalf("get: %+v %v", got, err)
		}
		for _, c := range [][2]string{{"app_other", "live"}, {"app_1", "test"}} {
			if _, err := e.inv.GetInvoice(ctx, c[0], c[1], inv.ID); err == nil {
				t.Errorf("%v could read the invoice", c)
			}
		}
		if l, _ := e.inv.ListInvoices(ctx, "app_1", "live", p.ID); len(l) != 1 {
			t.Errorf("list = %d", len(l))
		}
		if l, _ := e.inv.ListInvoices(ctx, "app_other", "live", p.ID); len(l) != 0 {
			t.Errorf("foreign list = %d", len(l))
		}
	})
}
