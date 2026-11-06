package dashboard

import (
	"testing"
	"time"

	"github.com/velixio/paymux/internal/outbound"
	"github.com/velixio/paymux/internal/store"
)

var tlStart = time.Date(2026, 11, 6, 16, 0, 0, 0, time.UTC)

func tlAt(minute int) time.Time { return tlStart.Add(time.Duration(minute) * time.Minute) }

func tlData() timelineData {
	return timelineData{
		Payment:  store.Payment{ID: "pay_1", Status: "created", CreatedAt: tlAt(0)},
		Provider: map[string]string{"acc_1": "Main (stripe)"},
		FmtTime:  func(t time.Time) string { return t.Format("15:04") },
	}
}

func failedTry(id string, minute int, code, message string) store.ProviderPaymentAttempt {
	return store.ProviderPaymentAttempt{ProviderAccountID: "acc_1", ProviderPaymentID: id, AttemptID: "att_1", Status: "failed",
		FailureCode: code, FailureMessage: message, CreatedAt: tlAt(minute), UpdatedAt: tlAt(minute)}
}

func kinds(items []timelineItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Kind)
	}
	return out
}

func TestGlanceMissingData(t *testing.T) {
	const riskNoScore = `{"cvv_check":"match"}`
	cases := []struct {
		name                 string
		tries                []store.ProviderPaymentAttempt
		riskText, riskTone   string
		lastFailure, triesTx string
	}{
		{name: "nothing yet", riskText: "Not provided", riskTone: "none", triesTx: "None yet"},
		{name: "failed without risk", tries: []store.ProviderPaymentAttempt{failedTry("p1", 1, "failed", "")},
			riskText: "Not provided", riskTone: "none", lastFailure: "No reason given", triesTx: "1 failed"},
		{name: "failed with risk and no score", tries: func() []store.ProviderPaymentAttempt {
			try := failedTry("p1", 1, "card_declined", "Card declined")
			try.RiskDetails = riskNoScore
			return []store.ProviderPaymentAttempt{try}
		}(), riskText: "Not scored", riskTone: "none", lastFailure: "Card declined", triesTx: "1 failed"},
		{name: "score not in yet", tries: []store.ProviderPaymentAttempt{{ProviderPaymentID: "p1", AttemptID: "att_1", Status: "pending", RiskDetails: riskNoScore}},
			riskText: "Pending", riskTone: "none", triesTx: "1 in progress"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := tlData()
			in.Tries = c.tries
			g := buildGlance(in)
			if g.RiskText != c.riskText || g.RiskTone != c.riskTone {
				t.Errorf("risk = %q/%q, want %q/%q", g.RiskText, g.RiskTone, c.riskText, c.riskTone)
			}
			if g.LastFailure != c.lastFailure {
				t.Errorf("last failure = %q, want %q", g.LastFailure, c.lastFailure)
			}
			if g.Tries != c.triesTx {
				t.Errorf("tries = %q, want %q", g.Tries, c.triesTx)
			}
		})
	}
}

func TestLastFailureNamesFailedThreeDSecure(t *testing.T) {
	in := tlData()
	try := failedTry("p1", 1, "failed", "")
	try.ThreeDS = "failed"
	in.Tries = []store.ProviderPaymentAttempt{try}
	if g := buildGlance(in); g.LastFailure != "3D Secure failed" {
		t.Fatalf("last failure = %q", g.LastFailure)
	}
}

func TestLastFailureGoesGreyAfterSuccess(t *testing.T) {
	in := tlData()
	paid := failedTry("p2", 2, "", "")
	paid.Status = "captured"
	in.Tries = []store.ProviderPaymentAttempt{failedTry("p1", 1, "card_declined", "Card declined"), paid}
	g := buildGlance(in)
	if g.LastFailure != "Card declined" || g.LastFailureTone != "none" {
		t.Fatalf("last failure = %q/%q", g.LastFailure, g.LastFailureTone)
	}
	if g.Tries != "1 failed, 1 paid" || g.TriesTone != "pass" {
		t.Fatalf("tries = %q/%q", g.Tries, g.TriesTone)
	}
}

