package connector_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/velixio/paymux/pkg/connector"
)

// acme is the smallest useful connector: it only declares what it is.
type acme struct{ deps connector.Deps }

func (a acme) Info() connector.Info { return connector.Info{Name: "acme"} }
func (acme) CreateCheckout(context.Context, connector.CheckoutRequest) (connector.CheckoutResult, error) {
	return connector.CheckoutResult{}, connector.ErrUnsupported
}
func (acme) HandleReturn(context.Context, connector.ReturnRequest) (connector.ReturnResult, error) {
	return connector.ReturnResult{}, connector.ErrUnsupported
}
func (acme) VerifyWebhook(context.Context, connector.WebhookRequest) error {
	return connector.ErrInvalidSignature
}
func (acme) ParseWebhook(context.Context, connector.WebhookRequest) ([]connector.Notification, error) {
	return nil, nil
}
func (acme) FetchPayment(context.Context, connector.PaymentRef) (connector.PaymentSnapshot, error) {
	return connector.PaymentSnapshot{}, connector.ErrNotFound
}

// A connector registers its factory and static Info from init; the server
// looks it up by name when a provider account is created.
func ExampleRegister() {
	connector.ResetRegistryForTest()
	factory := func(cfg json.RawMessage, deps connector.Deps) (connector.Connector, error) {
		return acme{deps: deps}, nil
	}
	connector.Register("acme", factory, connector.Info{
		DisplayName:  "Acme Pay",
		Capabilities: []connector.Capability{connector.CapRefund},
		Currencies:   []string{"USD"},
	})

	_, info, ok := connector.Lookup("acme")
	fmt.Println(ok, info.Name, info.DisplayName, info.Has(connector.CapRefund), info.Has(connector.CapCancel))
	for _, i := range connector.List() {
		fmt.Println("registered:", i.Name)
	}
	// Output:
	// true acme Acme Pay true false
	// registered: acme
}

// Errors are typed; match them with errors.Is / errors.As, never by text.
func ExampleErrNotRefundable() {
	err := fmt.Errorf("acme refund: %w", connector.ErrNotRefundable{Reason: "payment older than 180 days"})

	var nr connector.ErrNotRefundable
	fmt.Println(errors.As(err, &nr), nr.Reason)
	fmt.Println(errors.Is(err, connector.ErrTemporary))
	// Output:
	// true payment older than 180 days
	// false
}

// VerifyWebhook reports a bad signature with ErrInvalidSignature, and only
// event types the provider never signs with ErrUnsigned.
func ExampleErrInvalidSignature() {
	var c connector.Connector = acme{}
	err := c.VerifyWebhook(context.Background(), connector.WebhookRequest{Body: []byte(`{}`)})
	fmt.Println(errors.Is(err, connector.ErrInvalidSignature), errors.Is(err, connector.ErrUnsigned))
	// Output: true false
}

// MemKV is the in-memory KV for tests; values can expire.
func ExampleMemKV() {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	kv := connector.NewMemKV(func() time.Time { return now })
	ctx := context.Background()

	_ = kv.Set(ctx, "plan:month:49900:INR", []byte("plan_123"), time.Hour)
	v, ok, _ := kv.Get(ctx, "plan:month:49900:INR")
	fmt.Println(string(v), ok)

	now = now.Add(2 * time.Hour)
	_, ok, _ = kv.Get(ctx, "plan:month:49900:INR")
	fmt.Println(ok)
	// Output:
	// plan_123 true
	// false
}

// Deps.Now uses the injected clock, so connectors never call time.Now.
func ExampleDeps_Now() {
	fixed := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	deps := connector.Deps{Clock: func() time.Time { return fixed }}
	fmt.Println(deps.Now().Format(time.DateOnly))
	// Output: 2026-10-17
}

// Webhook headers are looked up case-insensitively.
func ExampleWebhookRequest_Header() {
	req := connector.WebhookRequest{Headers: map[string][]string{"X-Signature": {"abc"}}}
	fmt.Println(req.Header("x-signature"))
	// Output: abc
}
