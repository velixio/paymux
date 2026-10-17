// Package core is the payments domain service: creating, reading, cancelling
// and refunding payments, and applying provider attempt snapshots through the
// state machines. Every state change runs in one store transaction that bumps
// the version, writes ledger entries, emits events and enqueues jobs.
package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
)

// Error kinds. Errors returned by Service wrap exactly one of these.
var (
	ErrInvalid       = errors.New("invalid request")
	ErrNotFound      = errors.New("not found")
	ErrInvalidState  = errors.New("invalid state")
	ErrNotRefundable = errors.New("not refundable")
	ErrRefundExceeds = errors.New("refund exceeds refundable balance")
	// ErrNoRoute means no provider account can take the payment.
	ErrNoRoute = errors.New("no route")
	// ErrCurrencyNotAccepted means a routing rule rejects the payment.
	ErrCurrencyNotAccepted = errors.New("currency not accepted")
)

// NewError builds a domain error of the given kind (for Router implementations).
func NewError(kind error, format string, a ...any) error { return newErr(kind, format, a...) }

// RouteRequest describes a payment to be routed (spec section 8).
type RouteRequest struct {
	Scope             Scope
	Currency, Country string
	Amount            int64
	Purpose           string // one_time | subscription
	Metadata          map[string]string
	AllowedConnectors []string
}

// Router validates at create time that a payment can be routed. It returns a
// domain error of kind ErrNoRoute or ErrCurrencyNotAccepted when it cannot.
type Router interface {
	CheckRoute(ctx context.Context, q store.Queryer, req RouteRequest) error
}

// Error is a typed domain error: Kind for matching with errors.Is, Message
// for humans (and the API).
type Error struct {
	Kind    error
	Message string
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Kind }

func newErr(kind error, format string, a ...any) error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, a...)}
}

// mapStoreErr turns store.ErrNotFound into a domain ErrNotFound naming what.
func mapStoreErr(err error, what string) error {
	if errors.Is(err, store.ErrNotFound) {
		return newErr(ErrNotFound, "%s not found", what)
	}
	return err
}

// Scope is the tenant a call acts for: an app in one mode (derived from the
// API key). Reads outside the scope behave as not found.
type Scope struct {
	AppID string
	Mode  string // "test" | "live"
}

func (s Scope) validate() error {
	if s.AppID == "" || (s.Mode != "test" && s.Mode != "live") {
		return newErr(ErrInvalid, "scope needs an app and a mode of test or live")
	}
	return nil
}

// Enqueuer enqueues a job inside the caller's transaction (*jobs.Queue).
type Enqueuer interface {
	Enqueue(ctx context.Context, tx store.Tx, j jobs.Job) error
}

// Options configures a Service.
type Options struct {
	DB       *store.DB
	Settings *settings.Service
	Jobs     Enqueuer
	Router   Router // nil skips create-time routing validation
	// Emitter receives domain events inside the state-changing transaction.
	// Nil means events are dropped (see NoopEmitter).
	Emitter events.Emitter
	Now     func() time.Time
}

// NoopEmitter drops events. It stands in until the outbound-webhooks lane
// provides the real emitter.
var NoopEmitter events.Emitter = events.EmitterFunc(func(context.Context, store.Tx, events.Event) error { return nil })

// Service implements the payments operations.
type Service struct {
	db       *store.DB
	settings *settings.Service
	jobs     Enqueuer
	router   Router
	emitter  events.Emitter
	now      func() time.Time
}

// NewService builds a Service.
func NewService(o Options) *Service {
	if o.Emitter == nil {
		o.Emitter = NoopEmitter
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{db: o.DB, settings: o.Settings, jobs: o.Jobs, router: o.Router, emitter: o.Emitter, now: o.Now}
}

func (s *Service) nowUTC() time.Time { return s.now().UTC() }
