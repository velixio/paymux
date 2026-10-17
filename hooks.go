package paymux

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/store"
)

// Go hooks (framework mode).
//
// A hook is a Go function registered on an App before Start. Hooks run after
// the transaction that caused the event has committed, on the job queue, so:
//
//   - a hook can never roll back or block a payment, refund or invoice;
//   - a hook that returns an error (or panics) is logged and raised as an ops
//     alert ("hook.failed"), and nothing else happens: the event is not
//     retried and the other hooks still run;
//   - delivery is at least once: if the process dies while a hook runs, the
//     hook may run again after a restart, so hooks should be idempotent.
//
// Hooks receive read-only snapshots (plain values, copied for every hook) and
// no handle to Paymux, so they cannot move money. OnRoute is the only hook
// that runs inline: it can reorder, never add to, the provider accounts
// routing already allows.

// hookTimeout bounds one hook call. The context a hook receives is cancelled
// after it; a hook that ignores the context can still outlive it.
const hookTimeout = 30 * time.Second

// EventInfo says which event a hook is running for.
type EventInfo struct {
	Type          string `json:"type"` // for example "payment.succeeded"
	OrgID         string `json:"org_id"`
	AppID         string `json:"app_id"`
	Mode          string `json:"mode"`        // "test" or "live"
	ObjectType    string `json:"object_type"` // "payment", "refund", ...
	ObjectID      string `json:"object_id"`
	ObjectVersion int64  `json:"object_version"`
}

// Payment is a snapshot of a payment.
type Payment struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	Kind           string            `json:"kind"`
	Amount         int64             `json:"amount"` // minor units
	Currency       string            `json:"currency"`
	AmountReceived int64             `json:"amount_received"`
	AmountRefunded int64             `json:"amount_refunded"`
	OrderRef       string            `json:"order_ref"`
	Description    string            `json:"description"`
	CustomerID     string            `json:"customer_id"`
	Metadata       map[string]string `json:"metadata"`
	Flags          []string          `json:"flags"`
	ExpiresAt      time.Time         `json:"expires_at"`
	CreatedAt      time.Time         `json:"created_at"`
	Version        int64             `json:"version"`
}

// PaymentEvent is what OnPaymentSucceeded and OnPaymentFailed hooks get.
type PaymentEvent struct {
	Event   EventInfo `json:"event"`
	Payment Payment   `json:"payment"`
	// Reason is why a payment failed, when known; AttemptID is the attempt
	// the event is about.
	Reason    string `json:"reason"`
	AttemptID string `json:"attempt_id"`
	// Raw is the event object as JSON, the same document apps get in the
	// webhook's data.object, for fields this snapshot does not model.
	Raw json.RawMessage `json:"raw"`
}

