package core_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/iprep"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// fakeLooker answers from info and counts the lookups; an IP it does not know
// fails, as when the service is down.
type fakeLooker struct {
	mu    sync.Mutex
	info  map[string]iprep.Info
	calls int
}

func (f *fakeLooker) Lookup(_ context.Context, ip string) (iprep.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	info, ok := f.info[ip]
	if !ok {
		return iprep.Info{}, errors.New("lookup down")
	}
	return info, nil
}

func (f *fakeLooker) set(ip string, info iprep.Info) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.info[ip] = info
}

// paidFromIP is a capture of amount whose provider saw the buyer at ip.
func paidFromIP(amount int64, ip string) core.Snapshot {
	captures++
	return core.Snapshot{Status: state.AttemptCaptured, Amount: amount, Currency: "INR", Method: "card",
		ProviderPaymentID: fmt.Sprintf("pp_ip%d", captures), Risk: connector.PaymentRisk{IPAddress: ip}}
}

// payFrom opens a payment's checkout from the checkout IPs and pays it from paid.
func (e *env) payFrom(svc *core.Service, paid string, checkout ...string) core.ApplyResult {
	e.t.Helper()
	p := e.payment(500)
	for _, ip := range checkout {
		if err := svc.NoteCheckoutIP(ctx, p.ID, ip); err != nil {
			e.t.Fatal(err)
		}
	}
	a := e.attempt(p.ID)
	r, err := svc.ApplyAttemptSnapshot(ctx, a.ID, paidFromIP(500, paid))
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func TestVPNAtCheckoutGoesToReview(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.ips.set("203.0.113.7", iprep.Info{Country: "US", VPN: true})
		r := e.payFrom(e.svc, "203.0.113.7", "203.0.113.7")
		if r.Payment.Status != "needs_review" || !r.Payment.HasFlag(core.FlagVPN) {
			t.Fatalf("VPN buyer: %s %v", r.Payment.Status, r.Payment.Flags)
		}
		if got, err := e.svc.ApproveReview(ctx, audit.System, r.Payment.ID); err != nil || got.Status != "succeeded" {
			t.Fatalf("approve: %s %v", got.Status, err)
		}

		// A privacy relay is not masked.
		e.ips.set("203.0.113.8", iprep.Info{Country: "US", Relay: true})
		if r := e.payFrom(e.svc, "203.0.113.8", "203.0.113.8"); r.Payment.Status != "succeeded" {
			t.Fatalf("relay: %s %v", r.Payment.Status, r.Payment.Flags)
		}
	})
}

func TestPaidFromAnotherCountryGoesToReview(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.ips.set("198.51.100.1", iprep.Info{Country: "IN"})
		e.ips.set("198.51.100.2", iprep.Info{Country: "IN"})
		e.ips.set("203.0.113.24", iprep.Info{Country: "US"})

		r := e.payFrom(e.svc, "203.0.113.24", "198.51.100.1")
		if r.Payment.Status != "needs_review" || !r.Payment.HasFlag(core.FlagIPMismatch) {
			t.Fatalf("another country: %s %v", r.Payment.Status, r.Payment.Flags)
		}
		if !core.Approvable(r.Payment) {
			t.Fatal("an IP mismatch review cannot be approved")
		}
		// Another IP in the same country (Wi-Fi then mobile data) matches.
		if r := e.payFrom(e.svc, "198.51.100.2", "198.51.100.1"); r.Payment.Status != "succeeded" {
			t.Fatalf("same country: %s %v", r.Payment.Status, r.Payment.Flags)
		}
		// The same IP, no checkout IP, or a paying IP the lookup cannot place: no review.
		if r := e.payFrom(e.svc, "198.51.100.1", "198.51.100.1"); r.Payment.Status != "succeeded" {
			t.Fatalf("same IP: %s", r.Payment.Status)
		}
		if r := e.payFrom(e.svc, "203.0.113.24"); r.Payment.Status != "succeeded" {
			t.Fatalf("no checkout IP: %s", r.Payment.Status)
		}
		if r := e.payFrom(e.svc, "203.0.113.99", "198.51.100.1"); r.Payment.Status != "succeeded" {
			t.Fatalf("lookup failed: %s", r.Payment.Status)
		}
	})
}

func TestIPRulesCanBeTurnedOff(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		for _, key := range []string{"review_vpn", "review_ip_mismatch"} {
			if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, key, []byte(`false`)); err != nil {
				t.Fatal(err)
			}
		}
		e.ips.set("198.51.100.1", iprep.Info{Country: "IN", Proxy: true})
		e.ips.set("203.0.113.24", iprep.Info{Country: "US"})
		if r := e.payFrom(e.svc, "203.0.113.24", "198.51.100.1"); r.Payment.Status != "succeeded" {
			t.Fatalf("rules off: %s %v", r.Payment.Status, r.Payment.Flags)
		}
	})
}

func TestIPMismatchWithoutLookups(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		bare := core.NewService(core.Options{DB: e.db, Settings: e.set, Jobs: e.q})
		if r := e.payFrom(bare, "203.0.113.24", "198.51.100.1"); r.Payment.Status != "needs_review" || !r.Payment.HasFlag(core.FlagIPMismatch) {
			t.Fatalf("another IPv4: %s %v", r.Payment.Status, r.Payment.Flags)
		}
		// The pay page reached over IPv6 and the provider over IPv4 says nothing.
		if r := e.payFrom(bare, "203.0.113.24", "2001:db8::1"); r.Payment.Status != "succeeded" {
			t.Fatalf("other family: %s %v", r.Payment.Status, r.Payment.Flags)
		}
	})
}

