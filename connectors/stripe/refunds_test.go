package stripe

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

func refundJSON(id, status, ref string) map[string]any {
	return map[string]any{"id": id, "object": "refund", "status": status, "amount": 10000, "currency": "inr", "payment_intent": "pi_3A1",
		"charge": "ch_3A1", "created": nowUnix - 30, "metadata": map[string]string{"paymux_ref": ref}}
}

func TestRefundUsesTheReferenceAsIdempotencyKey(t *testing.T) {
	f := newFake()
	f.reply("POST /v1/refunds", 200, refundJSON("re_1", "pending", "ref_01"))
	c := newConn(t, f)
	snap, err := c.Refund(bg, connector.RefundRequest{ProviderRef: "ref_01", ProviderPaymentID: "pi_3A1", Amount: 10000, Currency: "INR", Reason: "requested_by_customer"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.ProviderRef != "ref_01" || snap.ProviderRefundID != "re_1" || snap.ProviderPaymentID != "pi_3A1" || snap.Status != connector.RefundPending ||
		snap.Amount != 10000 || snap.Currency != "INR" {
		t.Fatalf("snap = %+v", snap)
	}
	c0 := f.to("POST /v1/refunds")[0]
	if c0.Header.Get("Idempotency-Key") != "ref_01" {
		t.Errorf("idempotency key = %q", c0.Header.Get("Idempotency-Key"))
	}
	for k, v := range map[string]string{"payment_intent": "pi_3A1", "amount": "10000", "metadata[paymux_ref]": "ref_01", "reason": "requested_by_customer", "metadata[reason]": "requested_by_customer"} {
		if c0.Form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, c0.Form.Get(k), v)
		}
	}
	// A reason Stripe does not know is kept in the metadata only; a charge id goes in "charge".
	if _, err := c.Refund(bg, connector.RefundRequest{ProviderRef: "ref_02", ProviderPaymentID: "ch_3A1", Amount: 500, Currency: "JPY", Reason: "mandate_verification"}); err != nil {
		t.Fatal(err)
	}
	c1 := f.to("POST /v1/refunds")[1]
	if c1.Form.Get("charge") != "ch_3A1" || c1.Form.Has("payment_intent") || c1.Form.Has("reason") || c1.Form.Get("metadata[reason]") != "mandate_verification" ||
		c1.Form.Get("amount") != "500" || c1.Header.Get("Idempotency-Key") != "ref_02" {
		t.Errorf("second refund form = %v", c1.Form)
	}
	for name, req := range map[string]connector.RefundRequest{
		"no payment": {ProviderRef: "r", Amount: 1, Currency: "INR"}, "no ref": {ProviderPaymentID: "pi", Amount: 1, Currency: "INR"},
		"zero": {ProviderRef: "r", ProviderPaymentID: "pi", Currency: "INR"}, "bad currency": {ProviderRef: "r", ProviderPaymentID: "pi", Amount: 1, Currency: "ZZZ"},
	} {
		if _, err := c.Refund(bg, req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRefundRefusals(t *testing.T) {
	f := newFake()
	f.on("POST /v1/refunds", func(w http.ResponseWriter, _ call) {
		stripeErr(w, 400, "invalid_request_error", "charge_already_refunded", "Charge ch_3A1 has already been refunded.")
	})
	_, err := newConn(t, f).Refund(bg, connector.RefundRequest{ProviderRef: "r", ProviderPaymentID: "pi_3A1", Amount: 100, Currency: "INR"})
	var nr connector.ErrNotRefundable
	if !errors.As(err, &nr) || !strings.Contains(nr.Reason, "already been refunded") {
		t.Errorf("err = %v", err)
	}
	f.on("POST /v1/refunds", func(w http.ResponseWriter, _ call) {
		stripeErr(w, 400, "invalid_request_error", "resource_missing", "No such payment_intent")
	})
	if _, err := newConn(t, f).Refund(bg, connector.RefundRequest{ProviderRef: "r", ProviderPaymentID: "pi_x", Amount: 100, Currency: "INR"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("missing payment: %v", err)
	}
}

func TestFetchRefund(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/refunds/re_1", 200, refundJSON("re_1", "succeeded", "ref_01"))
	f.on("GET /v1/refunds", func(w http.ResponseWriter, c call) {
		if c.Query.Get("payment_intent") != "pi_3A1" {
			stripeErr(w, 400, "invalid_request_error", "", "bad query")
			return
		}
		write(w, 200, map[string]any{"data": []any{refundJSON("re_0", "failed", "ref_00"), refundJSON("re_2", "pending", "ref_02")}, "has_more": false})
	})
	c := newConn(t, f)
	if s, err := c.FetchRefund(bg, connector.RefundRef{ProviderRefundID: "re_1"}); err != nil || s.Status != connector.RefundSucceeded || s.ProviderRef != "ref_01" {
		t.Errorf("by id: %+v %v", s, err)
	}
	// By id, but the refund carries another reference: not this one.
	if _, err := c.FetchRefund(bg, connector.RefundRef{ProviderRefundID: "re_1", ProviderRef: "ref_99"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("foreign refund: %v", err)
	}
	// By reference within the payment: matched on our metadata, never by position.
	if s, err := c.FetchRefund(bg, connector.RefundRef{ProviderRef: "ref_02", ProviderPaymentID: "pi_3A1"}); err != nil || s.ProviderRefundID != "re_2" {
		t.Errorf("by reference: %+v %v", s, err)
	}
	if _, err := c.FetchRefund(bg, connector.RefundRef{ProviderRef: "ref_404", ProviderPaymentID: "pi_3A1"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown reference: %v", err)
	}
	if _, err := c.FetchRefund(bg, connector.RefundRef{ProviderRef: "ref_02"}); err == nil {
		t.Error("a reference without a payment was accepted")
	}
}

func TestListingsPageByCursorWithinTheRange(t *testing.T) {
	f := newFake()
	f.on("GET /v1/payment_intents", func(w http.ResponseWriter, c call) {
		if c.Query.Get("starting_after") == "" {
			write(w, 200, map[string]any{"data": []any{intentJSON("pi_1", "succeeded", "r1", chargeJSON("card", 0)), intentJSON("pi_2", "processing", "r2", nil)}, "has_more": true})
			return
		}
		write(w, 200, map[string]any{"data": []any{intentJSON("pi_3", "canceled", "", nil)}, "has_more": false})
	})
	f.on("GET /v1/refunds", func(w http.ResponseWriter, _ call) {
		write(w, 200, map[string]any{"data": []any{refundJSON("re_1", "succeeded", "ref_01")}, "has_more": false})
	})
	c := newConn(t, f)
	tr := connector.TimeRange{From: 1760000000000, To: 1760086400000}
	pg, err := c.ListPayments(bg, tr, "")
	if err != nil || len(pg.Items) != 2 || pg.Next != "pi_2" || pg.Items[0].Status != connector.PaymentSucceeded || pg.Items[0].ProviderRef != "r1" || pg.Items[0].Method != "card" ||
		pg.Items[1].Status != connector.PaymentProcessing {
		t.Fatalf("page 1 = %+v %v", pg, err)
	}
	q := f.to("GET /v1/payment_intents")[0].Query
	if q.Get("created[gte]") != "1760000000" || q.Get("created[lt]") != "1760086400" || q.Get("limit") != "100" || q["expand[]"][0] != "data.latest_charge" {
		t.Errorf("query = %v", q)
	}
	pg, err = c.ListPayments(bg, tr, pg.Next)
	if err != nil || len(pg.Items) != 1 || pg.Next != "" || pg.Items[0].Status != connector.PaymentCanceled {
		t.Fatalf("page 2 = %+v %v", pg, err)
	}
	if f.to("GET /v1/payment_intents")[1].Query.Get("starting_after") != "pi_2" {
		t.Error("cursor not sent")
	}
	rp, err := c.ListRefunds(bg, tr, "")
	if err != nil || len(rp.Items) != 1 || rp.Items[0].ProviderRef != "ref_01" || rp.Next != "" {
		t.Errorf("refunds = %+v %v", rp, err)
	}
}
