// Package subscriptions implements recurring billing (spec 6.8): creating
// subscriptions and mandates, the merchant-triggered renewal schedule with
// pre-debit notices, charging and dunning, provider-managed subscriptions
// driven by provider notifications, and the hosted /s checkout.
//
// Locking: every change to a subscription or its cycles happens in a store
// transaction that first locks the subscription row (SELECT ... FOR UPDATE on
// Postgres; SQLite has a single writer). That makes the cycle state machine
// below race-free without any in-process mutex.
package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/paymux/paymux/internal/billing"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/web"
	"github.com/paymux/paymux/pkg/connector"
)

// Error kinds. Errors returned by Service wrap exactly one of these.
var (
	ErrInvalid                   = errors.New("invalid request")
	ErrNotFound                  = errors.New("not found")
	ErrInvalidState              = errors.New("invalid state")
	ErrAmountExceedsMandateLimit = errors.New("amount exceeds mandate limit")
	ErrNoRoute                   = errors.New("no route")
	ErrUnsupported               = errors.New("unsupported")
)

// Error is a typed domain error: Kind for errors.Is, Message for humans and the API.
type Error struct {
	Kind    error
	Message string
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Kind }

func newErr(kind error, format string, a ...any) error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, a...)}
}

func mapNotFound(err error, what string) error {
	if errors.Is(err, store.ErrNotFound) {
		return newErr(ErrNotFound, "%s not found", what)
	}
	return err
}

// Job kinds registered by RegisterJobs.
const (
	JobNotifyPreDebit   = "subscription.notify_predebit"
	JobChargeRenewal    = "subscription.charge_renewal"
	JobPollRenewal      = "subscription.poll_renewal"
	JobExpireIncomplete = "subscription.expire_incomplete"
	JobMandateExpiry    = "mandate.expiry"
	JobRevokeMandate    = "mandate.revoke"
	JobCancelAtProvider = "subscription.cancel_at_provider"
)

// Event types.
const (
	EvCreated               = "subscription.created"
	EvActivated             = "subscription.activated"
	EvRenewed               = "subscription.renewed"
	EvRenewalUpcoming       = "subscription.renewal_upcoming"
	EvPastDue               = "subscription.past_due"
	EvCanceled              = "subscription.canceled"
	EvEnded                 = "subscription.ended"
	EvIncompleteExpired     = "subscription.incomplete_expired"
	EvUpdated               = "subscription.updated"
	EvReauthorizationNeeded = "subscription.reauthorization_required"
	EvMandateCreated        = "mandate.created"
	EvMandateActivated      = "mandate.activated"
	EvMandateRevoked        = "mandate.revoked"
	EvMandateExpired        = "mandate.expired"
	EvMandateFailed         = "mandate.failed"
)

// Connectors gives access to the connector instance of a provider account.
type Connectors interface {
	Get(ctx context.Context, providerAccountID string) (connector.Connector, error)
}

// ConnectorsFunc adapts a function to Connectors.
type ConnectorsFunc func(ctx context.Context, providerAccountID string) (connector.Connector, error)

// Get calls f.
func (f ConnectorsFunc) Get(ctx context.Context, id string) (connector.Connector, error) {
	return f(ctx, id)
}

// Enqueuer enqueues a job inside the caller's transaction (*jobs.Queue).
type Enqueuer interface {
	Enqueue(ctx context.Context, tx store.Tx, j jobs.Job) error
}

// Options configures a Service.
type Options struct {
	DB         *store.DB
	Core       *core.Service
	Settings   *settings.Service
	Jobs       Enqueuer
	Emitter    events.Emitter
	Connectors Connectors
	// BaseURL is the origin of checkout_url (PAYMUX_BASE_URL).
	BaseURL string
	Now     func() time.Time
	// Pages renders the hosted /s views; nil uses the embedded templates.
	Pages *web.Renderer
	// Mailer sends the customer pre-debit notice; nil sends nothing.
	Mailer notify.Mailer
	// Templates is where email templates are looked up first (the overlay with
	// PAYMUX_DATA_DIR/custom); nil or a missing file falls back to the defaults.
	Templates fs.FS
	Logger    *slog.Logger
}

// Service implements the subscription operations.
type Service struct {
	db         *store.DB
	core       *core.Service
	settings   *settings.Service
	jobs       Enqueuer
	emitter    events.Emitter
	connectors Connectors
	baseURL    string
	now        func() time.Time
	mailer     notify.Mailer
	templates  fs.FS
	log        *slog.Logger

	pagesOnce sync.Once
	pagesR    *web.Renderer
	pagesErr  error
	pagesOpt  *web.Renderer
}

// New builds a Service.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Emitter == nil {
		o.Emitter = events.EmitterFunc(func(context.Context, store.Tx, events.Event) error { return nil })
	}
	return &Service{db: o.DB, core: o.Core, settings: o.Settings, jobs: o.Jobs, emitter: o.Emitter, connectors: o.Connectors,
		baseURL: strings.TrimRight(o.BaseURL, "/"), now: o.Now, pagesOpt: o.Pages, mailer: o.Mailer, templates: o.Templates, log: o.Logger}
}

