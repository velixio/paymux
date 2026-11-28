package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// cryptoCheckoutResp is GET and POST /v1/payments/{id}/crypto: a crypto
// checkout for an app that shows its own pay page instead of the hub's.
type cryptoCheckoutResp struct {
	Object    string                   `json:"object"` // "crypto_checkout"
	PaymentID string                   `json:"payment_id"`
	Amount    int64                    `json:"amount"`
	Currency  string                   `json:"currency"`
	Coins     []connector.CryptoCoin   `json:"coins"`
	Invoice   *connector.CryptoInvoice `json:"invoice,omitempty"`
}

type cryptoChooseReq struct {
	Coin    string `json:"coin"`               // CURRENCY_NETWORK, one of coins
	PayerIP string `json:"payer_ip,omitempty"` // the buyer's IP, for the provider's risk checks
}

// cryptoTarget is the crypto controller and provider reference of payment
// id's latest attempt.
func (a *API) cryptoTarget(w http.ResponseWriter, r *http.Request, pr core.Principal) (connector.CryptoCheckout, string, store.Payment, bool) {
	p, err := a.core.GetPayment(r.Context(), scope(pr), r.PathValue("id"))
	if err != nil {
		a.failCore(w, r, err)
		return nil, "", p, false
	}
	var attempts []store.Attempt
	if err := a.db.Read(r.Context(), func(q store.Querier) (err error) {
		attempts, err = store.ListAttempts(r.Context(), q, p.ID)
		return err
	}); err != nil {
		a.fail(w, r, http.StatusInternalServerError, "internal", "the payment's attempts could not be read")
		return nil, "", p, false
	}
	if a.checkout != nil && len(attempts) > 0 {
		last := attempts[len(attempts)-1]
		if live, err := a.checkout.LiveConnector(r.Context(), last.ProviderAccountID); err == nil {
			if ctl, ok := live.Connector.(connector.CryptoCheckout); ok {
				return ctl, last.ProviderRef, p, true
			}
		}
	}
	a.fail(w, r, http.StatusConflict, "not_crypto",
		"this payment has no crypto checkout: start one with POST /v1/payments/{id}/checkout-action routed to a crypto provider account")
	return nil, "", p, false
}

func (a *API) getCryptoCheckout(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	ctl, ref, p, ok := a.cryptoTarget(w, r, pr)
	if !ok {
		return
	}
	co, err := ctl.CryptoState(r.Context(), ref)
	a.writeCrypto(w, r, ctl, p, co, err)
}

func (a *API) chooseCryptoCoin(w http.ResponseWriter, r *http.Request, pr core.Principal, body []byte) {
	if !a.allow(w, r, pr, authz.PaymentCreate) {
		return
	}
	var req cryptoChooseReq
	if !a.decodeBody(w, r, body, &req, true) {
		return
	}
	ctl, ref, p, ok := a.cryptoTarget(w, r, pr)
	if !ok {
		return
	}
	co, err := ctl.ChooseCryptoCoin(r.Context(), ref, req.Coin, req.PayerIP)
	switch {
	case errors.Is(err, connector.ErrCoinRefused):
		a.fail(w, r, http.StatusBadRequest, "coin_refused", "this coin cannot be used: choose another from coins")
		return
	case errors.Is(err, connector.ErrCheckoutClosed):
		err = nil // already paid or confirming: show where it stands
	}
	a.writeCrypto(w, r, ctl, p, co, err)
}

func (a *API) writeCrypto(w http.ResponseWriter, r *http.Request, ctl connector.CryptoCheckout, p store.Payment, co connector.CryptoState, err error) {
	switch {
	case errors.Is(err, connector.ErrNotFound):
		a.fail(w, r, http.StatusNotFound, "not_found", "the crypto checkout was not found")
		return
	case err != nil:
		a.log.Warn("httpapi: crypto checkout", "payment", p.ID, "err", err)
		a.fail(w, r, http.StatusBadGateway, "provider_unavailable", "the crypto provider could not be reached; try again shortly")
		return
	}
	if co.Invoice != nil {
		co.Invoice.ExpiresAt = co.Invoice.ExpiresAt.UTC().Truncate(time.Second)
	}
	writeJSON(w, http.StatusOK, cryptoCheckoutResp{Object: "crypto_checkout", PaymentID: p.ID, Amount: co.Amount,
		Currency: co.Currency, Coins: ctl.CryptoCoins(), Invoice: co.Invoice})
}
