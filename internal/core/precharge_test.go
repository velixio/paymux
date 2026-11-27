package core_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/iprep"
	"github.com/velixio/paymux/internal/store"
)

// live moves e to live mode, where the pre-charge rules apply.
func (e *env) live() {
	e.t.Helper()
	if err := e.db.Write(ctx, func(tx store.Tx) error {
		return store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: "pa_live", OrgID: store.DefaultOrgID, Mode: "live",
			Connector: "fake", Name: "Fake live", Status: "active", CreatedAt: time.Now()})
	}); err != nil {
		e.t.Fatal(err)
	}
	e.scope, e.pa = core.Scope{AppID: "app_1", Mode: "live"}, "pa_live"
}

// buyerPayment is a payment whose app sent buyer as the buyer's IP, for a
// customer with email billed in country.
func (e *env) buyerPayment(buyer, email, country string) store.Payment {
	e.t.Helper()
	in := core.CreatePaymentInput{Scope: e.scope, Amount: 500, Currency: "INR", BuyerIP: buyer}
	if email != "" {
		in.Customer = &core.CustomerInput{ExternalRef: email, Email: email, Billing: store.Billing{Country: country}}
	}
	res, err := e.svc.CreatePayment(ctx, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Payment
}

// tryCheckout starts an attempt on p and returns the block flag that refused
// it, or "" when it started.
func (e *env) tryCheckout(p store.Payment) (store.Attempt, string) {
	e.t.Helper()
	a, err := e.svc.CreateAttempt(ctx, p.ID, e.pa)
	if err == nil {
		return a, ""
	}
	if !errors.Is(err, core.ErrBlocked) {
		e.t.Fatalf("CreateAttempt: %v", err)
	}
	got, gerr := e.svc.GetPaymentByID(ctx, p.ID)
	if gerr != nil || !core.Blocked(got) {
		e.t.Fatalf("blocked payment not tagged: %v %v", got.Flags, gerr)
	}
	for _, f := range []string{core.FlagBlockedVPN, core.FlagBlockedCountry, core.FlagBlockedFraudLink, core.FlagBlockedIPLimit, core.FlagBlockedIPDeclines, core.FlagBlockedEmailLimit} {
		if got.HasFlag(f) {
			return store.Attempt{}, f
		}
	}
	e.t.Fatalf("blocked payment names no rule: %v", got.Flags)
	return store.Attempt{}, ""
}

func (e *env) setOrg(key, value string) {
	e.t.Helper()
	if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, key, []byte(value)); err != nil {
		e.t.Fatal(err)
	}
}

func TestVPNCheckoutIsRefusedBeforeAnyCharge(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.live()
		e.ips.set("170.62.103.12", iprep.Info{Country: "US", VPN: true})
		p := e.buyerPayment("170.62.103.12", "", "")
		if _, flag := e.tryCheckout(p); flag != core.FlagBlockedVPN {
			t.Fatalf("VPN buyer: %q", flag)
		}
		if !slices.Contains(e.alertKinds(), "payment.risk_blocked") {
			t.Fatalf("no alert: %v", e.alertKinds())
		}
		// A refused payment stays refused, with one alert, until allowed.
		if _, err := e.svc.CreateAttempt(ctx, p.ID, e.pa); !errors.Is(err, core.ErrBlocked) {
			t.Fatalf("second try: %v", err)
		}
		n := 0
		for _, k := range e.alertKinds() {
			if k == "payment.risk_blocked" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("alerts = %d", n)
		}
		if changed, err := e.svc.AllowCheckout(ctx, audit.System, p.ID); err != nil || !changed {
			t.Fatalf("allow: %v %v", changed, err)
		}
		if _, flag := e.tryCheckout(p); flag != "" {
			t.Fatalf("allowed payment refused: %q", flag)
		}
		// Off, a VPN buyer may pay.
		e.setOrg("block_vpn_checkout", "false")
		if _, flag := e.tryCheckout(e.buyerPayment("170.62.103.12", "", "")); flag != "" {
			t.Fatalf("rule off: %q", flag)
		}
	})
}

