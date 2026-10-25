package subscriptions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/billing"
	"github.com/paymux/paymux/internal/checkout"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/routing"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

// CreateInput is the input of Create. Amount is in minor units per period.
type CreateInput struct {
	Scope         core.Scope
	Customer      *core.CustomerInput
	Amount        int64
	Currency      string
	Interval      string // day | week | month | year
	IntervalCount int    // default 1
	TrialDays     int
	// StartAt delays the first period; zero means now.
	StartAt     time.Time
	TotalCount  int // 0 = until cancelled
	Description string
	Metadata    map[string]string
	ReturnURL   string // default: the app's return URL
	// Method is an optional mandate method hint (upi, card, emandate) for
	// merchant-triggered billing; the default prefers UPI.
	Method string
	// AllowedConnectors restricts routing to these connectors for the life of the
	// subscription; Country is the buyer's country, which routing rules can match
	// on. Both are kept on the subscription, like a payment's.
	AllowedConnectors []string
	Country           string
}

// Created is the result of Create and the other calls that start a checkout.
type Created struct {
	Subscription store.Subscription
	// CheckoutURL is /s/{id}?t=... on the base URL; shown only here.
	CheckoutURL string
}

const (
	maxIntervalCount = 366
	maxAmount        = int64(1) << 50
)

func validateCreate(in *CreateInput) error {
	if err := in.Scope.Validate(); err != nil {
		return newErr(ErrInvalid, "%v", err)
	}
	if in.Amount <= 0 || in.Amount > maxAmount {
		return newErr(ErrInvalid, "amount must be a positive integer in minor units")
	}
	if _, err := money.Exponent(in.Currency); err != nil {
		return newErr(ErrInvalid, "unknown currency %q", in.Currency)
	}
	if !slices.Contains([]string{"day", "week", "month", "year"}, in.Interval) {
		return newErr(ErrInvalid, "interval must be day, week, month or year")
	}
	if in.IntervalCount == 0 {
		in.IntervalCount = 1
	}
	if in.IntervalCount < 1 || in.IntervalCount > maxIntervalCount {
		return newErr(ErrInvalid, "interval_count must be between 1 and %d", maxIntervalCount)
	}
	if in.TrialDays < 0 || in.TrialDays > 365 {
		return newErr(ErrInvalid, "trial_days must be between 0 and 365")
	}
	if in.TotalCount < 0 || in.TotalCount > 10000 {
		return newErr(ErrInvalid, "total_count must be between 0 and 10000")
	}
	if len(in.Description) > 1000 {
		return newErr(ErrInvalid, "description too long (max 1000)")
	}
	in.Country = strings.ToUpper(strings.TrimSpace(in.Country))
	if in.Country != "" && (len(in.Country) != 2 || in.Country[0] < 'A' || in.Country[0] > 'Z' || in.Country[1] < 'A' || in.Country[1] > 'Z') {
		return newErr(ErrInvalid, "country must be an ISO 3166 alpha-2 code")
	}
	if len(in.AllowedConnectors) > 20 {
		return newErr(ErrInvalid, "allowed_connectors has at most 20 entries")
	}
	for i, c := range in.AllowedConnectors {
		in.AllowedConnectors[i] = strings.TrimSpace(c)
		if in.AllowedConnectors[i] == "" || len(in.AllowedConnectors[i]) > 64 {
			return newErr(ErrInvalid, "allowed_connectors entries are connector names (1-64 characters)")
		}
	}
	if len(in.Metadata) > 20 {
		return newErr(ErrInvalid, "metadata has at most 20 keys")
	}
	if in.Method != "" && !slices.Contains([]string{"upi", "card", "emandate"}, in.Method) {
		return newErr(ErrInvalid, "method must be upi, card or emandate")
	}
	if in.ReturnURL != "" {
		if u, err := url.Parse(in.ReturnURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return newErr(ErrInvalid, "return_url must be an absolute http(s) URL")
		}
	}
	c := in.Customer
	if c == nil || (c.Email == "" && c.Phone == "") {
		return newErr(ErrInvalid, "customer with an email or phone is required")
	}
	return nil
}

func newToken() (token, hash string) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("subscriptions: entropy source failed: " + err.Error())
	}
	token = base64.RawURLEncoding.EncodeToString(b[:])
	return token, hashToken(token)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func (s *Service) checkoutURL(id, token string) string {
	return s.baseURL + "/s/" + id + "?t=" + url.QueryEscape(token)
}

// route is the chosen way of billing a new subscription.
type route struct {
	accountID, mode, method string
	maxAmount               int64 // mandate max (merchant_triggered)
}

