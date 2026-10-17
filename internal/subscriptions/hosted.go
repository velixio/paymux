package subscriptions

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/web"
	"github.com/paymux/paymux/pkg/connector"
)

// CheckoutKind says what the hosted page must do.
type CheckoutKind string

const (
	CheckoutAction CheckoutKind = "action" // run Action
	CheckoutDone   CheckoutKind = "done"   // nothing to set up (already active)
	CheckoutClosed CheckoutKind = "closed" // canceled, ended or expired
)

// Checkout is the content of the hosted /s/{id} page.
type Checkout struct {
	Kind         CheckoutKind
	Action       connector.NextAction
	Connector    string
	Subscription store.Subscription
	AppName      string
}

// OpenCheckout verifies the checkout token and returns what to render. The
// first call creates the provider-side checkout (a provider subscription, or
// the mandate registration) through CreateSubscriptionCheckout or
// CreateMandateCheckout; its action is stored and reused by later loads of the
// page. Unknown ids and wrong tokens are both ErrNotFound.
func (s *Service) OpenCheckout(ctx context.Context, id, token string) (Checkout, error) {
	var sub store.Subscription
	var m store.Mandate
	var cust store.Customer
	var appName string
	err := s.db.Read(ctx, func(q store.Querier) error {
		var err error
		if sub, err = store.GetSubscription(ctx, q, id); err != nil {
			return err
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(sub.CheckoutTokenHash)) != 1 {
			return store.ErrNotFound
		}
		if app, err := store.GetApp(ctx, q, sub.AppID); err == nil {
			appName = app.Name
		}
		if cust, err = store.GetCustomer(ctx, q, sub.CustomerID); err != nil {
			return err
		}
		mid := sub.PendingMandateID
		if mid == "" && sub.Status == string(state.SubIncomplete) {
			mid = sub.MandateID
		}
		if mid != "" {
			m, err = store.GetMandate(ctx, q, mid)
		}
		return err
	})
	if err != nil {
		return Checkout{}, mapNotFound(err, "checkout")
	}
	co := Checkout{Subscription: sub, AppName: appName}
	switch {
	case terminal(sub.Status):
		co.Kind = CheckoutClosed
		return co, nil
	case sub.Status != string(state.SubIncomplete) && sub.PendingMandateID == "" && !reauthRetry(sub):
		co.Kind = CheckoutDone
		return co, nil
	}
	if needsFreshMandate(sub, m) {
		var err error
		if sub, m, err = s.freshMandate(ctx, sub.ID, token); err != nil {
			return Checkout{}, err
		}
		co.Subscription = sub
	}
	conn, err := s.connectors.Get(ctx, sub.ProviderAccountID)
	if err != nil {
		return Checkout{}, err
	}
	co.Kind, co.Connector = CheckoutAction, connectorName(conn)
	if sub.CheckoutAction != "" {
		return co, json.Unmarshal([]byte(sub.CheckoutAction), &co.Action)
	}
	customer := connector.Customer{Name: cust.Name, Email: cust.Email, Phone: cust.Phone}
	var res connector.CheckoutResult
	switch sub.BillingMode {
	case string(connector.ProviderManaged):
		mgr, ok := conn.(connector.SubscriptionManager)
		if !ok {
			return Checkout{}, newErr(ErrUnsupported, "connector cannot manage subscriptions")
		}
		req := connector.SubscriptionRequest{SubscriptionID: sub.ID, ProviderRef: sub.ProviderRef, Amount: sub.Amount,
			Currency: sub.Currency, Interval: sub.Interval, IntervalCount: sub.IntervalCount, TotalCount: sub.TotalCount,
			Description: sub.Description, Customer: customer, Metadata: sub.Metadata}
		if sub.AnchorAt.After(s.nowUTC()) {
			req.StartAt = sub.AnchorAt.UnixMilli()
		}
		res, err = mgr.CreateSubscriptionCheckout(ctx, req)
	default:
		mm, ok := conn.(connector.MandateManager)
		if !ok {
			return Checkout{}, newErr(ErrUnsupported, "connector cannot manage mandates")
		}
		// The registration transaction pays the first period itself (RBI: the
		// first debit carries the customer's authentication). With a trial, or
		// when the subscription is already running (re-authorization, new
		// payment method), nothing is due yet: the provider's verification amount.
		amount := conn.Info().MandateSetupAmount
		if amount <= 0 {
			amount = defaultSetupAmount
		}
		if sub.Status == string(state.SubIncomplete) && sub.TrialEnd.IsZero() {
			amount = sub.Amount
		}
		req := connector.MandateRequest{MandateID: m.ID, ProviderRef: m.ProviderRef, Amount: amount, MaxAmount: m.MaxAmount,
			Currency: m.Currency, Method: m.Method, Customer: customer}
		if !m.ValidUntil.IsZero() {
			req.ValidUntil = m.ValidUntil.UnixMilli()
		}
		res, err = mm.CreateMandateCheckout(ctx, req)
	}
	if err != nil {
		return Checkout{}, err
	}
	raw, _ := json.Marshal(res.NextAction)
	err = s.db.Write(ctx, func(tx store.Tx) error {
		cur, err := s.lockSub(ctx, tx, sub.ID)
		if err != nil {
			return err
		}
		if cur.CheckoutAction != "" || cur.CheckoutTokenHash != sub.CheckoutTokenHash {
			return nil // another load won, or the token was replaced meanwhile
		}
		cur.CheckoutAction = string(raw)
		if res.ProviderOrderID != "" && sub.BillingMode == string(connector.ProviderManaged) && cur.ProviderSubscriptionID == "" {
			cur.ProviderSubscriptionID = res.ProviderOrderID
		}
		if res.ProviderOrderID != "" && sub.BillingMode == string(connector.MerchantTriggered) && m.ProviderMandateID == "" {
			m.ProviderMandateID = res.ProviderOrderID
			if err := store.UpdateMandate(ctx, tx, m, s.nowUTC()); err != nil {
				return err
			}
		}
		return s.saveSub(ctx, tx, &cur)
	})
	co.Action = res.NextAction
	return co, err
}

