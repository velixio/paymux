package razorpay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	testKeyID         = "rzp_test_key"
	testKeySecret     = "test_key_secret_123"
	testWebhookSecret = "whsec_test_known"
)

// call is one request the fake received.
type call struct {
	Method, Path, Query string
	Header              http.Header
	Body                []byte
}

// fake is an in-memory Razorpay API.
type fake struct {
	mu    sync.Mutex
	calls []call

	payStatus       string // status of payment pay_Nx1
	refundStatus    string
	captureFails    bool // capture answers "already captured" 400
	captureOmitsUPI bool
	captureCard     bool
	cardExpandFails bool
	refundErrCode   int // when set, refund answers this status with an error body
	listPages       int // number of full pages to serve for lists
	plans           int // plans created
	subStatus       string
	settlements     []map[string]any // GET /v1/settlements
	documents       int              // documents uploaded
	docFail         int              // when set, POST /v1/documents answers this status
	acceptErr       int              // when set, dispute accept answers this status
	contestErr      int              // when set, dispute contest answers this status
	patchSubErr     string           // when set, PATCH of the subscription answers 400 with this description
	recon           map[string][]any // GET /v1/settlements/recon/combined, keyed "Y-M-D"
	listReceipt     string           // when set, GET /v1/orders ignores the receipt filter and returns this receipt
	orderPayments   []map[string]any // when set, payments returned for order_Nx0
}

func newFake(status string) *fake { return &fake{payStatus: status, refundStatus: "processed"} }

func (f *fake) record(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), b})
	return b
}

