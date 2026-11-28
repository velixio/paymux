package connector

import "testing"

type cryptoURLs struct{ origin string }

func (u cryptoURLs) ReturnURL(string) string { return "" }
func (u cryptoURLs) WebhookURL() string      { return "https://hub.example.com/hooks/pa_1" }
func (u cryptoURLs) CheckoutOrigin() string  { return u.origin }

func TestCryptoPageURL(t *testing.T) {
	got, err := CryptoPageURL(cryptoURLs{}, "ref/1")
	if err != nil || got != "https://hub.example.com/crypto/ref%2F1?pa=pa_1" {
		t.Fatalf("%q %v", got, err)
	}
	got, _ = CryptoPageURL(cryptoURLs{origin: "https://pay.example.com"}, "r")
	if got != "https://pay.example.com/crypto/r?pa=pa_1" {
		t.Fatalf("checkout origin: %q", got)
	}
	if h := CryptoCheckoutHosts(cryptoURLs{}); h.Self != "https://hub.example.com" {
		t.Fatalf("hosts %+v", h)
	}
	if _, err := CryptoPageURL(nil, "r"); err == nil {
		t.Fatal("nil URLs accepted")
	}
	if (CryptoCoin{Currency: "USDT", Network: "TRC20"}).Code() != "USDT_TRC20" {
		t.Fatal("code")
	}
}
