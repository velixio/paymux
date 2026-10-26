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
	// ListErr fails ListPayments and ListRefunds.
	ListErr error
	// Settlements is the provider's view; ListSettlements returns those settled in the range.
	Settlements []connector.SettlementSnapshot
	// Mandates and Subs are the provider's view (FetchMandate, FetchSubscription), keyed by provider id.
	Mandates map[string]connector.MandateSnapshot
	Subs     map[string]connector.SubscriptionSnapshot
	// PayoutFn answers CreatePayout (default: accepts it as processing, once per
	// provider_ref); Payouts is the provider's view keyed by provider_ref
	// (FetchPayout); PayoutCalls logs every CreatePayout request;
	// PayoutFetchErr fails FetchPayout.
	PayoutFn       func(connector.PayoutRequest) (connector.PayoutSnapshot, error)
	Payouts        map[string]connector.PayoutSnapshot
	PayoutCalls    []connector.PayoutRequest
	PayoutFetchErr error
	// ManagerErr fails FetchMandate and FetchSubscription.
	ManagerErr                 error
	MandateFetches, SubFetches []string
	SettleErr                  error
	// AcceptFn and EvidenceFn run after the call is recorded, so a test can
	// move the provider's dispute (for example to under_review).
	AcceptFn   func(connector.DisputeRef)
	EvidenceFn func(connector.DisputeRef)

	Checkouts    []connector.CheckoutRequest
	Returns      []connector.ReturnRequest
	Fetches      []connector.PaymentRef
	RefundCalls  []connector.RefundRequest
	RefundFetch  []connector.RefundRef
	DisputeList  int
	PaymentLists int
	RefundLists  int
	ListRanges   []connector.TimeRange
	DisputeGets  []connector.DisputeRef
	Accepts      []connector.DisputeRef
	Evidence     []connector.Evidence
	Configs      []json.RawMessage
	Deps         []connector.Deps
	Built        int
	// LiveMerchantOfRecord makes the connector instances report themselves
	// merchant of record, as a remote connector can after discovery although
	// its registry entry cannot say.
	LiveMerchantOfRecord bool
	// NoHosts makes the connector instances declare no checkout hosts, so the
	// hub refuses every URL they return.
	NoHosts bool
}

var seq atomic.Int64

// Register registers a fresh fake connector under a unique name derived from
// prefix and returns its state. It is safe to call from parallel tests.
func Register(prefix string) *State { return RegisterSchema(prefix, schema) }