func (f *fake) callsTo(method, path string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method == method && c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

func payJSON(status string) map[string]any {
	return map[string]any{
		"id": "pay_Nx1", "entity": "payment", "amount": 49900, "currency": "INR", "status": status,
		"order_id": "order_Nx0", "method": "upi", "amount_refunded": 0, "fee": 1178, "tax": 180,
		"upi":        map[string]any{"payer_account_type": "bank_account"},
		"error_code": nil, "error_description": nil, "created_at": 1760000000,
	}
}

func refundJSON(status string) map[string]any {
	return map[string]any{
		"id": "rfnd_Nx2", "entity": "refund", "amount": 10000, "currency": "INR", "payment_id": "pay_Nx1",
		"receipt": "rf_01", "status": status, "created_at": 1760000200,
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, code int, desc string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": "BAD_REQUEST_ERROR", "description": desc}})
}

func fakeCollection(items ...any) map[string]any {
	return map[string]any{"entity": "collection", "count": len(items), "items": items}
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	if u, p, ok := r.BasicAuth(); !ok || u != testKeyID || p != testKeySecret {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"code": "BAD_REQUEST_ERROR", "description": "Authentication failed"}})
		return
	}
	p := r.URL.Path
	switch {
	case r.Method == "POST" && p == "/v1/orders":
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		in["id"], in["status"], in["entity"] = "order_Nx0", "created", "order"
		writeJSON(w, 200, in)
	case r.Method == "GET" && p == "/v1/orders":
		receipt := r.URL.Query().Get("receipt")
		if f.listReceipt != "" {
			receipt = f.listReceipt
		}
		writeJSON(w, 200, fakeCollection(map[string]any{"id": "order_Nx0", "receipt": receipt, "amount": 49900, "currency": "INR", "status": "attempted"}))
	case r.Method == "GET" && p == "/v1/orders/order_Nx0":
		st := "attempted"
		if f.payStatus == "" {
			st = "created"
		}
		writeJSON(w, 200, map[string]any{"id": "order_Nx0", "amount": 49900, "currency": "INR", "status": st, "receipt": "pay_ref_01"})
	case r.Method == "GET" && p == "/v1/orders/order_Nx0/payments":
		if f.orderPayments != nil {
			items := make([]any, len(f.orderPayments))
			for i := range f.orderPayments {
				items[i] = f.orderPayments[i]
			}
			writeJSON(w, 200, fakeCollection(items...))
			return
		}
		if f.payStatus == "" {
			writeJSON(w, 200, fakeCollection())
			return
		}
		writeJSON(w, 200, fakeCollection(payJSON(f.payStatus)))
	case r.Method == "GET" && p == "/v1/payments/pay_Nx1":
		if f.cardExpandFails && r.URL.Query().Get("expand[]") == "card" {
			writeAPIError(w, 500, "card expansion unavailable")
			return
		}
		writeJSON(w, 200, payJSON(f.payStatus))
	case r.Method == "POST" && p == "/v1/payments/pay_Nx1/capture":
		if f.captureFails {
			writeAPIError(w, 400, "This payment has already been captured")
			return
		}
		f.mu.Lock()
		f.payStatus = "captured"
		f.mu.Unlock()
		captured := payJSON("captured")
		if f.captureOmitsUPI {
			delete(captured, "upi")
		}
		if f.captureCard {
			captured["method"] = "card"
		}
		writeJSON(w, 200, captured)
	case r.Method == "POST" && p == "/v1/payments/pay_Nx1/refund":
		if f.refundErrCode != 0 {
			writeAPIError(w, f.refundErrCode, "The refund amount provided is greater than amount captured")
			return
		}
		writeJSON(w, 200, refundJSON(f.refundStatus))
	case r.Method == "GET" && p == "/v1/payments/pay_Nx1/refunds":
		writeJSON(w, 200, fakeCollection(refundJSON(f.refundStatus)))
	case r.Method == "GET" && p == "/v1/refunds/rfnd_Nx2":
		writeJSON(w, 200, refundJSON(f.refundStatus))
	case r.Method == "GET" && (p == "/v1/payments" || p == "/v1/refunds"):
		skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))
		count, _ := strconv.Atoi(r.URL.Query().Get("count"))
		n := count
		if skip/count >= f.listPages {
			n = 1 // last, short page
		}
		items := make([]any, 0, n)
		for i := 0; i < n; i++ {
			if p == "/v1/payments" {
				items = append(items, payJSON("captured"))
			} else {
				items = append(items, refundJSON("processed"))
			}
		}
		writeJSON(w, 200, fakeCollection(items...))
	case r.Method == "GET" && p == "/v1/settlements":
		q := r.URL.Query()
		from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
		to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
		var items []any
		for _, s := range f.settlements {
			if at := s["created_at"].(int64); at >= from && at <= to {
				items = append(items, s)
			}
		}
		skip, _ := strconv.Atoi(q.Get("skip"))
		count, _ := strconv.Atoi(q.Get("count"))
		if skip > len(items) {
			skip = len(items)
		}
		items = items[skip:min(skip+count, len(items))]
		writeJSON(w, 200, fakeCollection(items...))
	case r.Method == "GET" && p == "/v1/settlements/recon/combined":
		q := r.URL.Query()
		items := f.recon[q.Get("year")+"-"+q.Get("month")+"-"+q.Get("day")]
		skip, _ := strconv.Atoi(q.Get("skip"))
		count, _ := strconv.Atoi(q.Get("count"))
		if skip > len(items) {
			skip = len(items)
		}
		writeJSON(w, 200, fakeCollection(items[skip:min(skip+count, len(items))]...))
	case r.Method == "POST" && p == "/v1/documents":
		if f.docFail != 0 {
			writeAPIError(w, f.docFail, "The file type is not supported")
			return
		}
		f.mu.Lock()
		f.documents++
		id := "doc_Nx" + strconv.Itoa(f.documents)
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"id": id, "entity": "document", "purpose": "dispute_evidence"})
	case r.Method == "POST" && p == "/v1/disputes/disp_Nx3/accept":
		if f.acceptErr != 0 {
			writeAPIError(w, f.acceptErr, "The dispute cannot be accepted in its current state")
			return
		}
		writeJSON(w, 200, disputeJSON("lost"))
	case r.Method == "PATCH" && p == "/v1/disputes/disp_Nx3/contest":
		if f.contestErr != 0 {
			writeAPIError(w, f.contestErr, "The response period has ended")
			return
		}
		writeJSON(w, 200, disputeJSON("under_review"))
	case r.Method == "GET" && p == "/v1/disputes":
		writeJSON(w, 200, fakeCollection(disputeJSON("open")))
	case r.Method == "GET" && p == "/v1/disputes/disp_Nx3":
		writeJSON(w, 200, disputeJSON("under_review"))
	case r.Method == "POST" && p == "/v1/plans":
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		f.mu.Lock()
		f.plans++
		id := "plan_Nx" + strconv.Itoa(f.plans)
		f.mu.Unlock()
		in["id"], in["entity"] = id, "plan"
		writeJSON(w, 200, in)
	case r.Method == "GET" && strings.HasPrefix(p, "/v1/plans/"):
		writeJSON(w, 200, map[string]any{"id": strings.TrimPrefix(p, "/v1/plans/"), "entity": "plan", "period": "monthly", "interval": 1,
			"item": map[string]any{"amount": 49900, "currency": "INR"}})
	case r.Method == "POST" && p == "/v1/subscriptions":
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		in["id"], in["entity"], in["status"] = "sub_Nx5", "subscription", "created"
		writeJSON(w, 200, in)
	case r.Method == "GET" && p == "/v1/subscriptions/sub_Nx5":
		writeJSON(w, 200, map[string]any{"id": "sub_Nx5", "entity": "subscription", "plan_id": "plan_Nx4", "status": f.subStatus,
			"current_start": 1760000000, "current_end": 1762592000, "total_count": 12, "paid_count": 1})
	case r.Method == "POST" && p == "/v1/subscriptions/sub_Nx5/cancel":
		writeJSON(w, 200, map[string]any{"id": "sub_Nx5", "entity": "subscription", "status": "cancelled"})
	case r.Method == "PATCH" && p == "/v1/subscriptions/sub_Nx5" && f.patchSubErr != "":
		writeAPIError(w, 400, f.patchSubErr)
	case r.Method == "PATCH" && p == "/v1/subscriptions/sub_Nx5":
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		in["id"] = "sub_Nx5"
		writeJSON(w, 200, in)
	case strings.HasPrefix(p, "/v1/missing"):
		writeAPIError(w, 404, "not found")
	default:
		writeAPIError(w, 404, "The requested URL was not found on the server")
	}
}

func disputeJSON(status string) map[string]any {
	return map[string]any{
		"id": "disp_Nx3", "entity": "dispute", "payment_id": "pay_Nx1", "amount": 49900, "currency": "INR",
		"reason_code": "fraudulent", "respond_by": 1761000000, "status": status, "phase": "chargeback", "created_at": 1760000300,
	}
}

func startFake(t *testing.T, f *fake) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}
