package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/disputes"
	"github.com/paymux/paymux/internal/routing"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

// ErrCheckoutFailed means every routed account failed to create a checkout.
var ErrCheckoutFailed = errors.New("checkout: no provider could start a checkout")

const (
	checkoutTimeout = 25 * time.Second
	fetchTimeout    = 25 * time.Second
)

// DisputeSync handles dispute notifications (*disputes.Service).
type DisputeSync interface {
	SyncNotification(ctx context.Context, accountID string, n connector.Notification) (disputes.Result, error)
}

// Options configures a Service.
type Options struct {
	// Disputes, when set, receives dispute notifications; without it they are
	// recorded as unknown objects.
	Disputes DisputeSync
	DB       *store.DB
	Core     *core.Service
	Conns    *connectors.Service
	Routing  *Routing
	Breaker  *routing.Breaker
	Logger   *slog.Logger
	Now      func() time.Time
	// Subs, when set, receives subscription and mandate notifications, learns
	// when a renewal payment changed, and handles returns from subscription
	// and mandate checkouts (internal/subscriptions implements it).
	Subs SubscriptionSink
}

// ErrUnknownObject is wrapped by a SubscriptionSink when a notification names
// a provider object Paymux does not know; the event is then recorded as
// unknown instead of being retried.
var ErrUnknownObject = errors.New("checkout: unknown provider object")

// SubscriptionSink is the subscriptions engine as the payment flow sees it.
type SubscriptionSink interface {
	// HandleNotification applies a subscription or mandate notification.
	HandleNotification(ctx context.Context, providerAccountID string, n connector.Notification) error
	// ReconcilePayment settles the subscription cycle of a renewal payment
	// whose attempt just changed; it is a no-op for other payments.
	ReconcilePayment(ctx context.Context, paymentID string) error
	// ReturnRedirect handles the customer returning from a subscription or
	// mandate checkout. handled is false when providerRef is not one of them.
	ReturnRedirect(ctx context.Context, providerAccountID, providerRef string) (url string, handled bool, err error)
}

// SubscriptionReturn handles a return that belongs to a subscription or
// mandate checkout, if it is one. url is where to send the customer ("" when
// the app gave no return URL).
func (s *Service) SubscriptionReturn(ctx context.Context, accountID, providerRef string) (url string, handled bool, err error) {
	if s.o.Subs == nil {
		return "", false, nil
	}
	return s.o.Subs.ReturnRedirect(ctx, accountID, providerRef)
}

// Service runs the payment flow around the connectors.
type Service struct {
	o Options
}

// New builds a Service.
func New(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Breaker == nil {
		o.Breaker = routing.NewBreaker(o.Now)
	}
	if o.Routing == nil {
		o.Routing = NewRouting(o.Breaker)
	}
	return &Service{o: o}
}

// Started is a checkout that was created at a provider.
type Started struct {
	Attempt   store.Attempt
	AccountID string
	Connector string
	Action    connector.NextAction
}

// StartCheckout routes the payment, creates an attempt on the first routed
// account and asks the connector for a checkout. When CreateCheckout errors
// (or the account's breaker is open and others exist) it records the failure
// and falls back to the next account; the payment itself only fails when
// every account failed. It never falls back after a customer-side failure.
func (s *Service) StartCheckout(ctx context.Context, paymentID string) (Started, error) {
	return s.startCheckout(ctx, paymentID, false)
}

// StartFreshCheckout is StartCheckout for a customer who asked to try again or
// to pay another way: it always creates a new attempt, even while an earlier
// one is still open (which can still be paid).
func (s *Service) StartFreshCheckout(ctx context.Context, paymentID string) (Started, error) {
	return s.startCheckout(ctx, paymentID, true)
}

// reuseWindow is how long a checkout is shown again when the provider gave no
// expiry of its own.
const reuseWindow = 15 * time.Minute

