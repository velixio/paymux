// Package fakeconn is a scriptable in-memory connector for tests of the
// checkout, pay-page and inbound code. It never touches the network.
package fakeconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/paymux/paymux/pkg/connector"
)

const schema = `{"type":"object","required":["api_key"],"additionalProperties":false,
"properties":{"api_key":{"type":"string","minLength":1,"x-secret":true},"region":{"type":"string"}}}`

// ValidConfig is a config the fake accepts.
const ValidConfig = `{"api_key":"fake_key_123"}`

// State is the scripted behaviour and call log shared by every Conn built
// for one registered name.
type State struct {
	Name string

	mu sync.Mutex
	// CheckoutFn answers CreateCheckout; the default redirects to
	// https://fake.example.com/checkout/<provider_ref>.
	CheckoutFn func(connector.CheckoutRequest) (connector.CheckoutResult, error)
	// Payments is the provider's view, keyed by provider_ref. FetchPayment
	// also matches ProviderOrderID and ProviderPaymentID.
	Payments  map[string]connector.PaymentSnapshot
	FetchErr  error
	ReturnErr error
	// RefundFn answers Refund; the default accepts and returns pending.
	RefundFn func(connector.RefundRequest) (connector.RefundSnapshot, error)
	// Refunds is the provider's view, keyed by provider_ref (FetchRefund).
	Refunds map[string]connector.RefundSnapshot
	// Disputes is the provider's view, keyed by provider dispute id.
	Disputes map[string]connector.DisputeSnapshot
	// DisputeErr fails FetchDispute and ListDisputes; AcceptErr and
	// EvidenceErr fail the responder calls.
	DisputeErr, AcceptErr, EvidenceErr error
	// AcceptFn and EvidenceFn run after the call is recorded, so a test can
	// move the provider's dispute (for example to under_review).
	AcceptFn   func(connector.DisputeRef)
	EvidenceFn func(connector.DisputeRef)

	Checkouts   []connector.CheckoutRequest
	Returns     []connector.ReturnRequest
	Fetches     []connector.PaymentRef
	RefundCalls []connector.RefundRequest
	RefundFetch []connector.RefundRef
	DisputeList int
	DisputeGets []connector.DisputeRef
	Accepts     []connector.DisputeRef
	Evidence    []connector.Evidence
	Configs     []json.RawMessage
	Deps        []connector.Deps
	Built       int
}

var seq atomic.Int64

// Register registers a fresh fake connector under a unique name derived from
// prefix and returns its state. It is safe to call from parallel tests.
func Register(prefix string) *State {
	st := &State{Name: fmt.Sprintf("%s%d", prefix, seq.Add(1)), Payments: map[string]connector.PaymentSnapshot{},
		Refunds: map[string]connector.RefundSnapshot{}, Disputes: map[string]connector.DisputeSnapshot{}}
	info := connector.Info{
		Version: "test", DisplayName: "Fake", ConfigSchema: json.RawMessage(schema),
		Currencies:   []string{"INR", "USD", "BDT"},
		Capabilities: []connector.Capability{connector.CapRefund, connector.CapDisputeRead, connector.CapDisputeRespond},
		RateLimit:    connector.RateLimit{RequestsPerSecond: 100, Burst: 100},
	}
	connector.Register(st.Name, func(cfg json.RawMessage, deps connector.Deps) (connector.Connector, error) {
		var c struct {
			APIKey string `json:"api_key"`
		}
		if err := json.Unmarshal(cfg, &c); err != nil || c.APIKey == "" {
			return nil, errors.New("fakeconn: api_key is required")
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		st.Built++
		st.Configs = append(st.Configs, append(json.RawMessage(nil), cfg...))
		st.Deps = append(st.Deps, deps)
		return &Conn{st: st}, nil
	}, info)
	return st
}

// Locked runs fn with the state lock held (for test setup and assertions).
func (s *State) Locked(fn func()) { s.mu.Lock(); defer s.mu.Unlock(); fn() }

// Conn is the connector instance.
type Conn struct{ st *State }

// Info implements connector.Connector.
func (c *Conn) Info() connector.Info {
	_, info, _ := connector.Lookup(c.st.Name)
	return info
}

// CreateCheckout implements connector.Connector.
func (c *Conn) CreateCheckout(_ context.Context, req connector.CheckoutRequest) (connector.CheckoutResult, error) {
	c.st.mu.Lock()
	c.st.Checkouts = append(c.st.Checkouts, req)
	fn := c.st.CheckoutFn
	c.st.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return connector.CheckoutResult{ProviderOrderID: "order_" + req.ProviderRef,
		NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: "https://fake.example.com/checkout/" + req.ProviderRef}}, nil
}

// HandleReturn implements connector.Connector: it accepts a return carrying
// form or query field "sig=ok" and echoes "payment_id".
func (c *Conn) HandleReturn(_ context.Context, req connector.ReturnRequest) (connector.ReturnResult, error) {
	c.st.mu.Lock()
	c.st.Returns = append(c.st.Returns, req)
	err := c.st.ReturnErr
	c.st.mu.Unlock()
	if err != nil {
		return connector.ReturnResult{}, err
	}
	if req.Param("sig") != "ok" {
		return connector.ReturnResult{}, connector.ErrInvalidReturn
	}
	return connector.ReturnResult{ProviderRef: req.ProviderRef, ProviderPaymentID: req.Param("payment_id"),
		ProviderOrderID: req.Param("order_id")}, nil
}

