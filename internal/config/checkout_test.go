package config

import "testing"

// B-7c: PAYMUX_CHECKOUT_URL puts the customer pages on their own origin.
func TestCheckoutURLRules(t *testing.T) {
	c, err := Load(base())
	if err != nil || c.CheckoutURL != nil || c.CustomerURL().String() != "https://pay.example.com" {
		t.Fatalf("unset: %+v %v", c.CheckoutURL, err)
	}
	c, err = Load(base("PAYMUX_CHECKOUT_URL", "https://checkout.example.com"))
	if err != nil || c.CheckoutURL == nil || c.CustomerURL().String() != "https://checkout.example.com" {
		t.Fatalf("set: %v", err)
	}
	// http only for the hosts PAYMUX_BASE_URL allows it for.
	if _, err := Load(env("PAYMUX_BASE_URL", "http://a.localhost:8080", "PAYMUX_CHECKOUT_URL", "http://b.localhost:8080", "PAYMUX_MASTER_KEY", goodKey)); err != nil {
		t.Errorf("local http: %v", err)
	}
	for _, u := range []string{"http://checkout.example.com", "ftp://x", "checkout.example.com",
		"https://checkout.example.com/pay", "https://checkout.example.com/?a=1", "https://user@checkout.example.com"} {
		_, err := Load(base("PAYMUX_CHECKOUT_URL", u))
		wantErr(t, err, "PAYMUX_CHECKOUT_URL")
	}
	// Same host name as the base URL: cookies are per host name, not per port or scheme.
	for _, u := range []string{"https://pay.example.com", "https://PAY.example.com:8443", "https://pay.example.com/"} {
		_, err := Load(base("PAYMUX_CHECKOUT_URL", u))
		wantErr(t, err, "PAYMUX_CHECKOUT_URL must be on a different host")
	}
}

func TestCheckoutURLWarning(t *testing.T) {
	prod, err := Load(base())
	if err != nil {
		t.Fatal(err)
	}
	if CheckoutURLWarning(prod) == "" {
		t.Error("production without PAYMUX_CHECKOUT_URL: no warning")
	}
	set, err := Load(base("PAYMUX_CHECKOUT_URL", "https://checkout.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if w := CheckoutURLWarning(set); w != "" {
		t.Errorf("warned although set: %q", w)
	}
	dev, err := Load(base("PAYMUX_ENV", "development"))
	if err != nil {
		t.Fatal(err)
	}
	if w := CheckoutURLWarning(dev); w != "" {
		t.Errorf("warned in development: %q", w)
	}
}