// SetTemplates sets where email templates are looked up first (call before serving).
func (s *Service) SetTemplates(f fs.FS) { s.templates = f }

// SetPages sets the HTML renderer of the hosted views (call before serving).
func (s *Service) SetPages(r *web.Renderer) { s.pagesOpt = r }

func (s *Service) pages() (*web.Renderer, error) {
	if s.pagesOpt != nil {
		return s.pagesOpt, nil
	}
	s.pagesOnce.Do(func() { s.pagesR, s.pagesErr = web.NewRenderer(web.Embedded()) })
	return s.pagesR, s.pagesErr
}

func (s *Service) nowUTC() time.Time { return s.now().UTC() }

// IncompleteTTL is how long a subscription may stay incomplete.
const IncompleteTTL = 24 * time.Hour

// lockOrder: callers pass the subscription id; the tx helper below locks it.
func (s *Service) lockSub(ctx context.Context, tx store.Tx, id string) (store.Subscription, error) {
	sub, err := store.GetSubscriptionForUpdate(ctx, tx, id)
	return sub, mapNotFound(err, "subscription")
}

// ---- settings --------------------------------------------------------

func resolve[T any](ctx context.Context, q store.Queryer, appID, key string) (T, error) {
	var out T
	v, err := settings.ResolveWith(ctx, q, appID, key)
	if err != nil {
		return out, err
	}
	err = v.Decode(&out)
	return out, err
}

type policy struct {
	predebitLead  time.Duration
	dunning       []time.Duration
	finalAction   string
	headroomBP    int64
	afaLimit      int64
	validityYears int
	peaks         []billing.Window
}

// emandateLimit is the e-NACH ceiling (Rs 1 crore) in paise (RBI e-mandate framework).
const emandateLimit int64 = 1_000_000_000

func (s *Service) loadPolicy(ctx context.Context, q store.Queryer, appID string) (policy, error) {
	var p policy
	lead, err := resolve[int64](ctx, q, appID, "predebit_lead")
	if err != nil {
		return p, err
	}
	p.predebitLead = time.Duration(lead) * time.Hour
	hours, err := resolve[[]int64](ctx, q, appID, "dunning_schedule")
	if err != nil {
		return p, err
	}
	for _, h := range hours {
		p.dunning = append(p.dunning, time.Duration(h)*time.Hour)
	}
	if p.finalAction, err = resolve[string](ctx, q, appID, "dunning_final_action"); err != nil {
		return p, err
	}
	hr, err := resolve[float64](ctx, q, appID, "mandate_headroom")
	if err != nil {
		return p, err
	}
	p.headroomBP = int64(hr*10000 + 0.5)
	if p.afaLimit, err = resolve[int64](ctx, q, appID, "mandate_afa_limit"); err != nil {
		return p, err
	}
	years, err := resolve[int64](ctx, q, appID, "mandate_validity")
	if err != nil {
		return p, err
	}
	p.validityYears = int(years)
	var wins []struct{ Start, End string }
	raw, err := settings.ResolveWith(ctx, q, appID, "upi_execution_windows")
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(raw.Raw, &wins); err != nil {
		return p, err
	}
	for _, w := range wins {
		from, to := clockMinutes(w.Start), clockMinutes(w.End)
		p.peaks = append(p.peaks, billing.Window{From: from, To: to})
	}
	return p, nil
}

func clockMinutes(hhmm string) int {
	var h, m int
	fmt.Sscanf(hhmm, "%d:%d", &h, &m)
	return h*60 + m
}

// methodLimit is the largest single debit the regulation allows for a mandate
// method: card and UPI up to mandate_afa_limit, e-NACH up to Rs 1 crore.
func (p policy) methodLimit(method string) int64 {
	if method == string(billing.MethodEMandate) {
		return emandateLimit
	}
	return p.afaLimit
}

// ---- views -------------------------------------------------------------

// View is the public JSON shape of a subscription.
type View struct {
	ID                      string            `json:"id"`
	Object                  string            `json:"object"`
	Status                  string            `json:"status"`
	BillingMode             string            `json:"billing_mode"`
	CustomerID              string            `json:"customer_id"`
	Amount                  int64             `json:"amount"`
	Currency                string            `json:"currency"`
	Interval                string            `json:"interval"`
	IntervalCount           int               `json:"interval_count"`
	CurrentPeriodStart      time.Time         `json:"current_period_start"`
	CurrentPeriodEnd        time.Time         `json:"current_period_end"`
	TrialEnd                *time.Time        `json:"trial_end,omitempty"`
	TotalCount              int               `json:"total_count,omitempty"`
	RenewalsDone            int               `json:"renewals_done"`
	CancelAtPeriodEnd       bool              `json:"cancel_at_period_end"`
	CanceledAt              *time.Time        `json:"canceled_at,omitempty"`
	EndedAt                 *time.Time        `json:"ended_at,omitempty"`
	PendingAmount           int64             `json:"pending_amount,omitempty"`
	RequiresReauthorization bool              `json:"requires_reauthorization"`
	Description             string            `json:"description,omitempty"`
	Metadata                map[string]string `json:"metadata"`
	ReturnURL               string            `json:"return_url,omitempty"`
	MandateID               string            `json:"mandate_id,omitempty"`
	CreatedAt               time.Time         `json:"created_at"`
	Version                 int64             `json:"version"`
}

