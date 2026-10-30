package razorpay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

var testConfig = json.RawMessage(`{"key_id":"` + testKeyID + `","key_secret":"` + testKeySecret + `","webhook_secret":"` + testWebhookSecret + `","name":"Voice Studio","logo":"https://example.com/logo.png"}`)

func sign(secret string, b []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func webhookReq(t *testing.T, name, secret, eventID string) connector.WebhookRequest {
	t.Helper()
	b := fixture(t, name)
	return connector.WebhookRequest{
		Method: "POST", Body: b, ReceivedAt: 1760000100000,
		Headers: map[string][]string{
			"X-Razorpay-Signature": {sign(secret, b)},
			"X-Razorpay-Event-Id":  {eventID},
			"Content-Type":         {"application/json"},
		},
	}
}

func TestConformance(t *testing.T) { connectortest.Run(t, New, conformanceFixtures(t)) }

// conformanceFixtures are shared with the remote-connector test.
func conformanceFixtures(t *testing.T) connectortest.Fixtures {
	statuses := []struct {
		raw, wantRaw string
		want         connector.PaymentStatus
	}{
		{"created", "", connector.PaymentProcessing},
		{"authorized", "captured", connector.PaymentSucceeded}, // captured by FetchPayment
		{"captured", "", connector.PaymentSucceeded},
		{"refunded", "", connector.PaymentSucceeded},
		{"failed", "", connector.PaymentFailed},
	}
	var cases []connectortest.StatusCase
	for _, s := range statuses {
		cases = append(cases, connectortest.StatusCase{
			Raw: s.raw, WantRaw: s.wantRaw, Want: s.want, WantAmount: 49900, Handler: newFake(s.raw),
		})
	}
	return connectortest.Fixtures{
		Config:   testConfig,
		Now:      time.Unix(1760000100, 0),
		Ref:      connector.PaymentRef{ProviderOrderID: "order_Nx0"},
		Statuses: cases,
		Webhooks: connectortest.WebhookFixtures{
			Valid:            webhookReq(t, "event_payment_captured", testWebhookSecret, "evt_1"),
			WrongSecret:      webhookReq(t, "event_payment_captured", "not_the_secret", "evt_1"),
			SignatureHeaders: []string{"X-Razorpay-Signature"},
		},
		Return: &connectortest.ReturnFixtures{
			Valid: connector.ReturnRequest{ProviderRef: "pay_ref_01", Method: "POST", Form: map[string][]string{
				"razorpay_order_id": {"order_Nx0"}, "razorpay_payment_id": {"pay_Nx1"},
				"razorpay_signature": {sign(testKeySecret, []byte("order_Nx0|pay_Nx1"))},
			}},
			Tampered: connector.ReturnRequest{ProviderRef: "pay_ref_01", Method: "POST", Form: map[string][]string{
				"razorpay_order_id": {"order_Nx0"}, "razorpay_payment_id": {"pay_OTHER"},
				"razorpay_signature": {sign(testKeySecret, []byte("order_Nx0|pay_Nx1"))},
			}},
			Want: connector.ReturnResult{ProviderRef: "pay_ref_01", ProviderOrderID: "order_Nx0", ProviderPaymentID: "pay_Nx1"},
		},
		Checkout: connector.CheckoutRequest{
			PaymentID: "pay_01", AttemptID: "att_01", AppID: "app_01", ProviderRef: "pay_ref_01",
			Amount: 49900, Currency: "INR", Customer: connector.Customer{Name: "A B", Email: "a@example.com", Phone: "+919999999999"},
		},
		CheckoutHandler: newFake("created"),
	}
}

// Golden values computed independently with Python hmac over the testdata file.
func TestGoldenSignatures(t *testing.T) {
	if got := sign(testWebhookSecret, fixture(t, "event_payment_captured")); got != "289766a19a71737519e55947cf3212ebdaf7deae6176dd05e6e1e67e9ad5f901" {
		t.Fatalf("webhook signature drifted: %s", got)
	}
	if got := sign(testKeySecret, []byte("order_Nx0|pay_Nx1")); got != "91321c64d23c827dd3a1a2e4a2df27fa4ad5d962c8bbe3e5299267476c02a6f9" {
		t.Fatalf("return signature drifted: %s", got)
	}
}