func TestTimelineOrderAndKinds(t *testing.T) {
	in := tlData()
	in.Attempts = []store.Attempt{{ID: "att_1", ProviderAccountID: "acc_1", Status: "failed", CreatedAt: tlAt(1)}}
	in.Tries = []store.ProviderPaymentAttempt{failedTry("p1", 2, "card_declined", "Card declined")}
	in.Refunds = []store.Refund{{ID: "rf_1", Amount: 100, Currency: "USD", Status: "pending", CreatedAt: tlAt(5)}}
	in.Ledger = []store.LedgerEntry{{Kind: "fee", Amount: 5, Currency: "USD", OccurredAt: tlAt(3)}}
	in.Events = []store.EventRow{
		{ID: "ev_1", Type: "payment.failed", CreatedAt: tlAt(4)},
		{ID: "ev_2", Type: "payment.updated", CreatedAt: tlAt(6)},
	}
	in.Targets = map[string][]outbound.ResendTarget{"ev_1": {{EndpointID: "ep_1", URL: "https://hooks.example.com/x", Status: "delivered", LastStatus: 200}}}
	items := buildTimeline(in)
	want := []string{"created", "checkout", "try", "ledger", "webhook", "refund", "event"}
	got := kinds(items)
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
	for _, it := range items {
		wantSystem := it.Kind == "ledger" || it.Kind == "event"
		if it.System != wantSystem {
			t.Errorf("%s: System = %v", it.Kind, it.System)
		}
	}
}

func TestFailedAttemptWithoutTryStillShows(t *testing.T) {
	in := tlData()
	in.Attempts = []store.Attempt{{ID: "att_1", ProviderAccountID: "acc_1", Status: "failed", FailureCode: "checkout_error",
		FailureMessage: "Provider rejected the amount", CreatedAt: tlAt(1), UpdatedAt: tlAt(2)}}
	var try *timelineItem
	items := buildTimeline(in)
	for i := range items {
		if items[i].Kind == "try" {
			try = &items[i]
		}
	}
	if try == nil || try.Title != "Provider rejected the amount" || try.Tone != "fail" || try.Try.FailureCode != "checkout_error" {
		t.Fatalf("items = %+v", items)
	}
	if g := buildGlance(in); g.LastFailure != "Provider rejected the amount" || g.Tries != "1 failed" {
		t.Fatalf("glance = %+v", g)
	}
}

// A payment from before tries were kept must not read as if nobody paid.
func TestPaidAttemptWithoutTryStillShows(t *testing.T) {
	in := tlData()
	in.Attempts = []store.Attempt{{ID: "att_1", ProviderAccountID: "acc_1", Status: "captured", CreatedAt: tlAt(1)}}
	if noTries(in) {
		t.Fatal("noTries = true")
	}
	items := buildTimeline(in)
	if last := items[len(items)-1]; last.Kind != "try" || last.Title != "Paid" {
		t.Fatalf("items = %+v", items)
	}
	if g := buildGlance(in); g.Tries != "1 paid" {
		t.Fatalf("glance = %+v", g)
	}
}

// Rows stored before statuses were normalized keep the provider's own text.
func TestRawProviderStatusesReadAsTheirMeaning(t *testing.T) {
	in := tlData()
	in.Payment.Status = "succeeded"
	in.Tries = []store.ProviderPaymentAttempt{
		{ProviderPaymentID: "p1", AttemptID: "att_1", Status: "complete/paid", CreatedAt: tlAt(2)},
		{ProviderPaymentID: "p2", AttemptID: "att_1", Status: "expired/unpaid", CreatedAt: tlAt(1), RiskDetails: `{"cvv_check":"match"}`},
	}
	items := buildTimeline(in)
	if items[1].Title != "Payment failed" || items[2].Title != "Paid" || items[2].Tone != "pass" {
		t.Fatalf("items = %+v", items)
	}
	if g := buildGlance(in); g.Tries != "1 failed, 1 paid" || g.TriesTone != "pass" || g.RiskText != "Not scored" {
		t.Fatalf("glance = %+v", g)
	}
}

// An unfinished try of a payment that has moved on will not finish.
func TestOpenTryOfFinishedPaymentReadsExpired(t *testing.T) {
	in := tlData()
	in.Payment.Status = "expired"
	in.Tries = []store.ProviderPaymentAttempt{{ProviderPaymentID: "p1", AttemptID: "att_1", Status: "pending", CreatedAt: tlAt(1)}}
	item := buildTimeline(in)[1]
	if item.Title != "Expired" || item.Try.Status != "expired" || item.Tone != "none" {
		t.Fatalf("item = %+v, try = %+v", item, item.Try)
	}
	if g := buildGlance(in); g.Tries != "1 expired" {
		t.Fatalf("glance = %+v", g)
	}
}