func TestIPLookupsAreCached(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.ips.set("198.51.100.1", iprep.Info{Country: "IN"})
		e.payFrom(e.svc, "198.51.100.1", "198.51.100.1")
		e.payFrom(e.svc, "198.51.100.1", "198.51.100.1")
		if e.ips.calls != 1 {
			t.Fatalf("%d lookups of one IP", e.ips.calls)
		}
		ips, infos, err := e.svc.CheckoutIPs(ctx, e.payment(500).ID)
		if err != nil || len(ips) != 0 || len(infos) != 0 {
			t.Fatalf("a new payment: %v %v %v", ips, infos, err)
		}
		if info, ok := e.svc.IPInfo(ctx, "198.51.100.1"); !ok || info.Country != "IN" {
			t.Fatalf("cached info %+v %v", info, ok)
		}
	})
}

func TestCardTestingAfterSuccessSaysTheCardMayBeStolen(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		a := e.attempt(p.ID)
		e.apply(a.ID, paidWith(500, "7209", 30))
		for i, card := range []string{"1111", "2222", "3333", "4444", "5555"} {
			e.apply(a.ID, decline(i+1, card))
		}
		if got := e.get(p.ID); got.Status != "succeeded" || !got.HasFlag(core.FlagCardTesting) {
			t.Fatalf("payment %s %v", got.Status, got.Flags)
		}
		e.alerts.mu.Lock()
		defer e.alerts.mu.Unlock()
		i := slices.IndexFunc(e.alerts.alerts, func(a notify.Alert) bool { return a.Kind == "payment.card_testing" })
		if i < 0 || !strings.Contains(e.alerts.alerts[i].Message, "already succeeded") {
			t.Fatalf("alerts %+v", e.alerts.alerts)
		}
	})
}

func TestTheBuyerIPTheAppSentIsCompared(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.ips.set("170.62.103.12", iprep.Info{Country: "US", VPN: true})
		e.ips.set("198.51.100.1", iprep.Info{Country: "IN"})
		e.ips.set("203.0.113.24", iprep.Info{Country: "IQ"})
		pay := func(buyer, paid string) core.ApplyResult {
			res, err := e.svc.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 500, Currency: "INR", BuyerIP: buyer})
			if err != nil {
				t.Fatal(err)
			}
			return e.apply(e.attempt(res.Payment.ID).ID, paidFromIP(500, paid))
		}
		if r := pay("170.62.103.12", "73.30.247.196"); r.Payment.Status != "needs_review" || !r.Payment.HasFlag(core.FlagVPN) {
			t.Fatalf("VPN at the order page: %s %v", r.Payment.Status, r.Payment.Flags)
		}
		if r := pay("198.51.100.1", "203.0.113.24"); r.Payment.Status != "needs_review" || !r.Payment.HasFlag(core.FlagIPMismatch) {
			t.Fatalf("paid from another country: %s %v", r.Payment.Status, r.Payment.Flags)
		}
		r := pay("198.51.100.1", "198.51.100.1")
		ips, _, err := e.svc.CheckoutIPs(ctx, r.Payment.ID)
		if r.Payment.Status != "succeeded" || err != nil || len(ips) != 1 || ips[0].Source != store.CheckoutIPApp {
			t.Fatalf("same IP: %s %+v %v", r.Payment.Status, ips, err)
		}
	})
}

func TestAddressMismatch(t *testing.T) {
	for _, tc := range []struct {
		oc, cc string
		want   bool
	}{
		{"US", "IQ", true}, // another country
		{"us", "US ", false},
		{"", "IQ", false}, // a side unknown
		{"US", "", false},
	} {
		if got := core.AddressMismatch(tc.oc, tc.cc); got != tc.want {
			t.Errorf("AddressMismatch(%q,%q) = %v", tc.oc, tc.cc, got)
		}
	}
}

func TestCardBilledElsewhereGoesToReview(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		pay := func(cardCountry, cardPostal string) core.ApplyResult {
			res, err := e.svc.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 500, Currency: "INR",
				Customer: &core.CustomerInput{ExternalRef: "u-" + cardCountry + cardPostal, Email: "b@example.com",
					Billing: store.Billing{Country: "US", PostalCode: "10080", StateCode: "New York"}}})
			if err != nil {
				t.Fatal(err)
			}
			snap := paidFromIP(500, "")
			snap.Risk = connector.PaymentRisk{Country: cardCountry, PostalCode: cardPostal, City: "Racheltown"}
			return e.apply(e.attempt(res.Payment.ID).ID, snap)
		}
		r := pay("IQ", "")
		if r.Payment.Status != "needs_review" || !r.Payment.HasFlag(core.FlagAddressMismatch) || !core.Approvable(r.Payment) {
			t.Fatalf("billed in Iraq: %s %v", r.Payment.Status, r.Payment.Flags)
		}
		if r := pay("US", "10080"); r.Payment.Status != "succeeded" {
			t.Fatalf("same address: %s %v", r.Payment.Status, r.Payment.Flags)
		}
		if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "review_address_mismatch", []byte(`false`)); err != nil {
			t.Fatal(err)
		}
		if r := pay("IQ", "1"); r.Payment.Status != "succeeded" {
			t.Fatalf("rule off: %s %v", r.Payment.Status, r.Payment.Flags)
		}
	})
}
