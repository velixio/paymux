package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector/money"
)

const (
	maxAmount        = int64(1) << 50 // far above any real payment, far below overflow
	maxMetadataKeys  = 20
	maxMetadataKey   = 40
	maxMetadataValue = 500
	maxText          = 1000
	defaultPageSize  = 20
	maxPageSize      = 100
)

// CustomerInput describes the payer; it is upserted by ExternalRef.
type CustomerInput struct {
	ExternalRef, Name, Email, Phone string
	Billing                         store.Billing
}

// CreatePaymentInput is the input of CreatePayment. Amount is in minor units.
type CreatePaymentInput struct {
	Scope       Scope
	Kind        string // default one_time
	Amount      int64
	Currency    string
	OrderRef    string
	Description string
	Customer    *CustomerInput
	Metadata    map[string]string
	ReturnURL   string // default: the app's default_return_url
	// Country and AllowedConnectors feed routing (section 8).
	Country           string
	AllowedConnectors []string
}

// CreatedPayment is the result of CreatePayment. ClientSecret is shown once;
// only its hash is stored.
type CreatedPayment struct {
	Payment      store.Payment
	ClientSecret string
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func newClientSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("core: entropy source failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// ClientSecretMatches reports whether secret is the payment's client secret,
// ignoring expiry (so a page can tell "expired" from "wrong link").
func ClientSecretMatches(p store.Payment, secret string) bool {
	return subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(p.ClientSecretHash)) == 1
}

// VerifyClientSecret reports whether secret is the payment's client secret
// and the payment has not passed expires_at.
func VerifyClientSecret(p store.Payment, secret string, now time.Time) bool {
	return ClientSecretMatches(p, secret) && !now.After(p.ExpiresAt)
}

// GetApp returns an app by id (for branding on the pay pages).
func (s *Service) GetApp(ctx context.Context, id string) (store.App, error) {
	var a store.App
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		a, err = store.GetApp(ctx, q, id)
		return
	})
	return a, mapStoreErr(err, "app")
}

func validateCreate(in *CreatePaymentInput) error {
	if err := in.Scope.validate(); err != nil {
		return err
	}
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if _, err := money.Exponent(in.Currency); err != nil {
		return newErr(ErrInvalid, "currency %q is not a supported ISO 4217 code", in.Currency)
	}
	if in.Amount <= 0 || in.Amount > maxAmount {
		return newErr(ErrInvalid, "amount must be a positive integer in minor units")
	}
	if in.Kind == "" {
		in.Kind = "one_time"
	}
	if !slices.Contains([]string{"one_time", "renewal", "mandate_setup"}, in.Kind) {
		return newErr(ErrInvalid, "unknown kind %q", in.Kind)
	}
	if len(in.OrderRef) > 255 || len(in.Description) > maxText {
		return newErr(ErrInvalid, "order_ref (max 255) or description (max 1000) too long")
	}
	if len(in.Metadata) > maxMetadataKeys {
		return newErr(ErrInvalid, "metadata may have at most %d keys", maxMetadataKeys)
	}
	for k, v := range in.Metadata {
		if k == "" || len(k) > maxMetadataKey || len(v) > maxMetadataValue {
			return newErr(ErrInvalid, "metadata keys are 1-%d chars and values at most %d", maxMetadataKey, maxMetadataValue)
		}
	}
	if in.ReturnURL != "" {
		if !validURL(in.ReturnURL) {
			return newErr(ErrInvalid, "return_url must be an absolute http(s) URL")
		}
	}
	if c := in.Customer; c != nil {
		if len(c.Name) > 200 || len(c.Phone) > 32 || len(c.ExternalRef) > 255 {
			return newErr(ErrInvalid, "customer name, phone or external_ref too long")
		}
		if c.Email != "" && (len(c.Email) > 254 || !strings.Contains(c.Email, "@") || strings.ContainsAny(c.Email, " \t\r\n")) {
			return newErr(ErrInvalid, "customer email is not valid")
		}
	}
	return nil
}

func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http")
}