// VerifyWebhook implements connector.Connector: header X-Fake-Sig "ok" is
// valid, "unsigned" stands for an event type the provider never signs, and
// anything else (including no header) is an invalid signature.
func (c *Conn) VerifyWebhook(_ context.Context, req connector.WebhookRequest) error {
	switch req.Header("X-Fake-Sig") {
	case "ok":
		return nil
	case "unsigned":
		return connector.ErrUnsigned
	}
	return connector.ErrInvalidSignature
}

// ParseWebhook implements connector.Connector: the body is a JSON array of
// connector.Notification (or a single one).
func (c *Conn) ParseWebhook(_ context.Context, req connector.WebhookRequest) ([]connector.Notification, error) {
	var many []connector.Notification
	if err := json.Unmarshal(req.Body, &many); err == nil {
		return many, nil
	}
	var one connector.Notification
	if err := json.Unmarshal(req.Body, &one); err != nil {
		return nil, fmt.Errorf("fakeconn: bad body: %w", err)
	}
	return []connector.Notification{one}, nil
}

// FetchPayment implements connector.Connector.
func (c *Conn) FetchPayment(_ context.Context, ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	c.st.Fetches = append(c.st.Fetches, ref)
	if c.st.FetchErr != nil {
		return connector.PaymentSnapshot{}, c.st.FetchErr
	}
	for key, snap := range c.st.Payments {
		if key == ref.ProviderRef && ref.ProviderRef != "" ||
			ref.ProviderOrderID != "" && snap.ProviderOrderID == ref.ProviderOrderID ||
			ref.ProviderPaymentID != "" && snap.ProviderPaymentID == ref.ProviderPaymentID {
			if snap.ProviderRef == "" {
				snap.ProviderRef = key
			}
			return snap, nil
		}
	}
	return connector.PaymentSnapshot{}, connector.ErrNotFound
}

// Refund implements connector.Refunder.
func (c *Conn) Refund(_ context.Context, req connector.RefundRequest) (connector.RefundSnapshot, error) {
	c.st.mu.Lock()
	c.st.RefundCalls = append(c.st.RefundCalls, req)
	fn := c.st.RefundFn
	c.st.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return connector.RefundSnapshot{ProviderRef: req.ProviderRef, ProviderRefundID: "rfnd_" + req.ProviderRef,
		ProviderPaymentID: req.ProviderPaymentID, Status: connector.RefundPending, RawStatus: "pending",
		Amount: req.Amount, Currency: req.Currency}, nil
}

// FetchRefund implements connector.Refunder.
func (c *Conn) FetchRefund(_ context.Context, ref connector.RefundRef) (connector.RefundSnapshot, error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	c.st.RefundFetch = append(c.st.RefundFetch, ref)
	for key, snap := range c.st.Refunds {
		if key == ref.ProviderRef && ref.ProviderRef != "" || ref.ProviderRefundID != "" && snap.ProviderRefundID == ref.ProviderRefundID {
			if snap.ProviderRef == "" {
				snap.ProviderRef = key
			}
			return snap, nil
		}
	}
	return connector.RefundSnapshot{}, connector.ErrNotFound
}

// ListDisputes implements connector.DisputeReader (one page, every dispute).
func (c *Conn) ListDisputes(_ context.Context, _ connector.TimeRange, _ connector.Cursor) (connector.Page[connector.DisputeSnapshot], error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	c.st.DisputeList++
	if c.st.DisputeErr != nil {
		return connector.Page[connector.DisputeSnapshot]{}, c.st.DisputeErr
	}
	var items []connector.DisputeSnapshot
	for _, d := range c.st.Disputes {
		items = append(items, d)
	}
	return connector.Page[connector.DisputeSnapshot]{Items: items}, nil
}

// FetchDispute implements connector.DisputeReader.
func (c *Conn) FetchDispute(_ context.Context, ref connector.DisputeRef) (connector.DisputeSnapshot, error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	c.st.DisputeGets = append(c.st.DisputeGets, ref)
	if c.st.DisputeErr != nil {
		return connector.DisputeSnapshot{}, c.st.DisputeErr
	}
	d, ok := c.st.Disputes[ref.ProviderDisputeID]
	if !ok {
		return connector.DisputeSnapshot{}, connector.ErrNotFound
	}
	return d, nil
}

// AcceptDispute implements connector.DisputeResponder.
func (c *Conn) AcceptDispute(_ context.Context, ref connector.DisputeRef) error {
	c.st.mu.Lock()
	c.st.Accepts = append(c.st.Accepts, ref)
	err, fn := c.st.AcceptErr, c.st.AcceptFn
	c.st.mu.Unlock()
	if err == nil && fn != nil {
		fn(ref)
	}
	return err
}

// SubmitEvidence implements connector.DisputeResponder.
func (c *Conn) SubmitEvidence(_ context.Context, ref connector.DisputeRef, ev connector.Evidence) error {
	c.st.mu.Lock()
	c.st.Evidence = append(c.st.Evidence, ev)
	err, fn := c.st.EvidenceErr, c.st.EvidenceFn
	c.st.mu.Unlock()
	if err == nil && fn != nil {
		fn(ref)
	}
	return err
}

var (
	_ connector.Connector        = (*Conn)(nil)
	_ connector.Refunder         = (*Conn)(nil)
	_ connector.DisputeReader    = (*Conn)(nil)
	_ connector.DisputeResponder = (*Conn)(nil)
)
