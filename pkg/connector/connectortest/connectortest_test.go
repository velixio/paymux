package connectortest_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// mock is a minimal conformant connector used to test the kit itself.
type mock struct {
	secret string
	http   *http.Client
}

func (m *mock) Info() connector.Info {
	return connector.Info{Name: "mock", Version: "1", DisplayName: "Mock", ConfigSchema: json.RawMessage(`{}`), Currencies: []string{"INR", "JPY"}}
}

func (m *mock) CreateCheckout(_ context.Context, r connector.CheckoutRequest) (connector.CheckoutResult, error) {
	return connector.CheckoutResult{NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: "https://x.example.com/" + r.ProviderRef}}, nil
}

func (m *mock) HandleReturn(_ context.Context, r connector.ReturnRequest) (connector.ReturnResult, error) {
	if r.Param("sig") != "good" {
		return connector.ReturnResult{}, connector.ErrInvalidReturn
	}
	return connector.ReturnResult{ProviderRef: r.ProviderRef, ProviderPaymentID: "p1"}, nil
}

func (m *mock) sign(b []byte) string {
	h := hmac.New(sha256.New, []byte(m.secret))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func (m *mock) VerifyWebhook(_ context.Context, r connector.WebhookRequest) error {
	if !hmac.Equal([]byte(r.Header("X-Sig")), []byte(m.sign(r.Body))) {
		return connector.ErrInvalidSignature
	}
	return nil
}

func (m *mock) ParseWebhook(context.Context, connector.WebhookRequest) ([]connector.Notification, error) {
	return []connector.Notification{{Kind: connector.NotifyPayment, EventID: "e1", EventType: "paid"}}, nil
}

func (m *mock) FetchPayment(_ context.Context, ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
	resp, err := m.http.Get("https://api.example.com/pay/" + ref.ProviderPaymentID)
	if err != nil {
		return connector.PaymentSnapshot{}, err
	}
	defer resp.Body.Close()
	var v struct {
		Status string `json:"status"`
		Amount int64  `json:"amount"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	st := map[string]connector.PaymentStatus{"ok": connector.PaymentSucceeded, "bad": connector.PaymentFailed}[v.Status]
	return connector.PaymentSnapshot{Status: st, RawStatus: v.Status, Amount: v.Amount, Currency: "INR"}, nil
}

func statusHandler(status string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "amount": 500})
	})
}

func TestRunAgainstMock(t *testing.T) {
	m := &mock{secret: "s3cret"}
	factory := func(_ json.RawMessage, d connector.Deps) (connector.Connector, error) {
		return &mock{secret: "s3cret", http: d.HTTP}, nil
	}
	body := []byte(`{"id":1}`)
	wrong := &mock{secret: "other"}
	connectortest.Run(t, factory, connectortest.Fixtures{
		Config: json.RawMessage(`{}`),
		Ref:    connector.PaymentRef{ProviderPaymentID: "p1"},
		Statuses: []connectortest.StatusCase{
			{Raw: "ok", Want: connector.PaymentSucceeded, WantAmount: 500, Handler: statusHandler("ok")},
			{Raw: "bad", Want: connector.PaymentFailed, WantAmount: 500, Handler: statusHandler("bad")},
		},
		Webhooks: connectortest.WebhookFixtures{
			Valid:            connector.WebhookRequest{Method: "POST", Body: body, Headers: map[string][]string{"X-Sig": {m.sign(body)}}},
			WrongSecret:      connector.WebhookRequest{Method: "POST", Body: body, Headers: map[string][]string{"X-Sig": {wrong.sign(body)}}},
			SignatureHeaders: []string{"X-Sig"},
		},
		Return: &connectortest.ReturnFixtures{
			Valid:    connector.ReturnRequest{ProviderRef: "r", Query: map[string][]string{"sig": {"good"}}},
			Tampered: connector.ReturnRequest{ProviderRef: "r", Query: map[string][]string{"sig": {"bad"}}},
			Want:     connector.ReturnResult{ProviderRef: "r", ProviderPaymentID: "p1"},
		},
		Checkout: connector.CheckoutRequest{ProviderRef: "r1", Amount: 100, Currency: "INR"},
	})
}