func (s *Service) startCheckout(ctx context.Context, paymentID string, fresh bool) (Started, error) {
	p, err := s.o.Core.GetPaymentByID(ctx, paymentID)
	if err != nil {
		return Started{}, err
	}
	switch state.PaymentStatus(p.Status) {
	case state.PaymentCanceled, state.PaymentExpired, state.PaymentNeedsReview:
		return Started{}, core.NewError(core.ErrInvalidState, "a payment that is %s cannot be paid", p.Status)
	}
	var accounts []store.ProviderAccount
	err = s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		accounts, err = s.o.Routing.Pick(ctx, q, core.RouteRequest{Scope: core.Scope{AppID: p.AppID, Mode: p.Mode},
			Currency: p.Currency, Amount: p.Amount, Purpose: routing.PurposeOneTime, Metadata: p.Metadata})
		return
	})
	if err != nil {
		return Started{}, err
	}
	if !fresh {
		if st, ok := s.reusable(ctx, p.ID, accounts); ok {
			return st, nil
		}
	}
	var cust store.Customer
	if p.CustomerID != "" {
		if cust, err = s.o.Core.GetCustomer(ctx, p.CustomerID); err != nil {
			return Started{}, err
		}
	}

	var prev *store.Attempt
	var lastErr error
	for _, pa := range accounts {
		att, err := s.o.Core.CreateAttempt(ctx, p.ID, pa.ID)
		if err != nil {
			return Started{}, err
		}
		// Fail the previous attempt only once the next one exists, so the
		// payment is not declared failed while a fallback is still possible.
		if prev != nil {
			s.failAttempt(ctx, *prev, lastErr)
		}
		prev = &att

		res, err := s.createCheckout(ctx, pa, p, att, cust)
		if err != nil {
			s.o.Breaker.Failure(pa.ID)
			s.o.Logger.Warn("checkout: create failed, trying next account", "provider_account", pa.ID,
				"payment", p.ID, "err", err)
			lastErr = err
			continue
		}
		s.o.Breaker.Success(pa.ID)
		expires := s.o.Now().Add(reuseWindow)
		if res.ExpiresAt > 0 {
			expires = time.UnixMilli(res.ExpiresAt)
		}
		if action, merr := json.Marshal(res.NextAction); merr == nil {
			if err := s.o.Core.RecordCheckoutAction(ctx, att.ID, res.ProviderOrderID, action, expires); err != nil {
				return Started{}, err
			}
		} else if res.ProviderOrderID != "" {
			if err := s.o.Core.RecordCheckout(ctx, att.ID, res.ProviderOrderID); err != nil {
				return Started{}, err
			}
		}
		att.ProviderOrderID = res.ProviderOrderID
		return Started{Attempt: att, AccountID: pa.ID, Connector: pa.Connector, Action: res.NextAction}, nil
	}
	if prev != nil {
		s.failAttempt(ctx, *prev, lastErr)
	}
	return Started{}, fmt.Errorf("%w: %v", ErrCheckoutFailed, lastErr)
}

// reusable returns the newest open attempt's checkout when it can be shown
// again: it is on an account routing still allows, its checkout has not
// expired, and its stored next action is intact.
func (s *Service) reusable(ctx context.Context, paymentID string, accounts []store.ProviderAccount) (Started, bool) {
	a, err := s.o.Core.LatestOpenAttempt(ctx, paymentID)
	if err != nil || a.NextAction == "" || !s.o.Now().Before(a.CheckoutExpiresAt) {
		return Started{}, false
	}
	var action connector.NextAction
	if json.Unmarshal([]byte(a.NextAction), &action) != nil || !action.Valid() {
		return Started{}, false
	}
	for _, pa := range accounts {
		if pa.ID == a.ProviderAccountID {
			return Started{Attempt: a, AccountID: pa.ID, Connector: pa.Connector, Action: action}, true
		}
	}
	return Started{}, false
}

func (s *Service) createCheckout(ctx context.Context, pa store.ProviderAccount, p store.Payment, att store.Attempt, c store.Customer) (connector.CheckoutResult, error) {
	live, err := s.o.Conns.Get(ctx, pa.ID)
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, checkoutTimeout)
	defer cancel()
	res, err := live.Connector.CreateCheckout(cctx, connector.CheckoutRequest{
		PaymentID: p.ID, AttemptID: att.ID, AppID: p.AppID, ProviderRef: att.ProviderRef, Amount: p.Amount,
		Currency: p.Currency, Description: p.Description, Metadata: p.Metadata,
		Customer: connector.Customer{Name: c.Name, Email: c.Email, Phone: c.Phone},
	})
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	if !res.NextAction.Valid() {
		return connector.CheckoutResult{}, fmt.Errorf("connector %s returned an invalid next action", pa.Connector)
	}
	return res, nil
}

func (s *Service) failAttempt(ctx context.Context, a store.Attempt, cause error) {
	msg := "checkout failed"
	if cause != nil {
		msg = cause.Error()
	}
	_, err := s.o.Core.ApplyAttemptSnapshot(ctx, a.ID, core.Snapshot{Status: state.AttemptFailed, FailureCode: "checkout_error", FailureMessage: msg})
	if err != nil {
		s.o.Logger.Error("checkout: marking attempt failed", "attempt", a.ID, "err", err)
	}
}