// Refund is a snapshot of a refund.
type Refund struct {
	ID          string    `json:"id"`
	PaymentID   string    `json:"payment_id"`
	AttemptID   string    `json:"attempt_id"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason"`
	InitiatedBy string    `json:"initiated_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// RefundEvent is what OnRefundSucceeded hooks get.
type RefundEvent struct {
	Event         EventInfo       `json:"event"`
	Refund        Refund          `json:"refund"`
	FailureReason string          `json:"failure_reason"`
	Raw           json.RawMessage `json:"raw"`
}

// Dispute is a snapshot of a dispute (chargeback).
type Dispute struct {
	ID        string    `json:"id"`
	PaymentID string    `json:"payment_id"`
	AttemptID string    `json:"attempt_id"`
	Amount    int64     `json:"amount"`
	Currency  string    `json:"currency"`
	Reason    string    `json:"reason"`
	Status    string    `json:"status"`
	Stage     string    `json:"stage"`
	DueBy     time.Time `json:"due_by"` // zero when the provider gave no deadline
	Source    string    `json:"source"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

// DisputeEvent is what OnDisputeOpened hooks get.
type DisputeEvent struct {
	Event   EventInfo       `json:"event"`
	Dispute Dispute         `json:"dispute"`
	Raw     json.RawMessage `json:"raw"`
}

// Subscription is a snapshot of a subscription.
type Subscription struct {
	ID                 string            `json:"id"`
	Status             string            `json:"status"`
	BillingMode        string            `json:"billing_mode"`
	CustomerID         string            `json:"customer_id"`
	Amount             int64             `json:"amount"`
	Currency           string            `json:"currency"`
	Interval           string            `json:"interval"`
	IntervalCount      int               `json:"interval_count"`
	CurrentPeriodStart time.Time         `json:"current_period_start"`
	CurrentPeriodEnd   time.Time         `json:"current_period_end"`
	RenewalsDone       int               `json:"renewals_done"`
	CancelAtPeriodEnd  bool              `json:"cancel_at_period_end"`
	Metadata           map[string]string `json:"metadata"`
	Version            int64             `json:"version"`
}

// SubscriptionEvent is what OnSubscriptionRenewed hooks get.
type SubscriptionEvent struct {
	Event        EventInfo       `json:"event"`
	Subscription Subscription    `json:"subscription"`
	Reason       string          `json:"reason"`
	Raw          json.RawMessage `json:"raw"`
}

// Invoice is a snapshot of a GST invoice.
type Invoice struct {
	ID             string    `json:"id"`
	Number         string    `json:"number"`
	PaymentID      string    `json:"payment_id"`
	SubscriptionID string    `json:"subscription_id"`
	IssuedAt       time.Time `json:"issued_at"`
	SupplyType     string    `json:"supply_type"`
	PlaceOfSupply  string    `json:"place_of_supply"`
	Currency       string    `json:"currency"`
	Taxable        int64     `json:"taxable"`
	CGST           int64     `json:"cgst"`
	SGST           int64     `json:"sgst"`
	IGST           int64     `json:"igst"`
	Total          int64     `json:"total"`
}

// InvoiceEvent is what OnInvoiceIssued hooks get.
type InvoiceEvent struct {
	Event   EventInfo       `json:"event"`
	Invoice Invoice         `json:"invoice"`
	Raw     json.RawMessage `json:"raw"`
}

// RouteAccount is a provider account routing allows for a payment.
type RouteAccount struct {
	ID        string `json:"id"`        // provider account id, what OnRoute returns
	Connector string `json:"connector"` // for example "razorpay"
	Name      string `json:"name"`
}

// RouteRequest is what OnRoute hooks get: the payment being routed and the
// accounts it may use, in the order routing would try them.
type RouteRequest struct {
	AppID    string            `json:"app_id"`
	Mode     string            `json:"mode"`
	Currency string            `json:"currency"`
	Country  string            `json:"country"`
	Amount   int64             `json:"amount"`
	Purpose  string            `json:"purpose"` // "one_time" or "subscription"
	Metadata map[string]string `json:"metadata"`
	Accounts []RouteAccount    `json:"accounts"`
}

// hookEvent is an event on its way to the hooks; it is also the payload of
// the dispatch job.
type hookEvent struct {
	Type          string          `json:"type"`
	OrgID         string          `json:"org_id"`
	AppID         string          `json:"app_id"`
	Mode          string          `json:"mode"`
	ObjectType    string          `json:"object_type"`
	ObjectID      string          `json:"object_id"`
	ObjectVersion int64           `json:"object_version"`
	Data          json.RawMessage `json:"data"`
}

func (e hookEvent) info() EventInfo {
	return EventInfo{Type: e.Type, OrgID: e.OrgID, AppID: e.AppID, Mode: e.Mode, ObjectType: e.ObjectType, ObjectID: e.ObjectID, ObjectVersion: e.ObjectVersion}
}

type namedHook struct {
	name string
	fn   func(context.Context, hookEvent) error
}

// hookSet holds the registered hooks. Registration may happen at any time.
type hookSet struct {
	mu     sync.RWMutex
	byType map[string][]namedHook
	route  []func(RouteRequest) []string
}

func (h *hookSet) add(typ, name string, fn func(context.Context, hookEvent) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byType == nil {
		h.byType = map[string][]namedHook{}
	}
	h.byType[typ] = append(h.byType[typ], namedHook{name, fn})
}

// wants reports whether any hook listens to the event type.
func (h *hookSet) wants(typ string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byType[typ]) > 0
}

func (h *hookSet) forType(typ string) []namedHook {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return slices.Clone(h.byType[typ])
}

// addHook registers fn for events of type typ; decode turns the event into the
// hook's snapshot, afresh for every hook so hooks cannot affect each other.
func addHook[E any](a *App, typ, name string, decode func(hookEvent) (E, error), fn func(context.Context, E) error) {
	if fn == nil {
		return
	}
	a.hooks.add(typ, name, func(ctx context.Context, ev hookEvent) error {
		e, err := decode(ev)
		if err != nil {
			return fmt.Errorf("decoding %s: %w", ev.Type, err)
		}
		return fn(ctx, e)
	})
}