func TestNoTriesHasOnlyCreated(t *testing.T) {
	in := tlData()
	items := buildTimeline(in)
	if len(items) != 1 || items[0].Kind != "created" {
		t.Fatalf("items = %+v", items)
	}
	if !noTries(in) {
		t.Fatal("noTries = false")
	}
}

func TestSayCheckoutStarted(t *testing.T) {
	in := tlData()
	in.Attempts = []store.Attempt{{ID: "att_1", ProviderAccountID: "acc_1", Status: "created", CreatedAt: tlAt(1)}}
	items := buildTimeline(in)
	if len(items) != 2 || items[1].Title != "Checkout started" || items[1].Meta != "Main (stripe)" {
		t.Fatalf("items = %+v", items)
	}
	if !noTries(in) {
		t.Fatal("a checkout alone is not a try")
	}
}

func TestRenewalIsNotCalledCheckout(t *testing.T) {
	in := tlData()
	in.Payment.SubscriptionID = "sub_1"
	in.Attempts = []store.Attempt{{ID: "att_1", ProviderAccountID: "acc_1", Status: "created", CreatedAt: tlAt(1)}}
	if items := buildTimeline(in); items[1].Title != "Charge started" {
		t.Fatalf("title = %q", items[1].Title)
	}
}

func TestThreeDSNotVerifiedIsGrey(t *testing.T) {
	for _, c := range []struct{ value, label, tone string }{
		{"passed", "3D Secure", "pass"},
		{"failed", "3D Secure", "fail"},
		{"not_verified", "3D Secure not verified", "none"},
		{"", "", ""},
	} {
		try := failedTry("p1", 1, "card_declined", "Declined")
		try.ThreeDS = c.value
		in := tlData()
		in.Tries = []store.ProviderPaymentAttempt{try}
		row := buildTimeline(in)[1].Try
		if row.ThreeDS != c.label || row.ThreeDSTone != c.tone {
			t.Errorf("%q: %q/%q, want %q/%q", c.value, row.ThreeDS, row.ThreeDSTone, c.label, c.tone)
		}
		if row.Declined != (c.value != "failed") {
			t.Errorf("%q: declined = %v", c.value, row.Declined)
		}
	}
}

func TestTryShowsWhatWeHave(t *testing.T) {
	cases := []struct{ name, method, details, card, bank, holder string }{
		{"full card", "card", `{"card":{"network":"visa","last4":"4242","issuer":"Sample Bank","name":"S CUSTOMER"}}`, "Visa •••• 4242", "Sample Bank", "S CUSTOMER"},
		{"bare card", "card", `{}`, "Card", "", ""},
		{"unknown method is not called a card", "", ``, "", "", ""},
		{"netbanking", "netbanking", `{"bank":"HDFC"}`, "Netbanking", "HDFC", ""},
		{"unreadable", "upi", `{not json`, "UPI", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			try := failedTry("p1", 1, "x", "y")
			try.Method, try.MethodDetails = c.method, c.details
			in := tlData()
			in.Tries = []store.ProviderPaymentAttempt{try}
			row := buildTimeline(in)[1].Try
			if row.Card != c.card || row.Bank != c.bank || row.Name != c.holder {
				t.Fatalf("got %q %q %q", row.Card, row.Bank, row.Name)
			}
		})
	}
}

func TestDeclinedOnlyWithAStatedReason(t *testing.T) {
	for _, c := range []struct {
		code, message string
		want          bool
	}{{"card_declined", "", true}, {"", "Your card was declined", true}, {"failed", "", false}, {"", "", false}, {"checkout_error", "Provider rejected the amount", false}} {
		in := tlData()
		in.Tries = []store.ProviderPaymentAttempt{failedTry("p1", 1, c.code, c.message)}
		if got := buildTimeline(in)[1].Try.Declined; got != c.want {
			t.Errorf("%q %q: declined = %v", c.code, c.message, got)
		}
	}
}

func TestFailureCodeFailedIsHidden(t *testing.T) {
	in := tlData()
	in.Tries = []store.ProviderPaymentAttempt{failedTry("p1", 1, "failed", "")}
	item := buildTimeline(in)[1]
	if item.Title != "Payment failed" || item.Try.FailureCode != "" {
		t.Fatalf("item = %+v, try = %+v", item, item.Try)
	}
}

