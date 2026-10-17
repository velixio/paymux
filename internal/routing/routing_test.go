package routing

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func accts() []Account {
	return []Account{
		{ID: "pa_rzp", Connector: "razorpay", Currencies: []string{"INR", "USD"}, Purposes: []string{PurposeOneTime, PurposeSubscription}},
		{ID: "pa_zoho", Connector: "zoho", Currencies: []string{"INR"}, Purposes: []string{PurposeOneTime}},
		{ID: "pa_payu", Connector: "payu", Currencies: []string{"INR"}, Purposes: []string{PurposeOneTime, PurposeSubscription}},
		{ID: "pa_whop", Connector: "whop", Currencies: []string{"USD", "BDT"}, Purposes: []string{PurposeOneTime, PurposeSubscription}},
	}
}

func seeded() Config {
	return Config{
		Org: []Rule{
			{Name: "inr", When: When{Currency: []string{"INR"}}, Action: Route("razorpay", "zoho", "payu")},
			{Name: "bdt", When: When{Currency: []string{"BDT"}}, Action: Route("razorpay", "whop")},
		},
		CatchAll: Rule{Name: "catch-all", Action: Route("razorpay", "whop")},
	}
}

func ids(r Result) []string { return r.Accounts }

func TestSeededDefaults(t *testing.T) {
	cases := []struct {
		req  Request
		want []string
		rule string
	}{
		{Request{Currency: "INR", Amount: 49900, Purpose: PurposeOneTime}, []string{"pa_rzp", "pa_zoho", "pa_payu"}, "inr"},
		{Request{Currency: "INR", Amount: 49900, Purpose: PurposeSubscription}, []string{"pa_rzp", "pa_payu"}, "inr"},
		{Request{Currency: "BDT", Amount: 100000, Purpose: PurposeOneTime}, []string{"pa_whop"}, "bdt"},
		{Request{Currency: "USD", Amount: 1000, Purpose: PurposeOneTime}, []string{"pa_rzp", "pa_whop"}, "catch-all"},
	}
	for _, c := range cases {
		r, err := Choose(seeded(), c.req, accts(), nil)
		if err != nil {
			t.Fatalf("%+v: %v", c.req, err)
		}
		if !reflect.DeepEqual(ids(r), c.want) || r.Rule != c.rule {
			t.Errorf("%s/%s: got %v via %q, want %v via %q", c.req.Currency, c.req.Purpose, ids(r), r.Rule, c.want, c.rule)
		}
	}
}

func TestAppRulesWin(t *testing.T) {
	cfg := seeded()
	cfg.App = []Rule{{Name: "app-inr-zoho", When: When{Currency: []string{"INR"}}, Action: Route("zoho")}}
	r, err := Choose(cfg, Request{Currency: "INR", Amount: 100, Purpose: PurposeOneTime}, accts(), nil)
	if err != nil || !reflect.DeepEqual(ids(r), []string{"pa_zoho"}) || r.Rule != "app-inr-zoho" {
		t.Fatalf("%v %v %v", ids(r), r.Rule, err)
	}
}

func TestFallThroughWhenTargetsFiltered(t *testing.T) {
	cfg := seeded()
	cfg.App = []Rule{{Name: "zoho-only", When: When{Currency: []string{"INR"}}, Action: Route("zoho")}}
	// Zoho has no subscription purpose, so the app rule yields nothing and org INR rule applies.
	r, err := Choose(cfg, Request{Currency: "INR", Amount: 100, Purpose: PurposeSubscription}, accts(), nil)
	if err != nil || r.Rule != "inr" || !reflect.DeepEqual(ids(r), []string{"pa_rzp", "pa_payu"}) {
		t.Fatalf("%v %v %v", ids(r), r.Rule, err)
	}
}

func TestRejectAction(t *testing.T) {
	cfg := seeded()
	cfg.CatchAll = Rule{Name: "reject", Action: Reject("We don't accept this currency yet")}
	_, err := Choose(cfg, Request{Currency: "JPY", Amount: 100, Purpose: PurposeOneTime}, accts(), nil)
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Message != "We don't accept this currency yet" {
		t.Fatalf("got %v", err)
	}
}

func TestNoRoute(t *testing.T) {
	_, err := Choose(seeded(), Request{Currency: "JPY", Amount: 100, Purpose: PurposeOneTime}, accts(), nil)
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("got %v", err)
	}
}