// mandateMethods lists the mandate methods a connector supports, UPI first.
func mandateMethods(info connector.Info) []string {
	var out []string
	for _, m := range []string{"upi", "card", "emandate"} {
		for _, l := range info.MandateLimits {
			if l.Method == m {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

func connectorLimit(info connector.Info, method string) int64 {
	for _, l := range info.MandateLimits {
		if l.Method == method {
			return l.MaxAmount
		}
	}
	return 0
}

// chooseRoute picks the provider account and billing mode for a new
// subscription (spec 6.8). Up to the AFA limit the first routed account that
// can bill the amount wins. Above it only an e-NACH mandate (up to Rs 1 crore)
// or a provider_managed account can; e-NACH is tried first. If neither exists
// the amount is refused with ErrAmountExceedsMandateLimit.
func (s *Service) chooseRoute(ctx context.Context, q store.Queryer, appID, mode string, in CreateInput, pol policy) (route, error) {
	app, err := store.GetApp(ctx, q, appID)
	if err != nil {
		return route{}, mapNotFound(err, "app")
	}
	cfg, err := checkout.AppRoutingConfig(ctx, q, app)
	if err != nil {
		return route{}, err
	}
	stored, err := store.ListAppProviderAccounts(ctx, q, appID, mode)
	if err != nil {
		return route{}, err
	}
	conns := map[string]connector.Connector{}
	var usable []store.ProviderAccount
	for _, a := range stored {
		c, err := s.connectors.Get(ctx, a.ID)
		if err != nil {
			continue // an account whose connector cannot be built is not routable
		}
		conns[a.ID] = c
		if len(a.AcceptedCurrencies) == 0 {
			a.AcceptedCurrencies = c.Info().Currencies // default: whatever the connector declares
		}
		usable = append(usable, a)
	}
	accounts := checkout.RoutingAccounts(usable)
	for i := range accounts {
		// The live connector knows best (a remote connector's currencies come from
		// discovery): a subscription is never routed to an account whose connector
		// cannot bill it in the currency, so the failure is here and not at the
		// customer's checkout.
		accounts[i].SubscriptionCurrencies = conns[accounts[i].ID].Info().SubscriptionCurrencies
	}
	res, err := routing.Choose(cfg, routing.Request{Currency: in.Currency, Country: in.Country, Amount: in.Amount, Purpose: routing.PurposeSubscription,
		Metadata: in.Metadata, AllowedConnectors: in.AllowedConnectors}, accounts, nil)
	var rej *routing.RejectedError
	switch {
	case errors.As(err, &rej):
		return route{}, newErr(ErrNoRoute, "%s", rej.Message)
	case errors.Is(err, routing.ErrNoRoute):
		return route{}, newErr(ErrNoRoute, "no provider account can bill %s subscriptions", in.Currency)
	case err != nil:
		return route{}, err
	}

	limited := false
	merchant := func(emandateOnly bool) (route, bool) {
		for _, id := range res.Accounts {
			c := conns[id]
			info := c.Info()
			if _, ok := c.(connector.MandateManager); !ok || !slices.Contains(info.SubscriptionModes, connector.MerchantTriggered) || in.Currency != "INR" {
				continue
			}
			methods := mandateMethods(info)
			if in.Method != "" {
				methods = slices.DeleteFunc(slices.Clone(methods), func(m string) bool { return m != in.Method })
			}
			for _, m := range methods {
				if emandateOnly && m != string(billing.MethodEMandate) {
					continue
				}
				lim := pol.methodLimit(m)
				if cl := connectorLimit(info, m); cl > 0 && cl < lim {
					lim = cl
				}
				if in.Amount > lim {
					limited = true
					continue
				}
				mx, err := billing.MandateMax(in.Amount, pol.headroomBP, lim)
				if err != nil {
					limited = true
					continue
				}
				return route{accountID: id, mode: string(connector.MerchantTriggered), method: m, maxAmount: mx}, true
			}
		}
		return route{}, false
	}
	managed := func() (route, bool) {
		for _, id := range res.Accounts {
			c := conns[id]
			if _, ok := c.(connector.SubscriptionManager); ok && slices.Contains(c.Info().SubscriptionModes, connector.ProviderManaged) {
				return route{accountID: id, mode: string(connector.ProviderManaged)}, true
			}
		}
		return route{}, false
	}

	if in.Amount > pol.afaLimit {
		if r, ok := merchant(true); ok {
			return r, nil
		}
		if r, ok := managed(); ok {
			return r, nil
		}
	} else {
		// In routing order: the first account that can serve it.
		for _, id := range res.Accounts {
			only := func(want string) bool {
				return slices.Contains(conns[id].Info().SubscriptionModes, connector.BillingMode(want))
			}
			switch {
			case only(string(connector.ProviderManaged)) && isManager(conns[id]):
				return route{accountID: id, mode: string(connector.ProviderManaged)}, nil
			case only(string(connector.MerchantTriggered)):
				saved := res.Accounts
				res.Accounts = []string{id}
				r, ok := merchant(false)
				res.Accounts = saved
				if ok {
					return r, nil
				}
			}
		}
	}
	if limited || in.Amount > pol.afaLimit {
		return route{}, newErr(ErrAmountExceedsMandateLimit,
			"amount %d exceeds what a mandate allows and no provider-managed account is available", in.Amount)
	}
	return route{}, newErr(ErrNoRoute, "no subscription-capable provider account for %s", in.Currency)
}

func isManager(c connector.Connector) bool { _, ok := c.(connector.SubscriptionManager); return ok }

// Create validates the request, routes it, stores an incomplete subscription
// (with a pending mandate for merchant-triggered billing) and returns its
// checkout URL. Setup must complete within 24 hours.
func (s *Service) Create(ctx context.Context, in CreateInput) (Created, error) {
	if err := validateCreate(&in); err != nil {
		return Created{}, err
	}
	token, tokenHash := newToken()
	var sub store.Subscription
	err := s.db.Write(ctx, func(tx store.Tx) error {
		app, err := store.GetApp(ctx, tx, in.Scope.AppID)
		if err != nil {
			return mapNotFound(err, "app")
		}
		pol, err := s.loadPolicy(ctx, tx, app.ID)
		if err != nil {
			return err
		}
		rt, err := s.chooseRoute(ctx, tx, app.ID, in.Scope.Mode, in, pol)
		if err != nil {
			return err
		}
		now := s.nowUTC()
		cid, err := store.UpsertCustomer(ctx, tx, store.Customer{ID: ids.New("cus"), OrgID: app.OrgID, AppID: app.ID, Mode: in.Scope.Mode,
			ExternalRef: in.Customer.ExternalRef, Name: in.Customer.Name, Email: in.Customer.Email, Phone: in.Customer.Phone,
			Billing: in.Customer.Billing}, now)
		if err != nil {
			return err
		}
		anchor := now
		if in.StartAt.After(now) {
			anchor = in.StartAt.UTC()
		}
		var trialEnd time.Time
		if in.TrialDays > 0 {
			trialEnd = anchor.Add(time.Duration(in.TrialDays) * 24 * time.Hour)
			anchor = trialEnd
		}
		iv := billing.Interval(in.Interval)
		sub = store.Subscription{ID: ids.New("sub"), OrgID: app.OrgID, Mode: in.Scope.Mode, AppID: app.ID, CustomerID: cid,
			Status: "incomplete", BillingMode: rt.mode, ProviderAccountID: rt.accountID, ProviderRef: ids.ProviderRef(),
			Amount: in.Amount, Currency: in.Currency, Interval: in.Interval, IntervalCount: in.IntervalCount, AnchorAt: anchor,
			CurrentPeriodStart: anchor, CurrentPeriodEnd: billing.Boundary(anchor, iv, in.IntervalCount, 1), TrialEnd: trialEnd,
			TotalCount: in.TotalCount, Description: in.Description, Metadata: in.Metadata, ReturnURL: in.ReturnURL,
			AllowedConnectors: in.AllowedConnectors, Country: in.Country, CheckoutTokenHash: tokenHash, ExpiresAt: now.Add(IncompleteTTL), Version: 1, CreatedAt: now, UpdatedAt: now}
		if sub.ReturnURL == "" {
			sub.ReturnURL = app.DefaultReturnURL
		}
		if rt.mode == string(connector.MerchantTriggered) {
			m := store.Mandate{ID: ids.New("mdt"), OrgID: app.OrgID, Mode: in.Scope.Mode, AppID: app.ID, CustomerID: cid,
				ProviderAccountID: rt.accountID, ProviderRef: ids.ProviderRef(), Method: rt.method, Status: "pending",
				MaxAmount: rt.maxAmount, Currency: in.Currency, Frequency: in.Interval,
				ValidUntil: now.AddDate(pol.validityYears, 0, 0), CreatedAt: now, UpdatedAt: now}
			if err := s.insertMandate(ctx, tx, m); err != nil {
				return err
			}
			sub.MandateID = m.ID
			if err := s.emitMandate(ctx, tx, EvMandateCreated, m); err != nil {
				return err
			}
		}
		if err := store.InsertSubscription(ctx, tx, sub); err != nil {
			return err
		}
		if err := s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobExpireIncomplete, Payload: map[string]string{"subscription_id": sub.ID},
			RunAt: sub.ExpiresAt, DedupeKey: "sub:" + sub.ID + ":setup:expire"}); err != nil {
			return err
		}
		return s.emitSub(ctx, tx, EvCreated, sub, eventData{})
	})
	if err != nil {
		return Created{}, err
	}
	return Created{Subscription: sub, CheckoutURL: s.checkoutURL(sub.ID, token)}, nil
}