func webhookGroupOf(t *testing.T, in timelineData) *webhookGroup {
	t.Helper()
	for _, it := range buildTimeline(in) {
		if it.Kind == "webhook" {
			return it.Webhook
		}
	}
	t.Fatal("no webhook item")
	return nil
}

func TestRetryOnlyOnFailedTargets(t *testing.T) {
	in := tlData()
	in.Events = []store.EventRow{{ID: "ev_1", Type: "payment.failed", CreatedAt: tlAt(1)}}
	in.Targets = map[string][]outbound.ResendTarget{"ev_1": {
		{EndpointID: "ok", URL: "https://a.example.com/hook?secret=1", DeliveryID: "d1", Status: "delivered", LastStatus: 200},
		{EndpointID: "bad", URL: "https://b.example.com", DeliveryID: "d2", Status: "failed", LastStatus: 500},
		{EndpointID: "wait", URL: "https://c.example.com", DeliveryID: "d3", Status: "pending", LastStatus: 503, NextAttemptAt: tlAt(30)},
		{EndpointID: "first", URL: "https://d.example.com", DeliveryID: "d4", Status: "pending", NextAttemptAt: tlAt(1)},
		{EndpointID: "new", URL: "https://e.example.com", New: true},
		{EndpointID: "busy", URL: "https://f.example.com", DeliveryID: "d5", Status: "pending", LastStatus: 500, InFlight: true},
	}}
	want := []struct {
		endpoint, result, tone, next string
		canRetry, isNew              bool
	}{
		{"a.example.com/hook", "200", "pass", "", false, false},
		{"b.example.com", "500", "fail", "", true, false},
		{"c.example.com", "503", "fail", "16:30", true, false},
		{"d.example.com", "Queued", "none", "", false, false},
		{"e.example.com", "Not sent", "none", "", false, true},
		{"f.example.com", "Sending", "none", "", false, false},
	}
	got := webhookGroupOf(t, in).Targets
	if len(got) != len(want) {
		t.Fatalf("targets = %+v", got)
	}
	for i, w := range want {
		g := got[i]
		if g.Endpoint != w.endpoint || g.Result != w.result || g.Tone != w.tone || g.NextTry != w.next || g.CanRetry != w.canRetry || g.New != w.isNew {
			t.Errorf("%d: %+v, want %+v", i, g, w)
		}
	}
}

func TestFailureWithoutStatusSaysFailed(t *testing.T) {
	in := tlData()
	in.Events = []store.EventRow{{ID: "ev_1", Type: "x", CreatedAt: tlAt(1)}}
	in.Targets = map[string][]outbound.ResendTarget{"ev_1": {{EndpointID: "e", URL: "https://a.example.com", DeliveryID: "d", Status: "failed", LastError: "timeout"}}}
	if got := webhookGroupOf(t, in).Targets[0]; got.Result != "Failed" || got.Tone != "fail" || !got.CanRetry {
		t.Fatalf("target = %+v", got)
	}
}

func TestSendCountMatchesTargets(t *testing.T) {
	in := tlData()
	in.Events = []store.EventRow{{ID: "ev_1", Type: "x", CreatedAt: tlAt(1)}}
	in.Targets = map[string][]outbound.ResendTarget{"ev_1": {
		{EndpointID: "a", URL: "https://a.example.com", DeliveryID: "d1", Status: "delivered"},
		{EndpointID: "b", URL: "https://b.example.com", New: true},
		{EndpointID: "c", URL: "https://c.example.com", DeliveryID: "d3", Status: "pending", InFlight: true},
	}}
	if n := webhookGroupOf(t, in).SendCount; n != 2 {
		t.Fatalf("SendCount = %d", n)
	}
}

// A delivery to an endpoint since disabled stays in the history but cannot be sent.
func TestDisabledEndpointHistoryKeptButNotCounted(t *testing.T) {
	in := tlData()
	in.Events = []store.EventRow{{ID: "ev_1", Type: "x", CreatedAt: tlAt(1)}}
	in.Deliveries = []store.DeliveryListRow{{DeliveryID: "d1", EventID: "ev_1", EndpointID: "gone", EndpointURL: "https://gone.example.com", Status: "failed", LastStatus: 500}}
	group := webhookGroupOf(t, in)
	if len(group.Targets) != 1 || group.Targets[0].CanRetry || group.Targets[0].Result != "500" || group.SendCount != 0 {
		t.Fatalf("group = %+v", group)
	}
}