func decodePayment(ev hookEvent) (PaymentEvent, error) {
	var d struct {
		Payment
		Reason    string `json:"reason"`
		AttemptID string `json:"attempt_id"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return PaymentEvent{}, err
	}
	return PaymentEvent{Event: ev.info(), Payment: d.Payment, Reason: d.Reason, AttemptID: d.AttemptID, Raw: slices.Clone(ev.Data)}, nil
}

func decodeRefund(ev hookEvent) (RefundEvent, error) {
	var d struct {
		Refund
		FailureReason string `json:"failure_reason"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return RefundEvent{}, err
	}
	return RefundEvent{Event: ev.info(), Refund: d.Refund, FailureReason: d.FailureReason, Raw: slices.Clone(ev.Data)}, nil
}

func decodeDispute(ev hookEvent) (DisputeEvent, error) {
	var d Dispute
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return DisputeEvent{}, err
	}
	return DisputeEvent{Event: ev.info(), Dispute: d, Raw: slices.Clone(ev.Data)}, nil
}

func decodeSubscription(ev hookEvent) (SubscriptionEvent, error) {
	var d struct {
		Subscription
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return SubscriptionEvent{}, err
	}
	return SubscriptionEvent{Event: ev.info(), Subscription: d.Subscription, Reason: d.Reason, Raw: slices.Clone(ev.Data)}, nil
}

func decodeInvoice(ev hookEvent) (InvoiceEvent, error) {
	var d Invoice
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return InvoiceEvent{}, err
	}
	return InvoiceEvent{Event: ev.info(), Invoice: d, Raw: slices.Clone(ev.Data)}, nil
}

// OnPaymentSucceeded registers f to run after a payment succeeds.
func (a *App) OnPaymentSucceeded(f func(context.Context, PaymentEvent) error) {
	addHook(a, "payment.succeeded", "OnPaymentSucceeded", decodePayment, f)
}

// OnPaymentFailed registers f to run after a payment attempt fails
// (event payment.failed).
func (a *App) OnPaymentFailed(f func(context.Context, PaymentEvent) error) {
	addHook(a, "payment.failed", "OnPaymentFailed", decodePayment, f)
}

// OnRefundSucceeded registers f to run after a refund succeeds.
func (a *App) OnRefundSucceeded(f func(context.Context, RefundEvent) error) {
	addHook(a, "refund.succeeded", "OnRefundSucceeded", decodeRefund, f)
}

// OnDisputeOpened registers f to run after a dispute is opened.
func (a *App) OnDisputeOpened(f func(context.Context, DisputeEvent) error) {
	addHook(a, "dispute.opened", "OnDisputeOpened", decodeDispute, f)
}

// OnSubscriptionRenewed registers f to run after a subscription renews.
func (a *App) OnSubscriptionRenewed(f func(context.Context, SubscriptionEvent) error) {
	addHook(a, "subscription.renewed", "OnSubscriptionRenewed", decodeSubscription, f)
}

// OnInvoiceIssued registers f to run after an invoice is issued.
func (a *App) OnInvoiceIssued(f func(context.Context, InvoiceEvent) error) {
	addHook(a, "invoice.issued", "OnInvoiceIssued", decodeInvoice, f)
}

// OnRoute registers f to suggest an order for the provider accounts a payment
// may use. It runs inline when the customer opens the pay page, so it must be
// quick and must not block. f returns provider account ids (RouteAccount.ID),
// or connector names meaning every account of that connector, best first;
// accounts it does not name keep their relative order after the named ones.
// It can only reorder: it cannot add an account routing excluded, and an
// account whose circuit breaker is open is still tried last. Returning nil,
// or panicking, leaves the order as it was. With several OnRoute hooks each
// sees the order the previous one produced.
func (a *App) OnRoute(f func(RouteRequest) []string) {
	if f == nil {
		return
	}
	a.hooks.mu.Lock()
	defer a.hooks.mu.Unlock()
	a.hooks.route = append(a.hooks.route, f)
}

// runHooks calls every hook registered for ev.Type, in registration order.
// A failing hook is reported and does not stop the others.
func (a *App) runHooks(ctx context.Context, ev hookEvent) {
	for _, h := range a.hooks.forType(ev.Type) {
		if err := callHook(ctx, h.fn, ev); err != nil {
			a.log.Error("hook failed", "hook", h.name, "event", ev.Type, "object", ev.ObjectID, "err", err)
			if a.hookFailed != nil {
				a.hookFailed(ctx, h.name, ev, err)
			}
		}
	}
}

