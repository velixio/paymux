package payu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// PayU's Chargeback API (docs.payu.in, API Reference: Chargeback), built from the
// documentation: a separate token from the Chargeback dashboard sent as
// X-Optimus-API-Key, GET /api/v1/chargebacks (at most 20 days per request),
// GET /api/v1/chargebacks/{id}, and PATCH /api/v1/chargebacks/dispute to accept
// or contest. The test account had no chargeback token.

const chargebackJSON = `{"id":"1295751","type":"chargebacks","attributes":{"id":1295751,"chargeback-amount":"1.0","chargeback-type":"CB","chargeback-date":"20-Feb-2023",
 "status":"Pending Response","reply-before":"24-Feb-2023","chargeback-reason":"Non Receipt of Goods or Services","bank-case-no":"30008130343023154997151",
 "debit-date":"24-Jul-2023","comments":null,"debit-status":"Chargeback Debited","credit-date":"NA","customer-dispute-docs":[],
 "transaction-details":{"payu-id":"16652223102","transaction-id":"c0bb0c2107f53e791cb4","transaction-date":"23-Jan-2023","transaction-amount":"1.0"}}}`

type cbFake struct {
	mu     sync.Mutex
	calls  []string
	query  map[string]string
	bodies map[string][]byte
	hdr    map[string]http.Header
}