func TestGlanceWebhooks(t *testing.T) {
	cases := []struct {
		name       string
		targets    []outbound.ResendTarget
		text, tone string
	}{
		{"no endpoints", nil, "None", "none"},
		{"new only", []outbound.ResendTarget{{New: true}}, "None", "none"},
		{"delivered", []outbound.ResendTarget{{Status: "delivered"}, {New: true}}, "All delivered", "pass"},
		{"failing", []outbound.ResendTarget{{Status: "delivered"}, {Status: "failed"}, {Status: "pending", LastStatus: 500}}, "2 failing", "fail"},
		{"queued", []outbound.ResendTarget{{Status: "pending"}}, "Sending", "none"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := tlData()
			in.Events = []store.EventRow{{ID: "ev_1", Type: "x"}}
			in.Targets = map[string][]outbound.ResendTarget{"ev_1": c.targets}
			g := buildGlance(in)
			if g.Webhooks != c.text || g.WebhooksTone != c.tone {
				t.Fatalf("webhooks = %q/%q", g.Webhooks, g.WebhooksTone)
			}
		})
	}
}

func TestRiskUsesLatestTryWithRisk(t *testing.T) {
	in := tlData()
	older := failedTry("p1", 1, "x", "y")
	older.RiskDetails = `{"score":62,"address_check":"no_match"}`
	in.Tries = []store.ProviderPaymentAttempt{older, failedTry("p2", 2, "x", "y")}
	panel := buildRiskPanel(in)
	if panel == nil || len(panel.Checks) != 1 || panel.Checks[0].Tone != "fail" {
		t.Fatalf("panel = %+v", panel)
	}
	if panel.Score != "" {
		t.Fatalf("the score belongs in the tile only, panel has %q", panel.Score)
	}
	if g := buildGlance(in); g.RiskText != "62 Medium" || g.RiskTone != "medium" {
		t.Fatalf("glance = %+v", g)
	}
}

// Payments from before tries kept risk still show the attempt's review.
func TestRiskFallsBackToAttempt(t *testing.T) {
	in := tlData()
	in.Payment.Status = "succeeded"
	in.Attempts = []store.Attempt{{ID: "att_1", Status: "captured", RiskDetails: `{"score":10,"cvv_check":"match"}`, CreatedAt: tlAt(1)}}
	in.Tries = []store.ProviderPaymentAttempt{{ProviderPaymentID: "p1", AttemptID: "att_1", Status: "captured", CreatedAt: tlAt(1)}}
	if g := buildGlance(in); g.RiskText != "10 Low" {
		t.Fatalf("glance = %+v", g)
	}
	if p := buildRiskPanel(in); p == nil || len(p.Checks) != 1 {
		t.Fatalf("panel = %+v", p)
	}
}

func TestRiskPanelShowsFailedThreeDSecure(t *testing.T) {
	in := tlData()
	try := failedTry("p1", 1, "x", "y")
	try.RiskDetails, try.ThreeDS = `{"cvv_check":"match","three_d_secure":false}`, "failed"
	in.Tries = []store.ProviderPaymentAttempt{try}
	panel := buildRiskPanel(in)
	if panel == nil || len(panel.Checks) != 2 {
		t.Fatalf("panel = %+v", panel)
	}
	if c := panel.Checks[1]; c.Label != "3D Secure" || c.Text != "Failed" || c.Tone != "fail" {
		t.Fatalf("check = %+v", c)
	}
}

func TestScoreOnlyRiskHasNoPanel(t *testing.T) {
	in := tlData()
	try := failedTry("p1", 1, "x", "y")
	try.RiskDetails = `{"score":12}`
	in.Tries = []store.ProviderPaymentAttempt{try}
	if p := buildRiskPanel(in); p != nil {
		t.Fatalf("panel = %+v", p)
	}
	if g := buildGlance(in); g.RiskText != "12 Low" {
		t.Fatalf("glance = %+v", g)
	}
}

// Errors stored before delivery errors dropped the URL still name the full one.
func TestDeliveryErrorHidesEndpointQuery(t *testing.T) {
	row := tlData().targetRow(outbound.ResendTarget{DeliveryID: "whd_1", URL: "https://x.example/hook?token=s3cret", Status: "failed",
		LastError: `Post "https://x.example/hook?token=s3cret": dial tcp: connection refused`})
	if want := `Post "https://x.example/hook": dial tcp: connection refused`; row.Error != want {
		t.Errorf("error = %q, want %q", row.Error, want)
	}
}