// connectorName returns the connector's registered name.
func connectorName(c connector.Connector) string { return c.Info().Name }

// ServeHosted serves GET /s/{id}?t=: the hosted subscription checkout.
// redirect actions answer 303; form_post and sdk actions render the shared
// pay_* templates; finished subscriptions get a short result page.
func (s *Service) ServeHosted(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "private, no-store")
	co, err := s.OpenCheckout(r.Context(), r.PathValue("id"), r.URL.Query().Get("t"))
	switch {
	case errors.Is(err, ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "this checkout could not be started, please try again later", http.StatusBadGateway)
		return
	}
	pages, err := s.pages()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	page := web.PayPage{AppName: co.AppName}
	tmpl := "pay_result"
	switch co.Kind {
	case CheckoutClosed:
		page.Title, page.Message = "Subscription closed", "This subscription is no longer open for setup."
	case CheckoutDone:
		page.Title, page.Message = "Subscription active", "This subscription is already set up."
	default:
		switch co.Action.Kind {
		case connector.ActionRedirect:
			http.Redirect(w, r, co.Action.URL, http.StatusSeeOther)
			return
		case connector.ActionFormPost:
			tmpl, page.FormURL = "pay_form_post", co.Action.URL
			keys := make([]string, 0, len(co.Action.Fields))
			for k := range co.Action.Fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				page.Fields = append(page.Fields, web.Field{Name: k, Value: co.Action.Fields[k]})
			}
		case connector.ActionSDK:
			tmpl, page.ScriptURL, page.Launcher, page.Options = "pay_sdk", co.Action.ScriptURL, co.Connector, co.Action.Options
		default:
			http.Error(w, "unsupported checkout action", http.StatusBadGateway)
			return
		}
	}
	if co.Kind != CheckoutAction {
		page.ReturnURL = returnURL(co.Subscription)
	}
	var buf bytes.Buffer
	if err := pages.Render(&buf, tmpl, page); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// defaultSetupAmount (Rs 1) is the registration amount of a connector that
