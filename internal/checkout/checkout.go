package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/disputes"
	"github.com/velixio/paymux/internal/logctx"
	"github.com/velixio/paymux/internal/routing"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
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

// PayoutSync handles payout notifications (*payouts.Service).
type PayoutSync interface {
	SyncNotification(ctx context.Context, accountID string, n connector.Notification) error
}

// Options configures a Service.
type Options struct {
	// Payouts, when set, receives payout notifications; without it they are
	// recorded as unknown objects.
	Payouts PayoutSync
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
	// Alerts, when set, is told when a connector answers a checkout with a URL
	// outside the hosts it declared.
	Alerts connectors.Alerter
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
	o       Options
	returns *fetchGate
}

// returnFetchEvery is how often the returns of one attempt may ask the
// provider. The return URL is public to whoever holds it, and every fetch
// spends the account's provider rate limit, which all its checkouts share.
const returnFetchEvery = 5 * time.Second

// fetchGate remembers, in memory, when each attempt last asked the provider.
type fetchGate struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// allow reports whether attempt id may ask the provider now, and if so
// records it. A refused caller shows what is stored; webhooks and the poller
// still catch up.
func (g *fetchGate) allow(id string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.last) > 10_000 { // forget attempts not seen lately
		for k, t := range g.last {
			if now.Sub(t) >= returnFetchEvery {
				delete(g.last, k)
			}
		}
	}
	if t, ok := g.last[id]; ok && now.Sub(t) < returnFetchEvery {
		return false
	}
	g.last[id] = now
	return true
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
	return &Service{o: o, returns: &fetchGate{last: map[string]time.Time{}}}
}

// Started is a checkout that was created at a provider.
type Started struct {
	Attempt   store.Attempt
	AccountID string
	Connector string
	Action    connector.NextAction
	// Hosts are what the account's connector declared (connector.HostsOf): the
	// hosted page's policy allows them and nothing else.
	Hosts connector.CheckoutHosts
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
			Currency: p.Currency, Country: p.Country, Amount: p.Amount, Purpose: routing.PurposeOneTime, Metadata: p.Metadata,
			AllowedConnectors: p.AllowedConnectors})
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
		return Started{Attempt: att, AccountID: pa.ID, Connector: pa.Connector, Action: res.NextAction, Hosts: s.hostsOf(ctx, pa.ID)}, nil
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
		if pa.ID != a.ProviderAccountID {
			continue
		}
		// The action was checked when it was made, but the account's hosts may
		// have changed since (or it predates the check): it is shown again only
		// if the connector as it is now still declares it. Otherwise a fresh
		// checkout is made, which is checked like any other.
		live, err := s.o.Conns.Get(ctx, pa.ID)
		if err != nil || connectors.CheckNextAction(ctx, s.o.Logger, s.o.Alerts, pa.ID, live.Connector, action) != nil {
			return Started{}, false
		}
		return Started{Attempt: a, AccountID: pa.ID, Connector: pa.Connector, Action: action, Hosts: connector.HostsOf(live.Connector)}, true
	}
	return Started{}, false
}

// hostsOf returns the checkout hosts of an account's connector; an account
// that cannot be built declares none, so its page allows no provider host.
func (s *Service) hostsOf(ctx context.Context, accountID string) connector.CheckoutHosts {
	live, err := s.o.Conns.Get(ctx, accountID)
	if err != nil {
		return connector.CheckoutHosts{}
	}
	return connector.HostsOf(live.Connector)
}

// noteAttempt tells the log scope of the request or job which payment and
// attempt it is working on, so a failed provider call or an error row can be
// traced back to them.
func noteAttempt(ctx context.Context, a store.Attempt) {
	logctx.Note(ctx, "payment_id", a.PaymentID, "attempt_id", a.ID, "provider_account_id", a.ProviderAccountID,
		"provider_order_id", a.ProviderOrderID, "provider_payment_id", a.ProviderPaymentID)
}