// CreatePayment validates the input, sets expires_at from the payment_ttl
// setting, stores the payment (status created) with the hash of a fresh
// client secret, and emits payment.created.
func (s *Service) CreatePayment(ctx context.Context, in CreatePaymentInput) (CreatedPayment, error) {
	if err := validateCreate(&in); err != nil {
		return CreatedPayment{}, err
	}
	secret := newClientSecret()
	var p store.Payment
	err := s.db.Write(ctx, func(tx store.Tx) error {
		app, err := store.GetApp(ctx, tx, in.Scope.AppID)
		if err != nil {
			return mapStoreErr(err, "app")
		}
		if s.router != nil {
			err := s.router.CheckRoute(ctx, tx, RouteRequest{Scope: in.Scope, Currency: in.Currency, Country: in.Country,
				Amount: in.Amount, Purpose: "one_time", Metadata: in.Metadata, AllowedConnectors: in.AllowedConnectors})
			if err != nil {
				return err
			}
		}
		ttl, err := s.policy(ctx, tx, app.ID, "payment_ttl")
		if err != nil {
			return err
		}
		var minutes int64
		if err := ttl.Decode(&minutes); err != nil {
			return err
		}
		now := s.nowUTC()
		p = store.Payment{
			ID: ids.New("pay"), OrgID: app.OrgID, AppID: app.ID, Mode: in.Scope.Mode, Kind: in.Kind, Amount: in.Amount,
			Currency: in.Currency, Status: string(state.PaymentCreated), OrderRef: in.OrderRef, Description: in.Description,
			Metadata: in.Metadata, ReturnURL: in.ReturnURL, ClientSecretHash: hashSecret(secret),
			ExpiresAt: now.Add(time.Duration(minutes) * time.Minute), Version: 1, CreatedAt: now, UpdatedAt: now,
		}
		if p.ReturnURL == "" {
			p.ReturnURL = app.DefaultReturnURL
		}
		if c := in.Customer; c != nil {
			p.CustomerID, err = store.UpsertCustomer(ctx, tx, store.Customer{ID: ids.New("cus"), OrgID: app.OrgID,
				AppID: app.ID, Mode: in.Scope.Mode, ExternalRef: c.ExternalRef, Name: c.Name, Email: c.Email,
				Phone: c.Phone, Billing: c.Billing}, now)
			if err != nil {
				return err
			}
		}
		if err := store.InsertPayment(ctx, tx, p); err != nil {
			return err
		}
		return s.emitPayment(ctx, tx, "payment.created", p, "", "")
	})
	if err != nil {
		return CreatedPayment{}, err
	}
	return CreatedPayment{Payment: p, ClientSecret: secret}, nil
}

// emitPayment emits a payment.* event for p at its current version.
func (s *Service) emitPayment(ctx context.Context, tx store.Tx, typ string, p store.Payment, reason, attemptID string) error {
	return s.emitter.Emit(ctx, tx, events.Event{OrgID: p.OrgID, AppID: p.AppID, Mode: p.Mode, Type: typ,
		ObjectType: "payment", ObjectID: p.ID, ObjectVersion: p.Version,
		Data: paymentEventData{PaymentView: ViewPayment(p), Reason: reason, AttemptID: attemptID}})
}

// GetPayment returns a payment of the scope's app and mode.
func (s *Service) GetPayment(ctx context.Context, sc Scope, id string) (store.Payment, error) {
	if err := sc.validate(); err != nil {
		return store.Payment{}, err
	}
	var p store.Payment
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		p, err = store.GetPayment(ctx, q, id)
		return
	})
	if err != nil {
		return store.Payment{}, mapStoreErr(err, "payment")
	}
	if p.AppID != sc.AppID || p.Mode != sc.Mode {
		return store.Payment{}, newErr(ErrNotFound, "payment not found")
	}
	return p, nil
}

// GetAttempt returns an attempt by id.
func (s *Service) GetAttempt(ctx context.Context, id string) (store.Attempt, error) {
	var a store.Attempt
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		a, err = store.GetAttempt(ctx, q, id)
		return
	})
	return a, mapStoreErr(err, "attempt")
}

// ListParams selects a page of payments. After is the exclusive cursor (the
// NextCursor of the previous page).
type ListParams struct {
	Status string
	After  string
	Limit  int
}

// Page is one page of payments, newest first.
type Page struct {
	Payments   []store.Payment
	HasMore    bool
	NextCursor string
}

// ListPayments lists the scope's payments, newest first, with cursor paging.
func (s *Service) ListPayments(ctx context.Context, sc Scope, lp ListParams) (Page, error) {
	if err := sc.validate(); err != nil {
		return Page{}, err
	}
	if lp.Status != "" && !slices.Contains(state.AllPaymentStatuses, state.PaymentStatus(lp.Status)) {
		return Page{}, newErr(ErrInvalid, "unknown status %q", lp.Status)
	}
	if lp.Limit <= 0 {
		lp.Limit = defaultPageSize
	}
	lp.Limit = min(lp.Limit, maxPageSize)
	var rows []store.Payment
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		rows, err = store.ListPayments(ctx, q, sc.AppID, sc.Mode, lp.Status, lp.After, lp.Limit+1)
		return
	})
	if err != nil {
		return Page{}, err
	}
	page := Page{Payments: rows}
	if len(rows) > lp.Limit {
		page.Payments, page.HasMore = rows[:lp.Limit], true
		page.NextCursor = rows[lp.Limit-1].ID
	}
	return page, nil
}