// declares none.
const defaultSetupAmount int64 = 100

// reauthRetry: a running merchant-triggered subscription whose
// re-authorization mandate failed and must be offered again.
func reauthRetry(sub store.Subscription) bool {
	return sub.BillingMode == string(connector.MerchantTriggered) && sub.RequiresReauthorization && sub.PendingMandateID == ""
}

// needsFreshMandate reports whether the mandate to check out has been used up
// by a failed or expired registration. Providers refuse a reference they have
// seen, so the retry registers a new mandate on the same subscription.
func needsFreshMandate(sub store.Subscription, m store.Mandate) bool {
	if sub.BillingMode != string(connector.MerchantTriggered) {
		return false
	}
	if reauthRetry(sub) {
		return true
	}
	return sub.Status == string(state.SubIncomplete) && (m.Status == string(state.MandateFailed) || m.Status == string(state.MandateExpired))
}

// freshMandate gives the subscription a new pending mandate (new provider
// reference, same method and size) for a registration retry. The checkout
// token stays the same, so the link the customer is on keeps working.
func (s *Service) freshMandate(ctx context.Context, subID, token string) (store.Subscription, store.Mandate, error) {
	var sub store.Subscription
	var m store.Mandate
	err := s.db.Write(ctx, func(tx store.Tx) error {
		var err error
		if sub, err = s.lockSub(ctx, tx, subID); err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(sub.CheckoutTokenHash)) != 1 {
			return newErr(ErrNotFound, "checkout not found")
		}
		pol, err := s.loadPolicy(ctx, tx, sub.AppID)
		if err != nil {
			return err
		}
		old, err := store.GetMandate(ctx, tx, firstNonEmpty(sub.MandateID, sub.PendingMandateID))
		if err != nil {
			return err
		}
		keep := sub.CheckoutTokenHash
		switch {
		case reauthRetry(sub):
			if m, _, err = s.newPendingMandate(ctx, tx, &sub, pol, old.Method, max(sub.Amount, sub.PendingAmount)); err != nil {
				return err
			}
			sub.CheckoutTokenHash = keep
		case sub.Status == string(state.SubIncomplete) && (old.Status == string(state.MandateFailed) || old.Status == string(state.MandateExpired)) && sub.MandateID == old.ID:
			now := s.nowUTC()
			m = store.Mandate{ID: ids.New("mdt"), OrgID: sub.OrgID, Mode: sub.Mode, AppID: sub.AppID, CustomerID: sub.CustomerID,
				ProviderAccountID: sub.ProviderAccountID, ProviderRef: ids.ProviderRef(), Method: old.Method, Status: string(state.MandatePending),
				MaxAmount: old.MaxAmount, Currency: old.Currency, Frequency: old.Frequency,
				ValidUntil: now.AddDate(pol.validityYears, 0, 0), CreatedAt: now, UpdatedAt: now}
			if err := store.InsertMandate(ctx, tx, m); err != nil {
				return err
			}
			sub.MandateID, sub.CheckoutAction = m.ID, ""
			if err := s.emitMandate(ctx, tx, EvMandateCreated, m); err != nil {
				return err
			}
		default:
			// Someone else already replaced it; use the current one.
			mid := firstNonEmpty(sub.PendingMandateID, sub.MandateID)
			if m, err = store.GetMandate(ctx, tx, mid); err != nil {
				return err
			}
			return nil
		}
		return s.saveSub(ctx, tx, &sub)
	})
	return sub, m, err
}
