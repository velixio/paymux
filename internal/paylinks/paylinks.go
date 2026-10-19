// Package paylinks implements payment links: a shareable URL (/l/<slug>) that,
// when a customer opens it and says who they are, creates a normal payment and
// sends them into the existing pay flow. A link has a fixed price or lets the
// customer choose within limits, may be reusable or single-use, and can expire
// or be switched off.
package paylinks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector/money"
)

// Event types.
const (
	EventCreated     = "payment_link.created"
	EventDeactivated = "payment_link.deactivated"
)

// MetadataKey is the payment metadata key that names the link a payment came from.
const MetadataKey = "payment_link_id"

// Deactivation reasons.
const (
	ReasonManual = "manual"
	ReasonUsed   = "used"
)

// Errors, matched with errors.Is.
var (
	ErrInvalid     = errors.New("paylinks: invalid input")
	ErrNotFound    = errors.New("paylinks: not found")
	ErrInactive    = errors.New("paylinks: the link is no longer active")
	ErrExpired     = errors.New("paylinks: the link has expired")
	ErrBusy        = errors.New("paylinks: the link is being used by someone else")
	ErrRateLimited = errors.New("paylinks: too many requests for this link")
	ErrUnavailable = errors.New("paylinks: no payment method is available")
)

// Error is a typed error: Kind for errors.Is, Message for people and the API.
type Error struct {
	Kind    error
	Message string
	// Key and Args name the message in the i18n catalogs, for the errors a
	// customer sees on the hosted page (empty for API-only errors).
	Key  string
	Args []any
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Kind }

func newErr(kind error, format string, a ...any) error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, a...)}
}

// newKeyed is an error whose message the hosted page translates from key.
func newKeyed(kind error, key string, args []any, message string) error {
	return &Error{Kind: kind, Message: message, Key: key, Args: args}
}

const (
	maxAmount        = int64(1) << 50
	maxText          = 1000
	maxMetadataKeys  = 19 // core allows 20; one is the link id
	maxMetadataKey   = 40
	maxMetadataValue = 500
	maxLifetime      = 5 * 365 * 24 * time.Hour
	slugLen          = 12
	defaultStarts    = 60
)

// Options configures the Service.
type Options struct {
	DB      *store.DB
	Core    *core.Service
	Emitter events.Emitter
	// Settings gives payment_ttl, how long a single-use link stays held by the
	// visitor whose payment is open (default 60 minutes without it).
	Settings *settings.Service
	BaseURL  string // PAYMUX_BASE_URL, the origin of link URLs
	Now      func() time.Time
	// StartsPerMinute bounds payments created through one link per minute
	// (default 60): the form is public, so it must not be a way to fill the
	// database.
	StartsPerMinute int
}

// Service manages payment links.
type Service struct {
	db       *store.DB
	core     *core.Service
	emitter  events.Emitter
	settings *settings.Service
	baseURL  string
	now      func() time.Time
	perMin   int

	mu      sync.Mutex
	windows map[string]*window
}

type window struct {
	start time.Time
	n     int
}

// New builds a Service.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.StartsPerMinute <= 0 {
		o.StartsPerMinute = defaultStarts
	}
	return &Service{db: o.DB, core: o.Core, emitter: o.Emitter, settings: o.Settings, baseURL: strings.TrimRight(o.BaseURL, "/"),
		now: func() time.Time { return o.Now().UTC() }, perMin: o.StartsPerMinute, windows: map[string]*window{}}
}

// URL is the public address of a link.
func (s *Service) URL(l store.PaymentLink) string { return s.baseURL + "/l/" + l.Slug }