func TestConditions(t *testing.T) {
	min, max := int64(1000), int64(5000)
	rule := Rule{Name: "r", When: When{Currency: []string{"INR"}, Country: []string{"IN"}, AmountMin: &min, AmountMax: &max,
		Purpose: []string{PurposeOneTime}, Metadata: map[string]string{"plan": "pro"}}, Action: Route("payu")}
	cfg := Config{Org: []Rule{rule}}
	base := Request{Currency: "INR", Country: "IN", Amount: 2000, Purpose: PurposeOneTime, Metadata: map[string]string{"plan": "pro"}}
	if r, err := Choose(cfg, base, accts(), nil); err != nil || r.Rule != "r" {
		t.Fatalf("base: %v %v", r, err)
	}
	for name, mut := range map[string]func(*Request){
		"country": func(r *Request) { r.Country = "US" },
		"low":     func(r *Request) { r.Amount = 999 },
		"high":    func(r *Request) { r.Amount = 5001 },
		"purpose": func(r *Request) { r.Purpose = PurposeSubscription },
		"meta":    func(r *Request) { r.Metadata = map[string]string{"plan": "free"} },
	} {
		req := base
		mut(&req)
		if _, err := Choose(cfg, req, accts(), nil); !errors.Is(err, ErrNoRoute) {
			t.Errorf("%s: expected no match, got %v", name, err)
		}
	}
}

func TestAllowedConnectorsAndAccountTargets(t *testing.T) {
	cfg := Config{Org: []Rule{{Name: "r", Action: Route("razorpay", "payu")}}}
	r, _ := Choose(cfg, Request{Currency: "INR", Amount: 1, Purpose: PurposeOneTime, AllowedConnectors: []string{"payu"}}, accts(), nil)
	if !reflect.DeepEqual(ids(r), []string{"pa_payu"}) {
		t.Fatalf("allowed connectors: %v", ids(r))
	}
	cfg = Config{Org: []Rule{{Name: "pin", Action: Rule{}.Action.withAccounts("pa_zoho")}}}
	r, _ = Choose(cfg, Request{Currency: "INR", Amount: 1, Purpose: PurposeOneTime}, accts(), nil)
	if !reflect.DeepEqual(ids(r), []string{"pa_zoho"}) {
		t.Fatalf("pinned account: %v", ids(r))
	}
}

func TestDisabledAndUntickedCurrency(t *testing.T) {
	a := accts()
	a[0].Currencies = []string{"USD"} // admin unticked INR on Razorpay
	a[1].Disabled = true
	r, _ := Choose(seeded(), Request{Currency: "INR", Amount: 1, Purpose: PurposeOneTime}, a, nil)
	if !reflect.DeepEqual(ids(r), []string{"pa_payu"}) {
		t.Fatalf("got %v", ids(r))
	}
}

func TestUnhealthySortsLast(t *testing.T) {
	open := func(id string) bool { return id == "pa_rzp" }
	r, _ := Choose(seeded(), Request{Currency: "INR", Amount: 1, Purpose: PurposeOneTime}, accts(), open)
	if !reflect.DeepEqual(ids(r), []string{"pa_zoho", "pa_payu", "pa_rzp"}) {
		t.Fatalf("got %v", ids(r))
	}
}

func TestBreaker(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBreaker(func() time.Time { return now })
	for i := 0; i < 4; i++ {
		b.Failure("pa_rzp")
	}
	if b.Open("pa_rzp") {
		t.Fatal("open after 4 failures")
	}
	b.Failure("pa_rzp")
	if !b.Open("pa_rzp") {
		t.Fatal("closed after 5 consecutive failures")
	}
	now = now.Add(2*time.Minute + time.Second)
	if b.Open("pa_rzp") {
		t.Fatal("still open after cooldown")
	}
	b.Failure("pa_x")
	b.Success("pa_x")
	for i := 0; i < 4; i++ {
		b.Failure("pa_x")
	}
	if b.Open("pa_x") {
		t.Fatal("success must reset the streak")
	}
	// Failures spread beyond 5 minutes do not trip the breaker.
	for i := 0; i < 5; i++ {
		b.Failure("pa_y")
		now = now.Add(2 * time.Minute)
	}
	if b.Open("pa_y") {
		t.Fatal("tripped on failures outside the 5-minute window")
	}
}