func (s *Service) createCheckout(ctx context.Context, pa store.ProviderAccount, p store.Payment, att store.Attempt, c store.Customer) (connector.CheckoutResult, error) {
	logctx.Note(ctx, "payment_id", p.ID, "attempt_id", att.ID, "app_id", p.AppID, "org_id", p.OrgID)
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
	// A refusal is an ordinary checkout error: routing goes on with the next account.
	if err := connectors.CheckNextAction(ctx, s.o.Logger, s.o.Alerts, pa.ID, live.Connector, res.NextAction); err != nil {
		return connector.CheckoutResult{}, err
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
		MethodDetails:     s.MethodDetails,
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

// SyncAttemptLatest fetches an attempt by its provider order or merchant
// reference instead of pinning the lookup to a provider payment id already
// stored on the attempt. Some providers allow a failed payment to be retried
// on the same order; an order-level lookup lets the connector select the
// successful retry. The fetched payment is still ownership-checked before it
// can replace the stored provider payment id.
func (s *Service) SyncAttemptLatest(ctx context.Context, attemptID string) (core.ApplyResult, error) {
	a, err := s.o.Core.GetAttempt(ctx, attemptID)
	if err != nil {
		return core.ApplyResult{}, err
	}
	noteAttempt(ctx, a)
	// Preserve the payment currently attached to the Paymux attempt before an
	// order-level lookup can replace it with a later provider retry.
	s.recordStoredProviderPaymentAttempt(ctx, a)
	live, err := s.o.Conns.Get(ctx, a.ProviderAccountID)
	if err != nil {
		return core.ApplyResult{}, err
	}
	ref := connector.PaymentRef{ProviderRef: a.ProviderRef, ProviderOrderID: a.ProviderOrderID}
	if ref.ProviderRef == "" && ref.ProviderOrderID == "" {
		ref.ProviderPaymentID = a.ProviderPaymentID
	}
	s.recordProviderPaymentAttempts(ctx, a, live.Connector, ref)
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
	ours := snapshotIsOurs(a, snap)
	if ours && snap.ProviderPaymentID != "" && snap.ProviderPaymentID != a.ProviderPaymentID {
		ours, err = s.hintedSnapshotIsOurs(ctx, a, snap)
		if err != nil {
			return core.ApplyResult{}, err
		}
	}
	if !ours {
		s.o.Logger.Warn("checkout: the provider returned a payment that is not this attempt's; ignored", "attempt", a.ID,
			"account", a.ProviderAccountID, "provider_payment", snap.ProviderPaymentID)
		p, perr := s.o.Core.GetPaymentByID(ctx, a.PaymentID)
		return core.ApplyResult{Payment: p, Attempt: a}, perr
	}
	s.recordProviderPaymentAttempt(ctx, a, snap)
	return s.o.Core.ApplyAttemptSnapshot(ctx, a.ID, ToSnapshot(snap))
}

func (s *Service) syncAttempt(ctx context.Context, attemptID string, hint connector.PaymentRef) (core.ApplyResult, error) {
	a, err := s.o.Core.GetAttempt(ctx, attemptID)
	if err != nil {
		return core.ApplyResult{}, err
	}
	noteAttempt(ctx, a)
	live, err := s.o.Conns.Get(ctx, a.ProviderAccountID)
	if err != nil {
		return core.ApplyResult{}, err
	}
	ref := connector.PaymentRef{ProviderRef: a.ProviderRef,
		ProviderOrderID: firstNonEmpty(a.ProviderOrderID, hint.ProviderOrderID),
		// One provider order can have a failed payment followed by a successful
		// retry. A verified return or webhook names the payment that changed, so
		// fetch that payment instead of pinning reconciliation to the first ID.
		// The fetched snapshot still has to match this attempt's stored order or
		// reference below before anything is applied.
		ProviderPaymentID: firstNonEmpty(hint.ProviderPaymentID, a.ProviderPaymentID)}
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
	ours := snapshotIsOurs(a, snap)
	hinted := (hint.ProviderPaymentID != "" && hint.ProviderPaymentID != a.ProviderPaymentID) ||
		(a.ProviderOrderID == "" && hint.ProviderOrderID != "")
	if ours && hinted {
		// An id came from outside (a return URL, a notification) and nothing
		// signs it: not contradicting the attempt is not enough, the payment
		// must name it, and must not already be another attempt's.
		ours, err = s.hintedSnapshotIsOurs(ctx, a, snap)
		if err != nil {
			return core.ApplyResult{}, err
		}
	}
	if !ours {
		// The provider answered for an id that was handed to us (a return
		// URL's payment_id, a notification) with a payment that belongs to
		// something else. It is not applied: nothing about this attempt is known.
		s.o.Logger.Warn("checkout: the provider returned a payment that is not this attempt's; ignored", "attempt", a.ID,
			"account", a.ProviderAccountID, "provider_payment", snap.ProviderPaymentID)
		p, perr := s.o.Core.GetPaymentByID(ctx, a.PaymentID)
		return core.ApplyResult{Payment: p, Attempt: a}, perr
	}
	s.recordProviderPaymentAttempt(ctx, a, snap)
	return s.o.Core.ApplyAttemptSnapshot(ctx, a.ID, ToSnapshot(snap))
}

// recordStoredProviderPaymentAttempt preserves the provider payment already
// known by Paymux. History is observational: failure to write it must never
// prevent a payment from being reconciled or booked.
func (s *Service) recordStoredProviderPaymentAttempt(ctx context.Context, a store.Attempt) {
	if a.ProviderPaymentID == "" {
		return
	}
	p := store.ProviderPaymentAttempt{ProviderAccountID: a.ProviderAccountID, ProviderPaymentID: a.ProviderPaymentID,
		AttemptID: a.ID, ProviderOrderID: a.ProviderOrderID, Status: a.Status, Method: a.Method,
		FailureCode: a.FailureCode, FailureMessage: a.FailureMessage, Amount: a.AmountReceived,
		Currency: strings.ToUpper(a.CurrencyReceived), CreatedAt: a.CreatedAt, UpdatedAt: s.o.Now()}
	s.storeProviderPaymentAttempt(ctx, p)
}

func (s *Service) recordProviderPaymentAttempt(ctx context.Context, a store.Attempt, snap connector.PaymentSnapshot) {
	if snap.ProviderPaymentID == "" {
		return
	}
	created := s.o.Now()
	if snap.CreatedAt > 0 {
		created = time.UnixMilli(snap.CreatedAt)
	}
	status := snap.RawStatus
	if status == "" {
		status = string(snap.Status)
	}
	p := store.ProviderPaymentAttempt{ProviderAccountID: a.ProviderAccountID, ProviderPaymentID: snap.ProviderPaymentID,
		AttemptID: a.ID, ProviderOrderID: firstNonEmpty(snap.ProviderOrderID, a.ProviderOrderID), Status: status,
		Method: snap.Method, FailureCode: snap.ErrorCode, FailureMessage: snap.ErrorMessage, Amount: snap.Amount,
		Currency: strings.ToUpper(snap.Currency), CreatedAt: created, UpdatedAt: s.o.Now()}
	s.storeProviderPaymentAttempt(ctx, p)
}

// providerPaymentAttemptFetcher is implemented by connectors that can list
// every payment made against one provider order. It is deliberately optional:
// fetching authoritative state continues to work for all other connectors.
type providerPaymentAttemptFetcher interface {
	FetchPaymentAttempts(context.Context, connector.PaymentRef) ([]connector.PaymentSnapshot, error)
}

func (s *Service) recordProviderPaymentAttempts(ctx context.Context, a store.Attempt, c connector.Connector, ref connector.PaymentRef) {
	f, ok := c.(providerPaymentAttemptFetcher)
	if !ok || ref.ProviderOrderID == "" {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	snaps, err := f.FetchPaymentAttempts(fctx, ref)
	if err != nil {
		s.o.Logger.Warn("checkout: provider payment attempt history could not be fetched", "attempt", a.ID, "err", err)
		return
	}
	for _, snap := range snaps {
		if snapshotIsOurs(a, snap) {
			s.recordProviderPaymentAttempt(ctx, a, snap)
		}
	}
}

func (s *Service) storeProviderPaymentAttempt(ctx context.Context, p store.ProviderPaymentAttempt) {
	if err := s.o.DB.Write(ctx, func(tx store.Tx) error { return store.UpsertProviderPaymentAttempt(ctx, tx, p) }); err != nil {
		s.o.Logger.Warn("checkout: provider payment attempt history could not be recorded", "attempt", p.AttemptID,
			"provider_payment", p.ProviderPaymentID, "err", err)
	}
}

// snapshotIsOurs reports whether a payment fetched from the provider can be
// the attempt's: where both sides name a reference or an order, they agree.
func snapshotIsOurs(a store.Attempt, snap connector.PaymentSnapshot) bool {
	if snap.ProviderRef != "" && a.ProviderRef != "" && snap.ProviderRef != a.ProviderRef {
		return false
	}
	if snap.ProviderOrderID != "" && a.ProviderOrderID != "" && snap.ProviderOrderID != a.ProviderOrderID {
		return false
	}
	return true
}

// hintedSnapshotIsOurs is snapshotIsOurs for a payment fetched by an id the
// attempt did not already hold: the snapshot must carry the attempt's own
// reference or order (what the attempt stored, never a hint), and its payment
// id must not be bound to another attempt of the account.
func (s *Service) hintedSnapshotIsOurs(ctx context.Context, a store.Attempt, snap connector.PaymentSnapshot) (bool, error) {
	named := (snap.ProviderRef != "" && snap.ProviderRef == a.ProviderRef) ||
		(snap.ProviderOrderID != "" && snap.ProviderOrderID == a.ProviderOrderID)
	if !named {
		return false, nil
	}
	if snap.ProviderPaymentID == "" {
		return true, nil
	}
	var other store.Attempt
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		other, err = store.FindAttempt(ctx, q, a.ProviderAccountID, "", "", snap.ProviderPaymentID)
		return
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return true, nil
	case err != nil:
		return false, err
	}
	return other.ID == a.ID, nil
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
	noteAttempt(ctx, a)
	if !s.returns.allow(a.ID, s.o.Now()) {
		// A return of this attempt asked the provider a moment ago: show what is stored.
		return s.o.Core.GetPaymentByID(ctx, a.PaymentID)
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
