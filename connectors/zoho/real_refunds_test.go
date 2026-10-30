package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

const realPaymentID = "28000000000181002"

func refundReq(ref string, amount int64, reason string) connector.RefundRequest {
	return connector.RefundRequest{ProviderPaymentID: realPaymentID, ProviderRef: ref, Amount: amount, Currency: "INR", Reason: reason}
}

// The API wants type (initiated_by_merchant) and a reason from a fixed list
// ("Please ensure that the type is valid." / "...the reason is valid."); the
// Paymux reference rides in meta_data, since reference_number is not a request field.
func TestRealRefundBody(t *testing.T) {
	f := newRFake()
	f.on("GET /api/v1/payments/"+realPaymentID, 200, raw(t, "real_payment")) // one refund, rf_ref_01
	f.on("POST /api/v1/payments/"+realPaymentID+"/refunds", 201, raw(t, "real_refund"))
	c := f.conn(t, widgetCfg).(connector.Refunder)
	for reason, want := range map[string]string{"requested_by_customer": "requested_by_customer", "duplicate": "duplicate", "fraudulent": "fraudulent", "customer wants out": "others", "": "others"} {
		if _, err := c.Refund(context.Background(), refundReq("rf_new_"+want, 500, reason)); err != nil {
			t.Fatal(err)
		}
		var body struct {
			Amount json.Number `json:"amount"`
			Reason string      `json:"reason"`
			Type   string      `json:"type"`
			Desc   string      `json:"description"`
			Meta   []metaItem  `json:"meta_data"`
		}
		_ = json.Unmarshal(f.bodies["POST /api/v1/payments/"+realPaymentID+"/refunds"], &body)
		if body.Reason != want || body.Type != "initiated_by_merchant" || body.Amount.String() != "5.00" || len(body.Meta) != 1 || body.Meta[0].Key != "paymux_ref" || body.Meta[0].Value != "rf_new_"+want {
			t.Errorf("reason %q: %s", reason, f.bodies["POST /api/v1/payments/"+realPaymentID+"/refunds"])
		}
		if reason != "" && body.Desc != reason {
			t.Errorf("the reason text is the description: %q", body.Desc)
		}
	}
}

// Zoho documents no idempotency for refunds. The payment lists its refunds with
// their meta_data, so a retried refund is found there and not made twice.
func TestRealRefundRetryFindsTheExistingRefund(t *testing.T) {
	f := newRFake()
	f.on("GET /api/v1/payments/"+realPaymentID, 200, raw(t, "real_payment"))
	// No POST route: a second refund would fail the test with a 404.
	c := f.conn(t, widgetCfg).(connector.Refunder)
	s, err := c.Refund(context.Background(), refundReq("rf_ref_01", 1000, "others"))
	if err != nil || s.ProviderRefundID != "28000000000184001" || s.Amount != 1000 || s.ProviderRef != "rf_ref_01" || s.Status != connector.RefundSucceeded {
		t.Fatalf("%+v %v", s, err)
	}
	for _, c := range f.calls {
		if c == "POST /api/v1/payments/"+realPaymentID+"/refunds" {
			t.Fatal("a refund was made twice")
		}
	}
	// The same reference with another amount is a conflict, not an adoption.
	var nr connector.ErrNotRefundable
	if _, err := c.Refund(context.Background(), refundReq("rf_ref_01", 5000, "others")); err == nil || errors.As(err, &nr) {
		t.Errorf("%v", err)
	}
}

func TestRealRefundErrors(t *testing.T) {
	f := newRFake()
	f.on("GET /api/v1/payments/"+realPaymentID, 200, []byte(`{"code":0,"payment":{"payment_id":"`+realPaymentID+`","refunds":[]}}`))
	f.on("POST /api/v1/payments/"+realPaymentID+"/refunds", 400, raw(t, "err_refunded"))
	f.on("GET /api/v1/payments/9", 400, raw(t, "err_unknown_payment"))
	c := f.conn(t, widgetCfg).(connector.Refunder)
	var nr connector.ErrNotRefundable
	if _, err := c.Refund(context.Background(), refundReq("r1", 100, "")); !errors.As(err, &nr) || nr.Reason == "" {
		t.Errorf("already refunded: %v", err)
	}
	// A validation error ("Please ensure that the type is valid.") is a bug, not "not refundable".
	f.on("POST /api/v1/payments/"+realPaymentID+"/refunds", 400, raw(t, "err_type_invalid"))
	if _, err := c.Refund(context.Background(), refundReq("r2", 100, "")); err == nil || errors.As(err, &nr) {
		t.Errorf("validation error: %v", err)
	}
	r := refundReq("r3", 100, "")
	r.ProviderPaymentID = "9"
	if _, err := c.Refund(context.Background(), r); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown payment: %v", err)
	}
}

func TestRealFetchRefund(t *testing.T) {
	f := newRFake()
	f.on("GET /api/v1/refunds/28000000000184001", 200, raw(t, "real_refund"))
	s, err := f.conn(t, widgetCfg).(connector.Refunder).FetchRefund(context.Background(), connector.RefundRef{ProviderRefundID: "28000000000184001"})
	if err != nil || s.Status != connector.RefundSucceeded || s.Amount != 1000 || s.ProviderRef != "rf_ref_01" || s.ProviderPaymentID != realPaymentID {
		t.Fatalf("%+v %v", s, err)
	}
}