// RegisterSchema is Register with a custom config schema. With a schema other
// than the default the fake accepts any config object.
func RegisterSchema(prefix, configSchema string) *State {
	st := &State{Name: fmt.Sprintf("%s%d", prefix, seq.Add(1)), Payments: map[string]connector.PaymentSnapshot{},
		Refunds: map[string]connector.RefundSnapshot{}, Disputes: map[string]connector.DisputeSnapshot{},
		Payouts: map[string]connector.PayoutSnapshot{}, Mandates: map[string]connector.MandateSnapshot{}, Subs: map[string]connector.SubscriptionSnapshot{}}
	info := connector.Info{
		Version: "test", DisplayName: "Fake", ConfigSchema: json.RawMessage(configSchema),
		Currencies: []string{"INR", "USD", "BDT"},
		// A frame host to see declared hosts reach the hosted page's policy.
		CheckoutHosts: &connector.CheckoutHosts{Frame: []string{"https://frames.fake.example.com"}},
		Capabilities: []connector.Capability{connector.CapRefund, connector.CapDisputeRead, connector.CapDisputeRespond, connector.CapList, connector.CapSettle,
			connector.CapMandateManager, connector.CapSubscriptionManager, connector.CapPayout},
		RateLimit: connector.RateLimit{RequestsPerSecond: 100, Burst: 100},
	}
	connector.Register(st.Name, func(cfg json.RawMessage, deps connector.Deps) (connector.Connector, error) {
		var c struct {
			APIKey string `json:"api_key"`
		}
		if err := json.Unmarshal(cfg, &c); err != nil || (configSchema == schema && c.APIKey == "") {
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

// CheckoutHosts implements connector.CheckoutHoster: the fake may send the
// customer to any example.com host, so a test that wants a refusal uses another
// domain. The hosted page's policy (Info) is not widened by this.
func (c *Conn) CheckoutHosts() connector.CheckoutHosts {
	c.st.mu.Lock()
	none := c.st.NoHosts
	c.st.mu.Unlock()
	if none {
		return connector.CheckoutHosts{}
	}
	h := *c.Info().CheckoutHosts
	open := []string{"https://*.example.com"}
	h.Script, h.Form, h.Redirect = open, open, open
	return h
}

// Info implements connector.Connector.
func (c *Conn) Info() connector.Info {
	_, info, _ := connector.Lookup(c.st.Name)
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	info.MerchantOfRecord = info.MerchantOfRecord || c.st.LiveMerchantOfRecord
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

func inRange(tr connector.TimeRange, createdAt int64) bool {
	if createdAt == 0 {
		return true
	}
	return (tr.From == 0 || createdAt >= tr.From) && (tr.To == 0 || createdAt <= tr.To)
}

// ListPayments implements connector.Lister: every scripted payment created in
// the range (CreatedAt 0 always matches), in one page.
func (c *Conn) ListPayments(_ context.Context, tr connector.TimeRange, _ connector.Cursor) (connector.Page[connector.PaymentSnapshot], error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	c.st.PaymentLists++
	c.st.ListRanges = append(c.st.ListRanges, tr)
	if c.st.ListErr != nil {
		return connector.Page[connector.PaymentSnapshot]{}, c.st.ListErr
	}
	var items []connector.PaymentSnapshot
	for key, s := range c.st.Payments {
		if s.ProviderRef == "" {
			s.ProviderRef = key
		}
		if inRange(tr, s.CreatedAt) {
			items = append(items, s)
		}
	}
	return connector.Page[connector.PaymentSnapshot]{Items: items}, nil
}

// ListRefunds implements connector.Lister.
func (c *Conn) ListRefunds(_ context.Context, tr connector.TimeRange, _ connector.Cursor) (connector.Page[connector.RefundSnapshot], error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	c.st.RefundLists++
	if c.st.ListErr != nil {
		return connector.Page[connector.RefundSnapshot]{}, c.st.ListErr
	}
	var items []connector.RefundSnapshot
	for key, s := range c.st.Refunds {
		if s.ProviderRef == "" {
			s.ProviderRef = key
		}
		if inRange(tr, s.CreatedAt) {
			items = append(items, s)
		}
	}
	return connector.Page[connector.RefundSnapshot]{Items: items}, nil
}

// ListSettlements implements connector.Settler.
func (c *Conn) ListSettlements(_ context.Context, tr connector.TimeRange) ([]connector.SettlementSnapshot, error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	if c.st.SettleErr != nil {
		return nil, c.st.SettleErr
	}
	var out []connector.SettlementSnapshot
	for _, s := range c.st.Settlements {
		if inRange(tr, s.SettledAt) {
			out = append(out, s)
		}
	}
	return out, nil
}

// FetchMandate implements connector.MandateManager.
func (c *Conn) FetchMandate(_ context.Context, ref connector.MandateRef) (connector.MandateSnapshot, error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	c.st.MandateFetches = append(c.st.MandateFetches, ref.ProviderMandateID)
	if c.st.ManagerErr != nil {
		return connector.MandateSnapshot{}, c.st.ManagerErr
	}
	m, ok := c.st.Mandates[ref.ProviderMandateID]
	if !ok {
		return connector.MandateSnapshot{}, connector.ErrNotFound
	}
	return m, nil
}

// FetchSubscription implements connector.SubscriptionManager.
func (c *Conn) FetchSubscription(_ context.Context, ref connector.SubscriptionRef) (connector.SubscriptionSnapshot, error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	c.st.SubFetches = append(c.st.SubFetches, ref.ProviderSubscriptionID)
	if c.st.ManagerErr != nil {
		return connector.SubscriptionSnapshot{}, c.st.ManagerErr
	}
	s, ok := c.st.Subs[ref.ProviderSubscriptionID]
	if !ok {
		return connector.SubscriptionSnapshot{}, connector.ErrNotFound
	}
	return s, nil
}

// The remaining manager methods are not scripted.

func (c *Conn) CreateMandateCheckout(context.Context, connector.MandateRequest) (connector.CheckoutResult, error) {
	return connector.CheckoutResult{}, connector.ErrUnsupported
}
func (c *Conn) NotifyPreDebit(context.Context, connector.PreDebitRequest) error {
	return connector.ErrUnsupported
}
func (c *Conn) ChargeMandate(context.Context, connector.ChargeRequest) (connector.PaymentSnapshot, error) {
	return connector.PaymentSnapshot{}, connector.ErrUnsupported
}
func (c *Conn) RevokeMandate(context.Context, connector.MandateRef) error {
	return connector.ErrUnsupported
}
func (c *Conn) CreateSubscriptionCheckout(context.Context, connector.SubscriptionRequest) (connector.CheckoutResult, error) {
	return connector.CheckoutResult{}, connector.ErrUnsupported
}
func (c *Conn) CancelSubscription(context.Context, connector.SubscriptionRef, bool) error {
	return connector.ErrUnsupported
}
func (c *Conn) UpdateSubscriptionAmount(context.Context, connector.SubscriptionRef, int64) error {
	return connector.ErrUnsupported
}

// CreatePayout implements connector.Payouter. Repeating a provider_ref returns
// the payout already made, as a real provider does.
func (c *Conn) CreatePayout(_ context.Context, req connector.PayoutRequest) (connector.PayoutSnapshot, error) {
	c.st.mu.Lock()
	c.st.PayoutCalls = append(c.st.PayoutCalls, req)
	fn := c.st.PayoutFn
	existing, seen := c.st.Payouts[req.ProviderRef]
	c.st.mu.Unlock()
	if seen {
		return existing, nil
	}
	if fn != nil {
		snap, err := fn(req)
		if err == nil {
			c.st.mu.Lock()
			c.st.Payouts[req.ProviderRef] = snap
			c.st.mu.Unlock()
		}
		return snap, err
	}
	snap := connector.PayoutSnapshot{ProviderRef: req.ProviderRef, ProviderPayoutID: "pout_" + req.ProviderRef,
		Status: connector.PayoutProcessing, RawStatus: "processing", Amount: req.Amount, Currency: req.Currency}
	c.st.mu.Lock()
	c.st.Payouts[req.ProviderRef] = snap
	c.st.mu.Unlock()
	return snap, nil
}

// FetchPayout implements connector.Payouter.
func (c *Conn) FetchPayout(_ context.Context, ref connector.PayoutRef) (connector.PayoutSnapshot, error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	if c.st.PayoutFetchErr != nil {
		return connector.PayoutSnapshot{}, c.st.PayoutFetchErr
	}
	if s, ok := c.st.Payouts[ref.ProviderRef]; ok {
		return s, nil
	}
	return connector.PayoutSnapshot{}, connector.ErrNotFound
}

var (
	_ connector.Payouter            = (*Conn)(nil)
	_ connector.MandateManager      = (*Conn)(nil)
	_ connector.SubscriptionManager = (*Conn)(nil)
	_ connector.Settler             = (*Conn)(nil)
	_ connector.Lister              = (*Conn)(nil)
	_ connector.Connector           = (*Conn)(nil)
	_ connector.Refunder            = (*Conn)(nil)
	_ connector.DisputeReader       = (*Conn)(nil)
	_ connector.DisputeResponder    = (*Conn)(nil)
)