// ToSnapshot maps a connector payment snapshot to a core attempt snapshot.
// Not-yet-paid states become pending, authorized and processing become
// authorized, and canceled and expired become failed.
func ToSnapshot(s connector.PaymentSnapshot) core.Snapshot {
	out := core.Snapshot{Amount: s.Amount, Currency: strings.ToUpper(s.Currency), Method: s.Method,
		ProviderPaymentID: s.ProviderPaymentID, ProviderOrderID: s.ProviderOrderID, FailureCode: s.ErrorCode, FailureMessage: s.ErrorMessage}
	switch s.Status {
	case connector.PaymentSucceeded:
		out.Status = state.AttemptCaptured
	case connector.PaymentFailed, connector.PaymentCanceled, connector.PaymentExpired:
		out.Status = state.AttemptFailed
		if out.FailureCode == "" {
			out.FailureCode = string(s.Status)
		}
	case connector.PaymentAuthorized, connector.PaymentProcessing:
		out.Status = state.AttemptAuthorized
	default: // created, requires_action
		out.Status = state.AttemptPending
	}
	return out
}

// SyncAttempt fetches the provider's truth for an attempt and applies it.
// hint carries identifiers learned from a verified return or webhook; empty
// fields fall back to what the attempt already stores. A provider that does
// not know the attempt yet (ErrNotFound) leaves everything unchanged.
func (s *Service) SyncAttempt(ctx context.Context, attemptID string) (core.ApplyResult, error) {
	return s.syncAttempt(ctx, attemptID, connector.PaymentRef{})
}

func (s *Service) syncAttempt(ctx context.Context, attemptID string, hint connector.PaymentRef) (core.ApplyResult, error) {
	a, err := s.o.Core.GetAttempt(ctx, attemptID)
	if err != nil {
		return core.ApplyResult{}, err
	}
	live, err := s.o.Conns.Get(ctx, a.ProviderAccountID)
	if err != nil {
		return core.ApplyResult{}, err
	}
	ref := connector.PaymentRef{ProviderRef: a.ProviderRef,
		ProviderOrderID:   firstNonEmpty(a.ProviderOrderID, hint.ProviderOrderID),
		ProviderPaymentID: firstNonEmpty(a.ProviderPaymentID, hint.ProviderPaymentID)}
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	snap, err := live.Connector.FetchPayment(fctx, ref)
	if errors.Is(err, connector.ErrNotFound) {
		p, perr := s.o.Core.GetPaymentByID(ctx, a.PaymentID)
		return core.ApplyResult{Payment: p, Attempt: a}, perr
	}
	if err != nil {
		return core.ApplyResult{}, err
	}
	return s.o.Core.ApplyAttemptSnapshot(ctx, a.ID, ToSnapshot(snap))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ReturnInput is the customer's return request as received.
type ReturnInput struct {
	Method  string
	Query   map[string][]string
	Form    map[string][]string
	Headers map[string][]string
	Body    []byte
}

// HandleReturn processes the customer coming back from a provider. The
// connector verifies what the provider signs; only a verified return
// contributes provider ids. Either way the state change comes from fetching
// the truth, never from the redirect. It returns the payment as it is after.
func (s *Service) HandleReturn(ctx context.Context, accountID, providerRef string, in ReturnInput) (store.Payment, error) {
	var a store.Attempt
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		a, err = store.FindAttempt(ctx, q, accountID, providerRef, "", "")
		return
	})
	if errors.Is(err, store.ErrNotFound) {
		return store.Payment{}, core.NewError(core.ErrNotFound, "unknown payment attempt")
	}
	if err != nil {
		return store.Payment{}, err
	}
	live, err := s.o.Conns.Get(ctx, accountID)
	if err != nil {
		return store.Payment{}, err
	}
	var hint connector.PaymentRef
	rr, rerr := live.Connector.HandleReturn(ctx, connector.ReturnRequest{ProviderRef: providerRef, Method: in.Method,
		Query: in.Query, Form: in.Form, Headers: in.Headers, Body: in.Body})
	switch {
	case rerr == nil:
		hint = connector.PaymentRef{ProviderOrderID: rr.ProviderOrderID, ProviderPaymentID: rr.ProviderPaymentID}
	case errors.Is(rerr, connector.ErrInvalidReturn):
		s.o.Logger.Warn("checkout: return signature invalid; relying on fetched state only", "attempt", a.ID)
	default:
		return store.Payment{}, rerr
	}
	res, err := s.syncAttempt(ctx, a.ID, hint)
	if err != nil {
		// The provider may be briefly unreachable: show the current state;
		// webhooks and the poller catch up.
		s.o.Logger.Warn("checkout: fetch after return failed", "attempt", a.ID, "err", err)
		return s.o.Core.GetPaymentByID(ctx, a.PaymentID)
	}
	return res.Payment, nil
}

// LiveConnector returns the built connector of a provider account, for the few
// pages that talk to a connector directly (the simulator's hosted page).
func (s *Service) LiveConnector(ctx context.Context, accountID string) (connectors.Live, error) {
	return s.o.Conns.Get(ctx, accountID)
}