func callHook(ctx context.Context, fn func(context.Context, hookEvent) error, ev hookEvent) (err error) {
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn(ctx, ev)
}

// suggestRoute applies the OnRoute hooks to the candidate accounts and
// returns the resulting order as account ids, or nil when no hook has an
// opinion.
func (a *App) suggestRoute(_ context.Context, req core.RouteRequest, candidates []store.ProviderAccount) []string {
	a.hooks.mu.RLock()
	fns := slices.Clone(a.hooks.route)
	a.hooks.mu.RUnlock()
	if len(fns) == 0 {
		return nil
	}
	current := slices.Clone(candidates)
	changed := false
	for _, fn := range fns {
		want := callRoute(fn, routeRequest(req, current))
		if len(want) == 0 {
			continue
		}
		changed = true
		next := make([]store.ProviderAccount, 0, len(current))
		placed := map[string]bool{}
		for _, w := range want {
			for _, pa := range current {
				if !placed[pa.ID] && (pa.ID == w || pa.Connector == w) {
					placed[pa.ID] = true
					next = append(next, pa)
				}
			}
		}
		for _, pa := range current {
			if !placed[pa.ID] {
				next = append(next, pa)
			}
		}
		current = next
	}
	if !changed {
		return nil
	}
	ids := make([]string, len(current))
	for i, pa := range current {
		ids[i] = pa.ID
	}
	return ids
}

func routeRequest(req core.RouteRequest, accounts []store.ProviderAccount) RouteRequest {
	r := RouteRequest{AppID: req.Scope.AppID, Mode: req.Scope.Mode, Currency: req.Currency, Country: req.Country, Amount: req.Amount,
		Purpose: req.Purpose, Metadata: map[string]string{}}
	for k, v := range req.Metadata {
		r.Metadata[k] = v
	}
	for _, pa := range accounts {
		r.Accounts = append(r.Accounts, RouteAccount{ID: pa.ID, Connector: pa.Connector, Name: pa.Name})
	}
	return r
}

func callRoute(fn func(RouteRequest) []string, r RouteRequest) (out []string) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	return fn(r)
}

// ---- the post-commit bus ----

const jobHook = "hooks.dispatch"

// hookEmitter wraps the domain-event emitter: after the wrapped emitter has
// recorded the event, and still inside the same transaction, it queues a job
// for the hooks. The job becomes runnable only when the transaction commits,
// and disappears if it rolls back.
type hookEmitter struct {
	next  events.Emitter
	queue *jobs.Queue
	hooks *hookSet
}

func (e hookEmitter) Emit(ctx context.Context, tx store.Tx, ev events.Event) error {
	if err := e.next.Emit(ctx, tx, ev); err != nil {
		return err
	}
	if !e.hooks.wants(ev.Type) {
		return nil
	}
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return fmt.Errorf("marshal event data: %w", err)
	}
	return e.queue.Enqueue(ctx, tx, jobs.Job{Kind: jobHook, MaxAttempts: 3, Payload: hookEvent{Type: ev.Type, OrgID: ev.OrgID, AppID: ev.AppID,
		Mode: ev.Mode, ObjectType: ev.ObjectType, ObjectID: ev.ObjectID, ObjectVersion: ev.ObjectVersion, Data: data}})
}

// registerHookJob makes the queue run hooks for queued events and raises an
// ops alert for each hook that fails.
func (a *App) registerHookJob(q *jobs.Queue, alerts interface {
	Raise(ctx context.Context, al notify.Alert) error
}) {
	if a.hookFailed == nil {
		a.hookFailed = func(ctx context.Context, hook string, ev hookEvent, err error) {
			_ = alerts.Raise(ctx, notify.Alert{Severity: notify.SeverityWarning, Kind: "hook.failed",
				Message: fmt.Sprintf("%s failed for %s %s: %v", hook, ev.ObjectType, ev.ObjectID, err),
				Refs:    map[string]string{"hook": hook, "event": ev.Type, "object": ev.ObjectID}})
		}
	}
	q.Register(jobHook, func(ctx context.Context, payload json.RawMessage) error {
		var ev hookEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			return jobs.Permanent(fmt.Errorf("bad %s payload: %w", jobHook, err))
		}
		a.runHooks(ctx, ev) // never returns the hooks' errors: they are not retried
		return nil
	})
}