// View is the public JSON shape of a payment link, used by the API and inside events.
type View struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	Livemode          bool              `json:"livemode"` // false for a test-mode object
	URL               string            `json:"url"`
	Slug              string            `json:"slug"`
	AppID             string            `json:"app_id"`
	Amount            *int64            `json:"amount"` // null: the customer chooses
	MinAmount         *int64            `json:"min_amount,omitempty"`
	MaxAmount         *int64            `json:"max_amount,omitempty"`
	Currency          string            `json:"currency"`
	Description       string            `json:"description,omitempty"`
	ReturnURL         string            `json:"return_url,omitempty"`
	SingleUse         bool              `json:"single_use"`
	Active            bool              `json:"active"`
	ExpiresAt         *time.Time        `json:"expires_at"`
	Metadata          map[string]string `json:"metadata"`
	DeactivatedAt     *time.Time        `json:"deactivated_at,omitempty"`
	DeactivatedReason string            `json:"deactivated_reason,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	Version           int64             `json:"version"`
}

// ViewLink renders l; url is its public address.
func ViewLink(l store.PaymentLink, url string) View {
	v := View{ID: l.ID, Object: "payment_link", Livemode: l.Mode == "live", URL: url, Slug: l.Slug, AppID: l.AppID, Currency: l.Currency, Description: l.Description,
		ReturnURL: l.ReturnURL, SingleUse: l.SingleUse, Active: l.Active, Metadata: l.Metadata, DeactivatedReason: l.DeactivatedReason,
		CreatedAt: l.CreatedAt, Version: l.Version}
	if v.Metadata == nil {
		v.Metadata = map[string]string{}
	}
	if l.Amount > 0 {
		v.Amount = &l.Amount
	}
	if l.MinAmount > 0 {
		v.MinAmount = &l.MinAmount
	}
	if l.MaxAmount > 0 {
		v.MaxAmount = &l.MaxAmount
	}
	if !l.ExpiresAt.IsZero() {
		v.ExpiresAt = &l.ExpiresAt
	}
	if !l.DeactivatedAt.IsZero() {
		v.DeactivatedAt = &l.DeactivatedAt
	}
	return v
}

type eventData struct {
	View
	PaymentID string `json:"payment_id,omitempty"`
}

func (s *Service) emit(ctx context.Context, tx store.Tx, typ string, l store.PaymentLink, paymentID string) error {
	return s.emitter.Emit(ctx, tx, events.Event{OrgID: l.OrgID, AppID: l.AppID, Mode: l.Mode, Type: typ,
		ObjectType: "payment_link", ObjectID: l.ID, ObjectVersion: l.Version,
		Data: eventData{View: ViewLink(l, s.URL(l)), PaymentID: paymentID}})
}

// CreateInput is the input of Create. Amount is a fixed price in minor units;
// leave it 0 and set MinAmount (and optionally MaxAmount) to let the customer choose.
type CreateInput struct {
	Scope     core.Scope
	OrgID     string // when set, the app must belong to this org (dashboard)
	Amount    int64
	MinAmount int64
	MaxAmount int64
	Currency  string

	Description, ReturnURL string
	SingleUse              bool
	ExpiresAt              time.Time // zero: never
	Metadata               map[string]string
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http")
}

func (s *Service) validate(in *CreateInput) error {
	if err := in.Scope.Validate(); err != nil {
		return newErr(ErrInvalid, "an app and a mode are required")
	}
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if _, err := money.Exponent(in.Currency); err != nil {
		return newErr(ErrInvalid, "currency %q is not a supported ISO 4217 code", in.Currency)
	}
	switch {
	case in.Amount < 0 || in.MinAmount < 0 || in.MaxAmount < 0 || in.Amount > maxAmount || in.MinAmount > maxAmount || in.MaxAmount > maxAmount:
		return newErr(ErrInvalid, "amounts are non-negative integers in minor units")
	case in.Amount > 0 && (in.MinAmount != 0 || in.MaxAmount != 0):
		return newErr(ErrInvalid, "a link has either a fixed amount or min_amount/max_amount, not both")
	case in.Amount == 0 && in.MinAmount == 0:
		return newErr(ErrInvalid, "set amount (a fixed price) or min_amount (the customer chooses)")
	case in.MaxAmount != 0 && in.MaxAmount < in.MinAmount:
		return newErr(ErrInvalid, "max_amount must not be below min_amount")
	}
	if len(in.Description) > maxText {
		return newErr(ErrInvalid, "description is too long (max %d)", maxText)
	}
	if in.ReturnURL != "" && !validURL(in.ReturnURL) {
		return newErr(ErrInvalid, "return_url must be an absolute http(s) URL")
	}
	now := s.now()
	if !in.ExpiresAt.IsZero() {
		if !in.ExpiresAt.After(now) {
			return newErr(ErrInvalid, "expires_at must be in the future")
		}
		if in.ExpiresAt.After(now.Add(maxLifetime)) {
			return newErr(ErrInvalid, "expires_at is more than five years away")
		}
	}
	if len(in.Metadata) > maxMetadataKeys {
		return newErr(ErrInvalid, "metadata may have at most %d keys", maxMetadataKeys)
	}
	for k, v := range in.Metadata {
		if k == "" || len(k) > maxMetadataKey || len(v) > maxMetadataValue {
			return newErr(ErrInvalid, "metadata keys are 1-%d chars and values at most %d", maxMetadataKey, maxMetadataValue)
		}
		if k == MetadataKey {
			return newErr(ErrInvalid, "metadata key %q is reserved", MetadataKey)
		}
	}
	return nil
}

const slugAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func newSlug() string {
	b := make([]byte, 0, slugLen)
	for len(b) < slugLen {
		var one [1]byte
		if _, err := rand.Read(one[:]); err != nil {
			panic("paylinks: entropy source failed: " + err.Error())
		}
		if one[0] < 36*7 { // 252: rejection sampling keeps the alphabet uniform
			b = append(b, slugAlphabet[int(one[0])%36])
		}
	}
	return string(b)
}

// Create stores a new link, emits payment_link.created and audits it.
func (s *Service) Create(ctx context.Context, actor audit.Actor, in CreateInput) (store.PaymentLink, error) {
	if err := s.validate(&in); err != nil {
		return store.PaymentLink{}, err
	}
	app, err := s.core.GetApp(ctx, in.Scope.AppID)
	if err != nil || in.OrgID != "" && app.OrgID != in.OrgID {
		return store.PaymentLink{}, newErr(ErrNotFound, "app not found")
	}
	now := s.now()
	for attempt := 0; ; attempt++ {
		l := store.PaymentLink{ID: ids.New("plink"), OrgID: app.OrgID, Mode: in.Scope.Mode, AppID: app.ID, Slug: newSlug(), Amount: in.Amount,
			MinAmount: in.MinAmount, MaxAmount: in.MaxAmount, Currency: in.Currency, Description: in.Description, ReturnURL: in.ReturnURL,
			SingleUse: in.SingleUse, ExpiresAt: in.ExpiresAt, Metadata: in.Metadata, Active: true, Version: 1, CreatedAt: now, UpdatedAt: now}
		err := s.db.Write(ctx, func(tx store.Tx) error {
			if err := store.InsertPaymentLink(ctx, tx, l); err != nil {
				return err
			}
			if err := s.emit(ctx, tx, EventCreated, l, ""); err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{OrgID: l.OrgID, Actor: actor, Action: "payment_link.create", Target: l.ID,
				After: map[string]any{"app_id": l.AppID, "mode": l.Mode, "amount": l.Amount, "min_amount": l.MinAmount, "currency": l.Currency,
					"single_use": l.SingleUse}})
		})
		if errors.Is(err, store.ErrConflict) && attempt < 5 { // a taken slug: draw another
			continue
		}
		if err != nil {
			return store.PaymentLink{}, err
		}
		return l, nil
	}
}

func inScope(sc core.Scope) func(store.PaymentLink) bool {
	return func(l store.PaymentLink) bool { return l.AppID == sc.AppID && l.Mode == sc.Mode }
}

func inOrg(org string) func(store.PaymentLink) bool {
	return func(l store.PaymentLink) bool { return l.OrgID == org }
}

func (s *Service) get(ctx context.Context, id string, ok func(store.PaymentLink) bool) (store.PaymentLink, error) {
	var l store.PaymentLink
	err := s.db.Read(ctx, func(q store.Querier) (err error) { l, err = store.GetPaymentLink(ctx, q, id); return })
	if errors.Is(err, store.ErrNotFound) || err == nil && !ok(l) {
		return store.PaymentLink{}, newErr(ErrNotFound, "payment link not found")
	}
	return l, err
}

// Get returns a link of the scope's app and mode.
func (s *Service) Get(ctx context.Context, sc core.Scope, id string) (store.PaymentLink, error) {
	if err := sc.Validate(); err != nil {
		return store.PaymentLink{}, newErr(ErrInvalid, "an app and a mode are required")
	}
	return s.get(ctx, id, inScope(sc))
}

// GetInOrg returns a link of the org (dashboard).
func (s *Service) GetInOrg(ctx context.Context, orgID, id string) (store.PaymentLink, error) {
	return s.get(ctx, id, inOrg(orgID))
}

// ListParams filters List.
type ListParams struct {
	Active        *bool
	StartingAfter string
	Limit         int
}

// List returns the scope's links, newest first.
func (s *Service) List(ctx context.Context, sc core.Scope, p ListParams) ([]store.PaymentLink, error) {
	if err := sc.Validate(); err != nil {
		return nil, newErr(ErrInvalid, "an app and a mode are required")
	}
	var org string
	if err := s.db.Read(ctx, func(q store.Querier) error {
		app, err := store.GetApp(ctx, q, sc.AppID)
		org = app.OrgID
		return err
	}); err != nil {
		return nil, newErr(ErrNotFound, "app not found")
	}
	return s.list(ctx, store.PaymentLinkFilter{OrgID: org, AppID: sc.AppID, Mode: sc.Mode, Active: p.Active, StartingAfter: p.StartingAfter, Limit: p.Limit})
}

// OrgListParams filters ListOrg.
type OrgListParams struct {
	AppID, Mode, StartingAfter string
	Active                     *bool
	Limit                      int
}

// ListOrg returns the org's links, newest first (dashboard).
func (s *Service) ListOrg(ctx context.Context, orgID string, p OrgListParams) ([]store.PaymentLink, error) {
	return s.list(ctx, store.PaymentLinkFilter{OrgID: orgID, AppID: p.AppID, Mode: p.Mode, Active: p.Active, StartingAfter: p.StartingAfter, Limit: p.Limit})
}

func (s *Service) list(ctx context.Context, f store.PaymentLinkFilter) ([]store.PaymentLink, error) {
	var out []store.PaymentLink
	err := s.db.Read(ctx, func(q store.Querier) (err error) { out, err = store.ListPaymentLinks(ctx, q, f); return })
	return out, err
}

// Deactivate switches a link of the scope off. It is idempotent: a link that
// is already off is returned as it is and nothing is emitted again.
func (s *Service) Deactivate(ctx context.Context, actor audit.Actor, sc core.Scope, id string) (store.PaymentLink, error) {
	if err := sc.Validate(); err != nil {
		return store.PaymentLink{}, newErr(ErrInvalid, "an app and a mode are required")
	}
	return s.deactivate(ctx, actor, id, inScope(sc), ReasonManual, "")
}

// DeactivateInOrg switches a link of the org off (dashboard).
func (s *Service) DeactivateInOrg(ctx context.Context, actor audit.Actor, orgID, id string) (store.PaymentLink, error) {
	return s.deactivate(ctx, actor, id, inOrg(orgID), ReasonManual, "")
}

func (s *Service) deactivate(ctx context.Context, actor audit.Actor, id string, ok func(store.PaymentLink) bool, reason, paymentID string) (store.PaymentLink, error) {
	var out store.PaymentLink
	err := s.db.Write(ctx, func(tx store.Tx) error {
		l, err := store.GetPaymentLink(ctx, tx, id)
		if errors.Is(err, store.ErrNotFound) || err == nil && !ok(l) {
			return newErr(ErrNotFound, "payment link not found")
		}
		if err != nil {
			return err
		}
		if !l.Active {
			out = l
			return nil
		}
		if out, err = store.DeactivatePaymentLink(ctx, tx, id, reason, s.now()); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, EventDeactivated, out, paymentID); err != nil {
			return err
		}
		after := map[string]string{"reason": reason}
		if paymentID != "" {
			after["payment_id"] = paymentID
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: out.OrgID, Actor: actor, Action: "payment_link.deactivate", Target: out.ID, After: after})
	})
	return out, err
}

// OnPaymentSucceeded is the core.PaymentHook that spends single-use links: it
// runs in the transaction in which a payment became succeeded.
func (s *Service) OnPaymentSucceeded(ctx context.Context, tx store.Tx, p store.Payment, a store.Attempt) error {
	id := p.Metadata[MetadataKey]
	if id == "" {
		return nil
	}
	l, err := store.GetPaymentLink(ctx, tx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil // metadata is the merchant's to write; a stray id changes nothing
	}
	if err != nil {
		return err
	}
	// Only a link of the same org, app and mode can be spent by this payment.
	if !l.SingleUse || l.OrgID != p.OrgID || l.AppID != p.AppID || l.Mode != p.Mode {
		return nil
	}
	if l.Active {
		out, err := store.SpendPaymentLink(ctx, tx, l.ID, p.ID, s.now())
		if err == nil {
			if err := s.emit(ctx, tx, EventDeactivated, out, p.ID); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Entry{OrgID: out.OrgID, Actor: audit.System, Action: "payment_link.deactivate", Target: out.ID,
				After: map[string]string{"reason": ReasonUsed, "payment_id": p.ID}}); err != nil {
				return err
			}
			// The link is sold: its other open payments must not be paid as well.
			_, err = s.core.CancelOpenOrderPayments(ctx, tx, l.AppID, l.Mode, l.ID, p.ID)
			return err
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		// Spent by another payment a moment ago: this one is the second sale.
		if l, err = store.GetPaymentLink(ctx, tx, l.ID); err != nil {
			return err
		}
	}
	// Spent already, by another payment: the item was sold once and paid twice.
	// (A link switched off by hand while this payment was open is not that: the
	// customer was entitled to finish.)
	if l.DeactivatedReason == ReasonUsed && l.UsedByPaymentID != "" && l.UsedByPaymentID != p.ID {
		return s.core.MarkExtraSale(ctx, tx, p, a, "payment_link_already_used")
	}
	return nil
}

// checkState reports why a link cannot take payments, or nil.
func (s *Service) checkState(l store.PaymentLink) error {
	switch {
	case !l.Active:
		return newErr(ErrInactive, "this payment link is no longer active")
	case !l.ExpiresAt.IsZero() && !s.now().Before(l.ExpiresAt):
		return newErr(ErrExpired, "this payment link has expired")
	}
	return nil
}

// Open returns the link behind a public slug if it can take payments right
// now; ErrInactive, ErrExpired and ErrNotFound say why not.
func (s *Service) Open(ctx context.Context, slug string) (store.PaymentLink, error) {
	l, err := s.bySlug(ctx, slug)
	if err != nil {
		return store.PaymentLink{}, err
	}
	return l, s.checkState(l)
}

func (s *Service) bySlug(ctx context.Context, slug string) (store.PaymentLink, error) {
	var l store.PaymentLink
	err := s.db.Read(ctx, func(q store.Querier) (err error) { l, err = store.GetPaymentLinkBySlug(ctx, q, slug); return })
	if errors.Is(err, store.ErrNotFound) {
		return store.PaymentLink{}, newErr(ErrNotFound, "payment link not found")
	}
	return l, err
}

// StartInput is what a customer submits on the link's page.
type StartInput struct {
	Name, Email, Phone string
	// Amount is the amount the customer chose, in minor units; ignored for a
	// fixed-price link.
	Amount int64
	// Holder identifies the visitor of a single-use link (a random value kept
	// in a cookie); an empty Holder is a visitor nobody can recognise again.
	Holder string
}

// Started is the payment created from a link; ClientSecret is shown once.
type Started struct {
	PaymentID, ClientSecret string
}

var phoneRE = regexp.MustCompile(`^[0-9+()\- ]{6,20}$`)

func validateWho(in *StartInput) error {
	in.Name, in.Email, in.Phone = strings.TrimSpace(in.Name), strings.ToLower(strings.TrimSpace(in.Email)), strings.TrimSpace(in.Phone)
	switch {
	case in.Name == "" || len(in.Name) > 200 || strings.ContainsFunc(in.Name, unicode.IsControl):
		return newKeyed(ErrInvalid, "link.err.name", nil, "please enter your name")
	case in.Email == "":
		return newKeyed(ErrInvalid, "link.err.email", nil, "please enter a valid email address")
	}
	if a, err := mail.ParseAddress(in.Email); err != nil || a.Address != in.Email || len(in.Email) > 254 {
		return newKeyed(ErrInvalid, "link.err.email", nil, "please enter a valid email address")
	}
	if in.Phone != "" && !phoneRE.MatchString(in.Phone) {
		return newKeyed(ErrInvalid, "link.err.phone", nil, "please enter a valid phone number")
	}
	return nil
}

func (s *Service) allowStart(slug string) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.windows) > 10000 { // forget idle links
		for k, w := range s.windows {
			if now.Sub(w.start) > time.Minute {
				delete(s.windows, k)
			}
		}
	}
	w := s.windows[slug]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &window{start: now}
		s.windows[slug] = w
	}
	if w.n >= s.perMin {
		return false
	}
	w.n++
	return true
}

func (s *Service) holdFor(ctx context.Context, appID string) time.Duration {
	minutes := int64(60)
	if s.settings != nil {
		if v, err := s.settings.Resolve(ctx, appID, "payment_ttl"); err == nil {
			_ = v.Decode(&minutes)
		}
	}
	return time.Duration(minutes) * time.Minute
}

// Start creates the payment for a customer who opened the link. For a
// single-use link the visitor holds the link until their payment's lifetime is
// over; the first successful payment then spends it (OnPaymentSucceeded).
func (s *Service) Start(ctx context.Context, slug string, in StartInput) (Started, error) {
	if !s.allowStart(slug) {
		return Started{}, newErr(ErrRateLimited, "too many payments are being started with this link; try again in a minute")
	}
	l, err := s.bySlug(ctx, slug)
	if err != nil {
		return Started{}, err
	}
	if err := s.checkState(l); err != nil {
		return Started{}, err
	}
	if err := validateWho(&in); err != nil {
		return Started{}, err
	}
	amount := l.Amount
	if amount == 0 {
		amount = in.Amount
		if amount < l.MinAmount || amount <= 0 || l.MaxAmount > 0 && amount > l.MaxAmount || amount > maxAmount {
			return Started{}, s.amountRangeErr(l)
		}
	}

	holder := in.Holder
	if l.SingleUse {
		if holder == "" {
			holder = randomHolder()
		}
		var ok bool
		var claimed string
		now := s.now()
		err := s.db.Write(ctx, func(tx store.Tx) (err error) {
			ok, err = store.ClaimPaymentLink(ctx, tx, l.ID, holder, now.Add(s.holdFor(ctx, l.AppID)), now)
			if ok && err == nil {
				var cur store.PaymentLink
				if cur, err = store.GetPaymentLink(ctx, tx, l.ID); err == nil {
					claimed = cur.ClaimPaymentID
				}
			}
			return
		})
		if err != nil {
			return Started{}, err
		}
		if ok && claimed != "" {
			// The visitor already has a payment on this link (a reload, the back button,
			// a double click): hand it back instead of making a second one that could
			// be paid as well. Only if it can still be paid and asks for this amount.
			if st, reused := s.reuseOpenPayment(ctx, l, claimed, amount); reused {
				return st, nil
			}
		}
		if !ok {
			// Either someone else holds it, or it was just spent or switched off.
			if cur, err := s.bySlug(ctx, slug); err == nil {
				if stateErr := s.checkState(cur); stateErr != nil {
					return Started{}, stateErr
				}
			}
			return Started{}, newErr(ErrBusy, "this single-use link is being used right now; try again in a little while")
		}
	}

	meta := make(map[string]string, len(l.Metadata)+1)
	for k, v := range l.Metadata {
		meta[k] = v
	}
	meta[MetadataKey] = l.ID
	res, err := s.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: core.Scope{AppID: l.AppID, Mode: l.Mode}, Amount: amount, Currency: l.Currency,
		OrderRef: l.ID, Description: l.Description, Metadata: meta, ReturnURL: l.ReturnURL,
		Customer: &core.CustomerInput{Name: in.Name, Email: in.Email, Phone: in.Phone}})
	if err != nil {
		if l.SingleUse {
			_ = s.db.Write(ctx, func(tx store.Tx) error { return store.ReleasePaymentLink(ctx, tx, l.ID, holder, s.now()) })
		}
		switch {
		case errors.Is(err, core.ErrNoRoute), errors.Is(err, core.ErrCurrencyNotAccepted):
			return Started{}, newErr(ErrUnavailable, "no payment method is available for this link")
		case errors.Is(err, core.ErrInvalid):
			return Started{}, newErr(ErrInvalid, "%v", err)
		}
		return Started{}, err
	}
	if l.SingleUse {
		_ = s.db.Write(ctx, func(tx store.Tx) error {
			return store.SetPaymentLinkClaimPayment(ctx, tx, l.ID, holder, res.Payment.ID, s.now())
		})
	}
	return Started{PaymentID: res.Payment.ID, ClientSecret: res.ClientSecret}, nil
}

// reuseOpenPayment returns the holder's earlier payment when it is still open
// and for the same amount, with a fresh client secret (only the hash is kept). An
// open payment for another amount is canceled first, so only one stays payable.
func (s *Service) reuseOpenPayment(ctx context.Context, l store.PaymentLink, paymentID string, amount int64) (Started, bool) {
	p, err := s.core.GetPaymentByID(ctx, paymentID)
	if err != nil {
		return Started{}, false
	}
	switch p.Status {
	case "created", "requires_action", "processing":
	default:
		return Started{}, false
	}
	if !p.ExpiresAt.After(s.now()) {
		return Started{}, false
	}
	if p.Amount != amount {
		_, _ = s.core.CancelPayment(ctx, core.Scope{AppID: l.AppID, Mode: l.Mode}, audit.System, p.ID)
		return Started{}, false
	}
	secret, err := s.core.ReissueClientSecret(ctx, p.ID)
	if err != nil {
		return Started{}, false
	}
	return Started{PaymentID: p.ID, ClientSecret: secret}, true
}

func randomHolder() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("paylinks: entropy source failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func (s *Service) format(minor int64, currency string) string {
	d, err := money.FormatDecimal(minor, currency)
	if err != nil {
		return fmt.Sprint(minor)
	}
	return currency + " " + d
}

// amountRangeErr is the error for an amount outside what the link allows.
func (s *Service) amountRangeErr(l store.PaymentLink) error {
	lo := s.format(l.MinAmount, l.Currency)
	if l.MaxAmount == 0 {
		return newKeyed(ErrInvalid, "link.err.amount_min", []any{lo}, "please enter an amount of at least "+lo)
	}
	hi := s.format(l.MaxAmount, l.Currency)
	return newKeyed(ErrInvalid, "link.err.amount_range", []any{lo, hi}, "please enter an amount between "+lo+" and "+hi)
}

// FormatAmount renders minor units with the currency code, for pages.
func (s *Service) FormatAmount(minor int64, currency string) string { return s.format(minor, currency) }
