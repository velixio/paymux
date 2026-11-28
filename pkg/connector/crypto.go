package connector

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// CryptoCheckout is implemented by a connector whose buyer pays by sending
// crypto to an address, from a gateway with no hosted page of its own. Its
// CreateCheckout redirects to the hub's crypto page (CryptoPageURL), where the
// buyer picks a coin and sees what to send where; an app that shows its own
// page drives the same calls through GET and POST /v1/payments/{id}/crypto.
// The payment's truth still comes from FetchPayment and webhooks.
type CryptoCheckout interface {
	// CryptoCoins are the coins the buyer may choose from.
	CryptoCoins() []CryptoCoin
	// CryptoState is the checkout of ref with its current invoice, if any.
	CryptoState(ctx context.Context, ref string) (CryptoState, error)
	// ChooseCryptoCoin starts (or, for the same coin, returns) the invoice
	// that pays ref with coin (CryptoCoin.Code). payerIP may be empty. It
	// fails with ErrCoinRefused for a coin it cannot take and with
	// ErrCheckoutClosed, with the state, once ref is paid or confirming.
	ChooseCryptoCoin(ctx context.Context, ref, coin, payerIP string) (CryptoState, error)
}

// Errors of CryptoCheckout.ChooseCryptoCoin.
var (
	ErrCoinRefused    = errors.New("connector: this coin cannot be used")
	ErrCheckoutClosed = errors.New("connector: this checkout is already paid")
)

// CryptoCoin is one currency on one network.
type CryptoCoin struct {
	Currency string `json:"currency"` // USDT
	Network  string `json:"network"`  // TRC20
	Label    string `json:"label"`    // USDT · Tron (TRC20)
}

// Code is the coin as CURRENCY_NETWORK (USDT_TRC20).
func (c CryptoCoin) Code() string { return c.Currency + "_" + c.Network }

// CryptoState is a crypto checkout as the buyer sees it.
type CryptoState struct {
	Amount   int64          `json:"amount"` // minor units of Currency
	Currency string         `json:"currency"`
	Invoice  *CryptoInvoice `json:"invoice,omitempty"` // nil until a coin is chosen
}

// Crypto invoice statuses.
const (
	CryptoPending    = "pending"    // waiting for the transfer
	CryptoProcessing = "processing" // seen on chain, waiting for confirmations
	CryptoPaid       = "paid"
	CryptoExpired    = "expired"
)

// CryptoInvoice is what the buyer must send, and how far the payment is.
type CryptoInvoice struct {
	ID            string    `json:"id"`
	Status        string    `json:"status"` // one of the Crypto* statuses
	Coin          string    `json:"coin"`   // CURRENCY_NETWORK
	Currency      string    `json:"currency"`
	Network       string    `json:"network"`
	Amount        string    `json:"amount"` // exact decimal text to send
	Address       string    `json:"address"`
	Confirmations int       `json:"confirmations"`
	ConfNeeded    int       `json:"confirmations_needed"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// CryptoPageURL is the hub's crypto checkout page for ref, on the checkout
// origin when the hub has one. The account is the last element of the
// webhook URL the hub gave the connector.
func CryptoPageURL(urls URLs, ref string) (string, error) {
	origin, account, err := hubOrigin(urls)
	if err != nil {
		return "", err
	}
	return origin + "/crypto/" + url.PathEscape(ref) + "?pa=" + url.QueryEscape(account), nil
}

// CryptoCheckoutHosts is what a CryptoCheckout connector's CheckoutHosts
// returns: the buyer only goes to the hub's own crypto page.
func CryptoCheckoutHosts(urls URLs) CheckoutHosts {
	origin, _, err := hubOrigin(urls)
	if err != nil {
		return CheckoutHosts{}
	}
	return CheckoutHosts{Self: origin}
}

func hubOrigin(urls URLs) (origin, account string, err error) {
	if urls == nil {
		return "", "", errors.New("connector: no URLs configured")
	}
	u, err := url.Parse(urls.WebhookURL())
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("connector: bad webhook URL %q", urls.WebhookURL())
	}
	origin = u.Scheme + "://" + u.Host
	if co, ok := urls.(interface{ CheckoutOrigin() string }); ok && co.CheckoutOrigin() != "" {
		origin = co.CheckoutOrigin()
	}
	return origin, strings.TrimPrefix(u.Path[strings.LastIndex(u.Path, "/"):], "/"), nil
}