func TestCheckoutFromAnotherCountryThanBillingIsRefused(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.live()
		e.ips.set("190.24.187.83", iprep.Info{Country: "CO"})
		if _, flag := e.tryCheckout(e.buyerPayment("190.24.187.83", "a@example.com", "US")); flag != core.FlagBlockedCountry {
			t.Fatalf("billed in the US, IP in Colombia: %q", flag)
		}
		if _, flag := e.tryCheckout(e.buyerPayment("190.24.187.83", "b@example.com", "CO")); flag != "" {
			t.Fatalf("same country: %q", flag)
		}
		// An IP the lookup cannot place, or no billing country: not judged.
		if _, flag := e.tryCheckout(e.buyerPayment("203.0.113.99", "c@example.com", "US")); flag != "" {
			t.Fatalf("lookup failed: %q", flag)
		}
		if _, flag := e.tryCheckout(e.buyerPayment("190.24.187.83", "", "")); flag != "" {
			t.Fatalf("no billing: %q", flag)
		}
	})
}

func TestTooManyCheckoutsFromOneIPAreRefused(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.live()
		ip := "198.51.100.7"
		e.ips.set(ip, iprep.Info{Country: "IN"})
		// A payment that never started a checkout does not count.
		_ = e.buyerPayment(ip, "", "")
		for i := 0; i < 2; i++ {
			if _, flag := e.tryCheckout(e.buyerPayment(ip, "", "")); flag != "" {
				t.Fatalf("checkout %d: %q", i+1, flag)
			}
		}
		if _, flag := e.tryCheckout(e.buyerPayment(ip, "", "")); flag != core.FlagBlockedIPLimit {
			t.Fatalf("third checkout: %q", flag)
		}
		// Off, any number of payments may start from one IP.
		e.setOrg("ip_payment_limit", "0")
		if _, flag := e.tryCheckout(e.buyerPayment(ip, "", "")); flag != "" {
			t.Fatalf("rule off: %q", flag)
		}
	})
}

func TestDeclinedCardsFromOneIPBlockIt(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.live()
		e.setOrg("ip_payment_limit", "0")
		ip := "198.51.100.8"
		e.ips.set(ip, iprep.Info{Country: "IN"})
		p := e.buyerPayment(ip, "", "")
		for i, last4 := range []string{"4034", "8621"} {
			a, flag := e.tryCheckout(p)
			if flag != "" {
				t.Fatalf("try %d: %q", i+1, flag)
			}
			e.apply(a.ID, decline(i, last4))
		}
		if _, flag := e.tryCheckout(e.buyerPayment(ip, "", "")); flag != core.FlagBlockedIPDeclines {
			t.Fatalf("after two declined cards: %q", flag)
		}
	})
}

func TestPaidPaymentsPerEmailAreLimited(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.live()
		e.setOrg("ip_payment_limit", "0")
		e.setOrg("email_paid_limit", "2")
		for i := 0; i < 2; i++ {
			a, flag := e.tryCheckout(e.buyerPayment("", "ring@example.com", ""))
			if flag != "" {
				t.Fatalf("payment %d: %q", i+1, flag)
			}
			if r := e.apply(a.ID, paidWith(500, "4034", 10)); r.Payment.Status != "succeeded" {
				t.Fatalf("payment %d: %s %v", i+1, r.Payment.Status, r.Payment.Flags)
			}
		}
		if _, flag := e.tryCheckout(e.buyerPayment("", "RING@example.com", "")); flag != core.FlagBlockedEmailLimit {
			t.Fatalf("third paid payment: %q", flag)
		}
	})
}

func TestFraudLinkedBuyersAreRefused(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.live()
		e.setOrg("ip_payment_limit", "0")
		ip := "198.51.100.9"
		e.ips.set(ip, iprep.Info{Country: "IN"})
		first := e.buyerPayment(ip, "casader@example.com", "")
		if _, flag := e.tryCheckout(first); flag != "" {
			t.Fatalf("first: %q", flag)
		}
		if _, err := e.svc.SetFraud(ctx, audit.System, first.ID, true); err != nil {
			t.Fatal(err)
		}
		if _, flag := e.tryCheckout(e.buyerPayment(ip, "other@example.com", "")); flag != core.FlagBlockedFraudLink {
			t.Fatalf("same IP, new email: %q", flag)
		}
		if _, flag := e.tryCheckout(e.buyerPayment("198.51.100.10", "casader@example.com", "")); flag != core.FlagBlockedFraudLink {
			t.Fatalf("same email, new IP: %q", flag)
		}
		if _, flag := e.tryCheckout(e.buyerPayment("198.51.100.11", "new@example.com", "")); flag != "" {
			t.Fatalf("unlinked buyer: %q", flag)
		}
	})
}

func TestTestModeIsNeverRefused(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.ips.set("170.62.103.12", iprep.Info{Country: "US", VPN: true})
		if _, flag := e.tryCheckout(e.buyerPayment("170.62.103.12", "a@example.com", "IN")); flag != "" {
			t.Fatalf("test mode: %q", flag)
		}
	})
}