func (f *cbFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.query == nil {
		f.query, f.bodies, f.hdr = map[string]string{}, map[string][]byte{}, map[string]http.Header{}
	}
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	f.query[key], f.bodies[key], f.hdr[key] = r.URL.RawQuery, b, r.Header.Clone()
	if r.Header.Get("X-Optimus-API-Key") != "cbtoken" {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"errors":[{"detail":"invalid token"}]}`))
		return
	}
	switch key {
	case "GET /api/v1/chargebacks":
		_, _ = w.Write([]byte(`{"data":[` + chargebackJSON + `],"meta":{"pagination":{"per-page":500,"total-chargebacks":1,"total-pages":1}}}`))
	case "GET /api/v1/chargebacks/1295751":
		_, _ = w.Write([]byte(`{"data":` + chargebackJSON + `}`))
	case "PATCH /api/v1/chargebacks/dispute":
		_, _ = w.Write([]byte(`{"data":{"id":"1295751","type":"chargeback-details","attributes":{"id":1295751,"payu-id":"16652223102","status":"Pending Doc Review"}}}`))
	default:
		w.WriteHeader(404)
	}
}

const cbConfig = `{"key":"testkey1","salt":"testsalt1","test_mode":true,"chargeback_token":"cbtoken","chargeback_merchant_id":"143419"}`

func cbConn(t *testing.T, f http.Handler, cfg string) connector.Connector {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(json.RawMessage(cfg), connectortest.NewDeps(srv, time.Unix(1760000000, 0)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChargebackReader(t *testing.T) {
	f := &cbFake{}
	c := cbConn(t, f, cbConfig)
	if !c.Info().Has(connector.CapDisputeRead) || !c.Info().Has(connector.CapDisputeRespond) {
		t.Fatal("payu declares dispute_read and dispute_respond")
	}
	dr := c.(connector.DisputeReader)
	from := time.Date(2023, 2, 20, 0, 0, 0, 0, ist)
	pg, err := dr.ListDisputes(context.Background(), connector.TimeRange{From: from.UnixMilli(), To: from.Add(24 * time.Hour).UnixMilli()}, "")
	if err != nil || len(pg.Items) != 1 || pg.Next != "" {
		t.Fatalf("%+v %v", pg, err)
	}
	d := pg.Items[0]
	if d.ProviderDisputeID != "1295751" || d.ProviderPaymentID != "16652223102" || d.Status != connector.DisputeNeedsResponse || d.Phase != "chargeback" ||
		d.Amount != 100 || d.Currency != "INR" || d.Reason != "Non Receipt of Goods or Services" ||
		d.DueBy != time.Date(2023, 2, 24, 0, 0, 0, 0, ist).UnixMilli() || d.CreatedAt != time.Date(2023, 2, 20, 0, 0, 0, 0, ist).UnixMilli() {
		t.Errorf("%+v", d)
	}
	q := f.query["GET /api/v1/chargebacks"]
	if !strings.Contains(q, "merchant_id=143419") || !strings.Contains(q, "from_date=20-02-2023") || !strings.Contains(q, "to_date=20-02-2023") {
		t.Errorf("query = %s", q)
	}
	got, err := dr.FetchDispute(context.Background(), connector.DisputeRef{ProviderDisputeID: "1295751"})
	if err != nil || got.ProviderDisputeID != "1295751" {
		t.Errorf("%+v %v", got, err)
	}
	if _, err := dr.FetchDispute(context.Background(), connector.DisputeRef{ProviderDisputeID: "999"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
}

// The API reads at most 20 days at a time: a longer range comes in pages, each
// the next window.
func TestChargebackListWindows(t *testing.T) {
	f := &cbFake{}
	c := cbConn(t, f, cbConfig).(connector.DisputeReader)
	from := time.Date(2023, 1, 1, 0, 0, 0, 0, ist)
	to := from.AddDate(0, 0, 50)
	var pages int
	cur := connector.Cursor("")
	for {
		pg, err := c.ListDisputes(context.Background(), connector.TimeRange{From: from.UnixMilli(), To: to.UnixMilli()}, cur)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if pg.Next == "" || pages > 5 {
			break
		}
		cur = pg.Next
	}
	if pages != 3 { // 20 + 20 + 10 days
		t.Errorf("pages = %d", pages)
	}
}

func TestChargebackStatusMapping(t *testing.T) {
	for raw, want := range map[string]connector.DisputeStatus{
		"Pending Response": connector.DisputeNeedsResponse, "Insufficient Document": connector.DisputeNeedsResponse,
		"Pending Doc Review": connector.DisputeUnderReview, "Submitted to bank": connector.DisputeUnderReview,
		"Closed in merchant favour": connector.DisputeWon, "Closed Customer Favour": connector.DisputeLost, "Closed under fraud": connector.DisputeLost, "Something new": connector.DisputeUnderReview,
	} {
		if got := mapChargebackStatus(raw); got != want {
			t.Errorf("%s -> %s want %s", raw, got, want)
		}
	}
}

func TestChargebackAcceptAndContest(t *testing.T) {
	f := &cbFake{}
	c := cbConn(t, f, cbConfig).(connector.DisputeResponder)
	if err := c.AcceptDispute(context.Background(), connector.DisputeRef{ProviderDisputeID: "1295751"}); err != nil {
		t.Fatal(err)
	}
	var b struct {
		ID, Key, Type string
		Reasons       []struct {
			UUID string `json:"uuid"`
			Form []struct {
				Identifier, Value, Filename, ContentType string
			} `json:"form_data"`
		} `json:"reason_code"`
	}
	raw := f.bodies["PATCH /api/v1/chargebacks/dispute"]
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	_ = json.Unmarshal(raw, &b)
	if generic["chargeback_id"] != "1295751" || generic["merchant_key"] != "testkey1" || generic["dispute_type"] != "accept" || len(b.Reasons) != 1 || b.Reasons[0].UUID == "" {
		t.Errorf("accept body = %s", raw)
	}
	if ct := f.hdr["PATCH /api/v1/chargebacks/dispute"].Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q (the unified call takes JSON, files base64-encoded, not multipart)", ct)
	}
	pdf := []byte("%PDF-1.4 evidence")
	if err := c.SubmitEvidence(context.Background(), connector.DisputeRef{ProviderDisputeID: "1295751"}, connector.Evidence{Text: "Delivered on 12 Feb.", Files: map[string][]byte{"proof_of_delivery": pdf}}); err != nil {
		t.Fatal(err)
	}
	raw = f.bodies["PATCH /api/v1/chargebacks/dispute"]
	_ = json.Unmarshal(raw, &generic)
	if generic["dispute_type"] != "contest" {
		t.Fatalf("contest body = %s", raw)
	}
	var file string
	var withText bool
	for _, r := range generic["reason_code"].([]any) {
		for _, fd := range r.(map[string]any)["form_data"].([]any) {
			m := fd.(map[string]any)
			if m["value"] == "Delivered on 12 Feb." {
				withText = true
			}
			if m["filename"] != nil {
				file = m["value"].(string)
				if m["content_type"] != "application/pdf" || !strings.HasSuffix(m["filename"].(string), ".pdf") {
					t.Errorf("file field = %v", m)
				}
			}
		}
	}
	if dec, _ := base64.StdEncoding.DecodeString(file); string(dec) != string(pdf) || !withText {
		t.Errorf("evidence not in the body: file=%q text=%v body=%s", file, withText, raw)
	}
	// One file at most (5 MB), and a text or a file is needed.
	before := len(f.calls)
	for _, e := range []connector.Evidence{{}, {Files: map[string][]byte{"a": pdf, "b": pdf}}, {Files: map[string][]byte{"a": make([]byte, 6<<20)}}} {
		if err := c.SubmitEvidence(context.Background(), connector.DisputeRef{ProviderDisputeID: "1295751"}, e); err == nil {
			t.Errorf("accepted %d files", len(e.Files))
		}
	}
	if len(f.calls) != before {
		t.Errorf("requests for refused evidence")
	}
}

// Without the chargeback token (a separate credential from the dashboard) the
// calls say what is missing instead of failing obscurely.
func TestChargebackNeedsItsOwnToken(t *testing.T) {
	c := cbConn(t, &cbFake{}, `{"key":"k","salt":"s","test_mode":true}`).(connector.DisputeReader)
	_, err := c.ListDisputes(context.Background(), connector.TimeRange{}, "")
	if !errors.Is(err, connector.ErrInvalidCredentials) || !strings.Contains(err.Error(), "chargeback token") {
		t.Errorf("%v", err)
	}
	bad := cbConn(t, &cbFake{}, `{"key":"k","salt":"s","test_mode":true,"chargeback_token":"wrong","chargeback_merchant_id":"1"}`).(connector.DisputeReader)
	if _, err := bad.FetchDispute(context.Background(), connector.DisputeRef{ProviderDisputeID: "1295751"}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("a refused token: %v", err)
	}
}
