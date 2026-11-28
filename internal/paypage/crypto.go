package paypage

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/velixio/paymux/internal/qrcode"
	"github.com/velixio/paymux/internal/web"
	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/money"
)

// RegisterCrypto mounts the crypto checkout page, /crypto/{ref}, on the hosts
// that serve the simulator's page: the buyer picks a coin, then sees what to
// send where until the payment lands.
func (h *Handler) RegisterCrypto(m Mux) {
	m.HandleFunc("GET /crypto/{ref}", withLook(h.cryptoPage))
	m.HandleFunc("POST /crypto/{ref}", withLook(h.cryptoChoose))
}

// cryptoTarget finds the crypto checkout connector of the provider account named in
// the query; anything else is a plain 404.
func (h *Handler) cryptoTarget(w http.ResponseWriter, r *http.Request) (connector.CryptoCheckout, string, string, bool) {
	ref, pa := r.PathValue("ref"), r.URL.Query().Get("pa")
	if ref == "" || pa == "" {
		h.notFound(w)
		return nil, "", "", false
	}
	live, err := h.o.Flow.LiveConnector(r.Context(), pa)
	if err != nil {
		h.notFound(w)
		return nil, "", "", false
	}
	ctl, ok := live.Connector.(connector.CryptoCheckout)
	if !ok {
		h.notFound(w)
		return nil, "", "", false
	}
	return ctl, ref, pa, true
}

func (h *Handler) cryptoPage(w http.ResponseWriter, r *http.Request) {
	ctl, ref, pa, ok := h.cryptoTarget(w, r)
	if !ok {
		return
	}
	co, err := ctl.CryptoState(r.Context(), ref)
	h.renderCrypto(w, r, ctl, ref, pa, co, err, "")
}

func (h *Handler) cryptoChoose(w http.ResponseWriter, r *http.Request) {
	ctl, ref, pa, ok := h.cryptoTarget(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		h.result(w, http.StatusBadRequest, "", "result.link_invalid.title", "result.link_invalid.msg", "")
		return
	}
	co, err := ctl.ChooseCryptoCoin(r.Context(), ref, r.PostForm.Get("coin"), h.o.ClientIP(r))
	switch {
	case errors.Is(err, connector.ErrCoinRefused):
		co, err = ctl.CryptoState(r.Context(), ref)
		h.renderCrypto(w, r, ctl, ref, pa, co, err, "This coin can't be used right now. Choose another one.")
		return
	case errors.Is(err, connector.ErrCheckoutClosed):
	case err != nil:
		h.renderCrypto(w, r, ctl, ref, pa, co, err, "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, cryptoAction(ref, pa), http.StatusSeeOther)
}

func cryptoAction(ref, pa string) string {
	return "/crypto/" + url.PathEscape(ref) + "?pa=" + url.QueryEscape(pa)
}

func (h *Handler) renderCrypto(w http.ResponseWriter, r *http.Request, ctl connector.CryptoCheckout, ref, pa string, co connector.CryptoState, err error, msg string) {
	if errors.Is(err, connector.ErrNotFound) {
		h.notFound(w)
		return
	}
	if err != nil {
		h.serverError(w, "", err)
		return
	}
	amount, ferr := money.FormatDecimal(co.Amount, co.Currency)
	if ferr != nil {
		amount = strconv.FormatInt(co.Amount, 10)
	}
	ret := "/return/" + url.PathEscape(pa) + "/" + url.PathEscape(ref)
	cp := &web.CryptoPage{Amount: amount, Currency: co.Currency, Action: cryptoAction(ref, pa), ReturnURL: ret, Error: msg}
	labels := map[string]string{}
	for _, c := range ctl.CryptoCoins() {
		cp.Coins = append(cp.Coins, web.CryptoCoin{Code: c.Code(), Label: c.Label})
		labels[c.Code()] = c.Label
	}
	if inv := co.Invoice; inv != nil {
		cp.Coin, cp.Send, cp.Address = inv.Coin, inv.Amount, inv.Address
		cp.CoinLabel = labels[inv.Coin]
		if cp.CoinLabel == "" {
			cp.CoinLabel = inv.Currency + " " + inv.Network
		}
		if !inv.ExpiresAt.IsZero() {
			cp.Expires = inv.ExpiresAt.UTC().Format("15:04 MST, 2 Jan")
		}
		cp.Confirmations, cp.ConfNeeded = inv.Confirmations, inv.ConfNeeded
		switch inv.Status {
		case "paid":
			// Back to the merchant through the normal return path.
			http.Redirect(w, r, ret, http.StatusSeeOther)
			return
		case "processing":
			cp.Confirming = true
		case "expired":
			cp.Expired = true
		default:
			cp.Pending = true
			if code, qerr := qrcode.New(inv.Address); qerr == nil {
				cp.QRPath, cp.QRBox = code.Path(), code.ViewBox()
			}
		}
	}
	h.page(w, http.StatusOK, "crypto_checkout", web.PayPage{Crypto: cp})
}
