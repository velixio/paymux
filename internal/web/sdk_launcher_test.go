package web

import (
	"bytes"
	"strings"
	"testing"
)

// The SDK page launches the provider's widget: Razorpay's checkout, and Zoho
// Payments' widget, whose script defines ZPayments.
func TestSDKPageLaunchesTheZohoWidget(t *testing.T) {
	r, err := NewRenderer(Embedded())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err = r.Render(&buf, "pay_sdk", PayPage{AppName: "Voicestudio", ScriptURL: "https://static.zohocdn.com/zpay/zpay-js/v1/zpayments.js", Launcher: "zoho",
		Options: map[string]any{"account_id": "acc_1", "api_key": "wk_1", "domain": "IN", "payments_session_id": "ps_1", "amount": "499.00",
			"currency": "INR", "return_url": "https://pay.example.com/return/pa_1/ref_1"}})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"ZPayments", "requestPaymentMethod", "payments_session_id", "return_url"} {
		if !strings.Contains(out, want) {
			t.Errorf("the page lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "TODO") {
		t.Error("a TODO is shipped to the browser")
	}
}