func tp(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ViewSubscription renders sub.
func ViewSubscription(sub store.Subscription) View {
	meta := sub.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	return View{ID: sub.ID, Object: "subscription", Status: sub.Status, BillingMode: sub.BillingMode, CustomerID: sub.CustomerID,
		Amount: sub.Amount, Currency: sub.Currency, Interval: sub.Interval, IntervalCount: sub.IntervalCount,
		CurrentPeriodStart: sub.CurrentPeriodStart, CurrentPeriodEnd: sub.CurrentPeriodEnd, TrialEnd: tp(sub.TrialEnd),
		TotalCount: sub.TotalCount, RenewalsDone: sub.RenewalsDone, CancelAtPeriodEnd: sub.CancelAtPeriodEnd,
		CanceledAt: tp(sub.CanceledAt), EndedAt: tp(sub.EndedAt), PendingAmount: sub.PendingAmount,
		RequiresReauthorization: sub.RequiresReauthorization, Description: sub.Description, Metadata: meta,
		ReturnURL: sub.ReturnURL, MandateID: sub.MandateID, CreatedAt: sub.CreatedAt, Version: sub.Version}
}

// MandateView is the public JSON shape of a mandate (events only).
type MandateView struct {
	ID         string     `json:"id"`
	Object     string     `json:"object"`
	Status     string     `json:"status"`
	Method     string     `json:"method"`
	MaxAmount  int64      `json:"max_amount"`
	Currency   string     `json:"currency"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`
	CustomerID string     `json:"customer_id"`
}

func viewMandate(m store.Mandate) MandateView {
	return MandateView{ID: m.ID, Object: "mandate", Status: m.Status, Method: m.Method, MaxAmount: m.MaxAmount,
		Currency: m.Currency, ValidUntil: tp(m.ValidUntil), CustomerID: m.CustomerID}
}

// eventData is the data of subscription.* events: the subscription plus event-specific extras.
type eventData struct {
	View
	Reason        string     `json:"reason,omitempty"`
	CheckoutURL   string     `json:"checkout_url,omitempty"`
	ChargeAt      *time.Time `json:"charge_at,omitempty"`
	RenewalAmount int64      `json:"renewal_amount,omitempty"`
}

func (s *Service) emitSub(ctx context.Context, tx store.Tx, typ string, sub store.Subscription, d eventData) error {
	d.View = ViewSubscription(sub)
	return s.emitter.Emit(ctx, tx, events.Event{OrgID: sub.OrgID, AppID: sub.AppID, Mode: sub.Mode, Type: typ,
		ObjectType: "subscription", ObjectID: sub.ID, ObjectVersion: sub.Version, Data: d})
}

func (s *Service) emitMandate(ctx context.Context, tx store.Tx, typ string, m store.Mandate) error {
	return s.emitter.Emit(ctx, tx, events.Event{OrgID: m.OrgID, AppID: m.AppID, Mode: m.Mode, Type: typ,
		ObjectType: "mandate", ObjectID: m.ID, ObjectVersion: 1, Data: viewMandate(m)})
}

// Get returns a subscription of the scope's app and mode.
func (s *Service) Get(ctx context.Context, sc core.Scope, id string) (store.Subscription, error) {
	var sub store.Subscription
	err := s.db.Read(ctx, func(q store.Querier) (err error) { sub, err = store.GetSubscription(ctx, q, id); return })
	if err != nil || sub.AppID != sc.AppID || sub.Mode != sc.Mode {
		return store.Subscription{}, newErr(ErrNotFound, "subscription not found")
	}
	return sub, nil
}

// ListParams filters List.
type ListParams struct {
	CustomerID, Status, StartingAfter string
	Limit                             int
}

// Page is one page of subscriptions.
type Page struct {
	Data       []store.Subscription
	HasMore    bool
	NextCursor string
}

// List returns the scope's subscriptions, newest first.
func (s *Service) List(ctx context.Context, sc core.Scope, lp ListParams) (Page, error) {
	if lp.Limit <= 0 {
		lp.Limit = 20
	}
	if lp.Limit > 100 {
		lp.Limit = 100
	}
	var rows []store.Subscription
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		rows, err = store.ListSubscriptions(ctx, q, sc.AppID, sc.Mode, lp.CustomerID, lp.Status, lp.StartingAfter, lp.Limit+1)
		return
	})
	if err != nil {
		return Page{}, err
	}
	pg := Page{Data: rows}
	if len(rows) > lp.Limit {
		pg.Data, pg.HasMore = rows[:lp.Limit], true
		pg.NextCursor = rows[lp.Limit-1].ID
	}
	return pg, nil
}