// CancelPayment cancels a payment that has not been paid. Cancelling an
// already-canceled payment is a no-op; cancelling any other state is
// ErrInvalidState.
func (s *Service) CancelPayment(ctx context.Context, sc Scope, actor audit.Actor, id string) (store.Payment, error) {
	if err := sc.validate(); err != nil {
		return store.Payment{}, err
	}
	var p store.Payment
	err := s.db.Write(ctx, func(tx store.Tx) error {
		var err error
		if p, err = store.GetPaymentForUpdate(ctx, tx, id); err != nil {
			return mapStoreErr(err, "payment")
		}
		if p.AppID != sc.AppID || p.Mode != sc.Mode {
			return newErr(ErrNotFound, "payment not found")
		}
		out := state.PaymentNext(state.PaymentStatus(p.Status), state.EvCancel)
		switch out.Action {
		case state.Ignore:
			return nil
		case state.Apply:
		default:
			return newErr(ErrInvalidState, "a payment that is %s cannot be canceled", p.Status)
		}
		before := ViewPayment(p)
		p.Status = string(out.Next)
		if err := store.UpdatePayment(ctx, tx, &p, s.nowUTC()); err != nil {
			return err
		}
		if err := s.emitPayment(ctx, tx, "payment.canceled", p, "", ""); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: p.OrgID, Actor: actor, Action: "payment.cancel",
			Target: p.ID, Before: before, After: ViewPayment(p)})
	})
	return p, err
}

// BillingInput is what the pay page collects about the payer.
type BillingInput struct {
	Name, Email, Phone string
	Billing            store.Billing
}

func merge(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// SetBilling stores billing details on the payment's customer, creating and
// linking the customer when the payment has none. Empty fields keep their
// stored value.
func (s *Service) SetBilling(ctx context.Context, paymentID string, in BillingInput) error {
	return s.db.Write(ctx, func(tx store.Tx) error {
		p, err := store.GetPaymentForUpdate(ctx, tx, paymentID)
		if err != nil {
			return mapStoreErr(err, "payment")
		}
		now := s.nowUTC()
		c := store.Customer{ID: ids.New("cus"), OrgID: p.OrgID, AppID: p.AppID, Mode: p.Mode}
		if p.CustomerID != "" {
			if c, err = store.GetCustomer(ctx, tx, p.CustomerID); err != nil {
				return mapStoreErr(err, "customer")
			}
		}
		merge(&c.Name, in.Name)
		merge(&c.Email, in.Email)
		merge(&c.Phone, in.Phone)
		b := &c.Billing
		merge(&b.LegalName, in.Billing.LegalName)
		merge(&b.GSTIN, in.Billing.GSTIN)
		merge(&b.Line1, in.Billing.Line1)
		merge(&b.Line2, in.Billing.Line2)
		merge(&b.City, in.Billing.City)
		merge(&b.StateCode, in.Billing.StateCode)
		merge(&b.PostalCode, in.Billing.PostalCode)
		merge(&b.Country, in.Billing.Country)
		if p.CustomerID != "" {
			return store.UpdateCustomer(ctx, tx, c, now)
		}
		if _, err := store.UpsertCustomer(ctx, tx, c, now); err != nil {
			return err
		}
		return store.SetPaymentCustomer(ctx, tx, p.ID, c.ID, now)
	})
}

// GetCustomer returns a customer by id.
func (s *Service) GetCustomer(ctx context.Context, id string) (store.Customer, error) {
	var c store.Customer
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		c, err = store.GetCustomer(ctx, q, id)
		return
	})
	return c, mapStoreErr(err, "customer")
}

// GetPaymentByID returns a payment without tenant scoping. It is for the
// pay and return pages, which authenticate with the client secret or a
// provider reference instead of an API key.
func (s *Service) GetPaymentByID(ctx context.Context, id string) (store.Payment, error) {
	var p store.Payment
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		p, err = store.GetPayment(ctx, q, id)
		return
	})
	return p, mapStoreErr(err, "payment")
}

// RecordCheckout stores the provider order id returned by CreateCheckout.
func (s *Service) RecordCheckout(ctx context.Context, attemptID, providerOrderID string) error {
	return s.db.Write(ctx, func(tx store.Tx) error {
		a, err := store.GetAttempt(ctx, tx, attemptID)
		if err != nil {
			return mapStoreErr(err, "attempt")
		}
		a.ProviderOrderID = providerOrderID
		return store.UpdateAttempt(ctx, tx, a, s.nowUTC())
	})
}
