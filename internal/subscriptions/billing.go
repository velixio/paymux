package subscriptions

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"

	"github.com/velixio/paymux/internal/billingform"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/look"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/web"
)

const maxBillingForm = 1 << 20

// billingNeed is what the hosted page must ask: the same rules as the pay page
// (billing_details, issue_invoices), applied to the subscription's price and
// currency and to what the customer already has.
func (s *Service) billingNeed(ctx context.Context, sub store.Subscription, cust store.Customer) (billingform.Need, error) {
	mode, issue, err := billingform.Settings(ctx, s.settings, sub.AppID)
	if err != nil {
		return billingform.Need{}, err
	}
	return billingform.NeedFor(mode, issue, sub.Currency, sub.Amount, cust, true), nil
}

// csrf binds the form to this checkout: the token in the link is its secret.
func (s *Service) csrf(subID, token string) string {
	m := hmac.New(sha256.New, s.csrfKey)
	m.Write([]byte("paymux-sub-csrf|" + subID + "|" + token))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Service) renderBilling(w http.ResponseWriter, r *http.Request, pages *web.Renderer, lang string, co Checkout, msgKey string, status int) {
	token := r.URL.Query().Get("t")
	form := "/s/" + co.Subscription.ID + "?t=" + url.QueryEscape(token)
	if q := r.URL.Query().Get("lang"); q != "" && pages.HasLang(q) {
		form += "&lang=" + url.QueryEscape(q)
	}
	data := web.PayPage{AppName: co.AppName, FormURL: form, CSRF: s.csrf(co.Subscription.ID, token)}
	if msgKey != "" {
		data.Error = pages.T(lang, msgKey)
	}
	billingform.Fill(&data, co.Customer, co.Subscription.Currency, co.Need)
	web.SetHostedCSP(w.Header(), &data)
	var buf bytes.Buffer
	if err := pages.Render(&buf, "pay_billing", data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// SubmitBilling handles the billing form of POST /s/{id}?t=: it checks the
// answers like the pay page does, stores them on the customer (every invoice of
// the subscription, renewals included, uses them) and sends the buyer back to
// the checkout, which now goes on to the provider.
func (s *Service) SubmitBilling(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	id, token := r.PathValue("id"), r.URL.Query().Get("t")
	sub, _, cust, appName, err := s.loadCheckout(r.Context(), id, token)
	switch {
	case errors.Is(err, ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBillingForm)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !hmac.Equal([]byte(r.PostForm.Get("csrf")), []byte(s.csrf(sub.ID, token))) {
		http.Error(w, "this form has expired; open the checkout link again", http.StatusForbidden)
		return
	}
	back := "/s/" + sub.ID + "?t=" + url.QueryEscape(token)
	if q := r.URL.Query().Get("lang"); q != "" {
		back += "&lang=" + url.QueryEscape(q)
	}
	if terminal(sub.Status) || sub.Status != string(state.SubIncomplete) && sub.PendingMandateID == "" && !reauthRetry(sub) || sub.CheckoutAction != "" {
		http.Redirect(w, r, back, http.StatusSeeOther) // nothing left to ask
		return
	}
	need, err := s.billingNeed(r.Context(), sub, cust)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	in, key := billingform.Parse(r.PostForm, sub.Currency, need)
	if key != "" {
		pages, perr := s.pages()
		if perr != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		lk := look.Resolve(r.Context(), s.settings, pages, sub.AppID, r.URL.Query().Get("lang"))
		cust.Name, cust.Email, cust.Billing = in.Name, in.Email, in.Billing // show what was typed
		co := Checkout{Kind: CheckoutBilling, Subscription: sub, AppName: appName, Customer: cust, Need: need}
		s.renderBilling(w, r, lk.Bind(pages), lk.Lang, co, key, http.StatusBadRequest)
		return
	}
	if err := s.core.SetCustomerBilling(r.Context(), sub.CustomerID, in); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
