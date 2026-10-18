package whop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Whop's dispute API, from its API reference (the sandbox key had no
// payment:dispute:read scope, so it could not be run live):
//   GET  /disputes?account_id=biz_...  (list), GET /disputes/{id}
//   POST /disputes/{id}/update_evidence  (text fields and {id} attachments)
//   POST /disputes/{id}/submit_evidence
// Files are made with POST /files {filename} and PUT to the returned upload_url.
// There is no call to accept a dispute.

const disputeJSON = `{"id":"dspt_1","amount":6.9,"currency":"usd","status":"warning_needs_response","reason":"Product Not Received","reason_code":"10.4",
 "needs_response_by":"2026-10-30T05:00:00.000Z","created_at":"2026-10-17T05:00:00.401Z","editable":true,"visa_rdr":false,
 "payment":{"id":"pay_w1","total":6.9,"currency":"usd"},"plan":{"id":"plan_1"},"product":{"id":"prod_1","title":"P"},"company":{"id":"biz_1","title":"Co"}}`

type disputeFake struct {
	mu     sync.Mutex
	calls  []string
	bodies map[string][]byte
	query  map[string]string
	put    []byte
	putCT  string
	n      int
}

func (f *disputeFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	if f.bodies == nil {
		f.bodies, f.query = map[string][]byte{}, map[string]string{}
	}
	f.bodies[key], f.query[key] = b, r.URL.RawQuery
	if strings.HasPrefix(r.URL.Path, "/upload/") && r.Method == "PUT" {
		f.put, f.putCT = b, r.Header.Get("Content-Type")
		w.WriteHeader(200)
		return
	}
	switch key {
	case "GET /api/v1/disputes":
		_, _ = w.Write([]byte(`{"data":[` + disputeJSON + `],"page_info":{"has_next_page":true,"end_cursor":"cur2"}}`))
	case "GET /api/v1/disputes/dspt_1":
		_, _ = w.Write([]byte(disputeJSON))
	case "POST /api/v1/files":
		f.n++
		_, _ = w.Write([]byte(fmt.Sprintf(`{"object":"file","id":"file_%d","upload_url":"http://%s/upload/%d","upload_headers":{"x-amz-acl":"private"},"upload_status":"pending"}`, f.n, r.Host, f.n)))
	case "POST /api/v1/disputes/dspt_1/update_evidence", "POST /api/v1/disputes/dspt_1/submit_evidence":
		_, _ = w.Write([]byte(disputeJSON))
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"type":"not_found","message":"Dispute not found"}}`))
	}
}

func disputeConn(t *testing.T, h http.Handler) (connector.Connector, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(testConfig, connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestDisputeReader(t *testing.T) {
	f := &disputeFake{}
	c, _ := disputeConn(t, f)
	if !c.Info().Has(connector.CapDisputeRead) || !c.Info().Has(connector.CapDisputeRespond) {
		t.Fatal("whop declares dispute_read and dispute_respond")
	}
	dr := c.(connector.DisputeReader)
	pg, err := dr.ListDisputes(context.Background(), connector.TimeRange{From: 1760000000000, To: 1761000000000}, "")
	if err != nil || len(pg.Items) != 1 || pg.Next != "cur2" {
		t.Fatalf("%+v %v", pg, err)
	}
	d := pg.Items[0]
	if d.ProviderDisputeID != "dspt_1" || d.ProviderPaymentID != "pay_w1" || d.Status != connector.DisputeNeedsResponse || d.Phase != "inquiry" || d.Amount != 690 ||
		d.Currency != "USD" || d.Reason != "Product Not Received" || d.DueBy != time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("%+v", d)
	}
	q := f.query["GET /api/v1/disputes"]
	if !strings.Contains(q, "account_id=biz_1") || !strings.Contains(q, "created_after=") || !strings.Contains(q, "created_before=") {
		t.Errorf("query = %s", q)
	}
	if _, err := dr.ListDisputes(context.Background(), connector.TimeRange{}, "cur2"); err != nil || !strings.Contains(f.query["GET /api/v1/disputes"], "after=cur2") {
		t.Errorf("cursor: %v %s", err, f.query["GET /api/v1/disputes"])
	}
	got, err := dr.FetchDispute(context.Background(), connector.DisputeRef{ProviderDisputeID: "dspt_1"})
	if err != nil || got.ProviderDisputeID != "dspt_1" {
		t.Errorf("%+v %v", got, err)
	}
	if _, err := dr.FetchDispute(context.Background(), connector.DisputeRef{ProviderDisputeID: "dspt_nope"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
}

func TestDisputeStatusMapping(t *testing.T) {
	for raw, want := range map[string]connector.DisputeStatus{
		"warning_needs_response": connector.DisputeNeedsResponse, "needs_response": connector.DisputeNeedsResponse,
		"warning_under_review": connector.DisputeUnderReview, "under_review": connector.DisputeUnderReview,
		"won": connector.DisputeWon, "lost": connector.DisputeLost, "closed": connector.DisputeClosed, "warning_closed": connector.DisputeClosed, "other": connector.DisputeClosed,
	} {
		if got := mapDisputeStatus(raw); got != want || !got.Valid() {
			t.Errorf("%s -> %s want %s", raw, got, want)
		}
	}
}

// Evidence is saved (text fields, attachments by file id) and then submitted. A
// file is made with POST /files, then its bytes are PUT to the upload URL.
func TestSubmitEvidence(t *testing.T) {
	f := &disputeFake{}
	c, _ := disputeConn(t, f)
	dr := c.(connector.DisputeResponder)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	err := dr.SubmitEvidence(context.Background(), connector.DisputeRef{ProviderDisputeID: "dspt_1"}, connector.Evidence{
		Text: "The customer used the product for three months.", Files: map[string][]byte{"refund_policy": png, "receipt": png}})
	if err != nil {
		t.Fatal(err)
	}
	var ev map[string]any
	_ = json.Unmarshal(f.bodies["POST /api/v1/disputes/dspt_1/update_evidence"], &ev)
	if ev["notes"] != "The customer used the product for three months." {
		t.Errorf("update body = %v", ev)
	}
	if a, _ := ev["refund_policy_attachment"].(map[string]any); a["id"] == nil || a["id"] == "" {
		t.Errorf("refund_policy_attachment = %v", ev["refund_policy_attachment"])
	}
	if a, _ := ev["uncategorized_attachment"].(map[string]any); a["id"] == nil || a["id"] == "" {
		t.Errorf("a file of another type goes to uncategorized_attachment: %v", ev)
	}
	last := f.calls[len(f.calls)-1]
	if last != "POST /api/v1/disputes/dspt_1/submit_evidence" {
		t.Errorf("submitted last: %v", f.calls)
	}
	if string(f.put) != string(png) || f.putCT != "image/png" {
		t.Errorf("uploaded %q as %q", f.put, f.putCT)
	}
	// Two files of the one uncategorized slot, nothing at all, and a dispute id
	// that is empty are refused before a request is made.
	before := len(f.calls)
	for _, e := range []connector.Evidence{{}, {Files: map[string][]byte{"a": png, "b": png}}} {
		if err := dr.SubmitEvidence(context.Background(), connector.DisputeRef{ProviderDisputeID: "dspt_1"}, e); err == nil {
			t.Errorf("accepted %+v", e)
		}
	}
	if err := dr.SubmitEvidence(context.Background(), connector.DisputeRef{}, connector.Evidence{Text: "x"}); err == nil {
		t.Error("accepted an empty dispute id")
	}
	if len(f.calls) != before {
		t.Errorf("requests for refused evidence: %v", f.calls[before:])
	}
}

func TestAcceptDisputeIsUnsupported(t *testing.T) {
	c, _ := disputeConn(t, &disputeFake{})
	if err := c.(connector.DisputeResponder).AcceptDispute(context.Background(), connector.DisputeRef{ProviderDisputeID: "dspt_1"}); !errors.Is(err, connector.ErrUnsupported) {
		t.Errorf("%v", err)
	}
}
