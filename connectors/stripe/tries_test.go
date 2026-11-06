package stripe

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

func parsedFailure(t *testing.T) connector.Notification {
	t.Helper()
	c := newConn(t, newFake())
	ns, err := c.ParseWebhook(bg, hook(fixture(t, "event_payment_intent_failed"), testSecret, nowUnix))
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 {
		t.Fatalf("notifications = %+v", ns)
	}
	return ns[0]
}

func TestParsePaymentIntentFailed(t *testing.T) {
	n := parsedFailure(t)
	if n.Kind != connector.NotifyPayment || n.Payment == nil {
		t.Fatalf("notification = %+v", n)
	}
	p := n.Payment
	if p.ProviderRef != "pay_ref_01" || p.ProviderPaymentID != "pi_3A1" || p.TryID != "ch_failed_1" {
		t.Errorf("ids = %+v", p)
	}
	if p.ErrorCode != "insufficient_funds" || p.ErrorMessage != "Your card has insufficient funds." {
		t.Errorf("error = %s %s", p.ErrorCode, p.ErrorMessage)
	}
	if p.Status != connector.PaymentRequiresAction {
		t.Errorf("an open session must not fail the attempt: %s", p.Status)
	}
	if p.MethodDetails.Card == nil || p.MethodDetails.Card.Last4 != "4242" || p.MethodDetails.Card.Network != "visa" {
		t.Errorf("card = %+v", p.MethodDetails.Card)
	}
}

func TestPaymentIntentFailedWithoutRefIsIgnored(t *testing.T) {
	var ev map[string]any
	if err := json.Unmarshal(fixture(t, "event_payment_intent_failed"), &ev); err != nil {
		t.Fatal(err)
	}
	ev["data"].(map[string]any)["object"].(map[string]any)["metadata"] = map[string]any{}
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := newConn(t, newFake()).ParseWebhook(bg, hook(body, testSecret, nowUnix))
	if err != nil || len(ns) != 0 {
		t.Fatalf("ns = %+v err = %v", ns, err)
	}
}

func TestRepeatedFailureSameTryID(t *testing.T) {
	first, second := parsedFailure(t), parsedFailure(t)
	if first.Payment.TryID != second.Payment.TryID {
		t.Fatalf("%s != %s", first.Payment.TryID, second.Payment.TryID)
	}
	f := newFake()
	f.reply("GET /v1/checkout/sessions/cs_test_1", 200, sessionJSON("cs_test_1", "pay_ref_01", "open", "unpaid", intentJSON("pi_3A1", "requires_payment_method", "pay_ref_01", nil)))
	f.on("GET /v1/charges", func(w http.ResponseWriter, _ call) { _, _ = w.Write(fixture(t, "charges_of_intent")) })
	got, err := newConn(t, f).FetchPaymentAttempts(bg, connector.PaymentRef{ProviderOrderID: "cs_test_1", ProviderRef: "pay_ref_01"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(got, func(s connector.PaymentSnapshot) bool { return s.TryID == first.Payment.TryID }) {
		t.Fatalf("the listing has another key for this try: %+v", got)
	}
}

func TestAuthFailureWithoutChargeTryID(t *testing.T) {
	pi := paymentIntent{ID: "pi_1", Status: "requires_payment_method", Metadata: map[string]string{metaRef: "r"},
		LastPayError: &paymentError{Code: "payment_intent_authentication_failure", Message: "Could not authenticate.",
			PaymentMethod: &errorPaymentMethod{ID: "pm_9"}}}
	snap := pi.snapshot()
	if snap.TryID != "pi_1:pm_9:payment_intent_authentication_failure" {
		t.Errorf("try id = %s", snap.TryID)
	}
	if snap.ThreeDS != connector.ThreeDSFailed {
		t.Errorf("3D Secure = %q", snap.ThreeDS)
	}
}

func TestFetchPaymentAttemptsListsCharges(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/checkout/sessions/cs_test_1", 200, sessionJSON("cs_test_1", "pay_ref_01", "open", "unpaid", intentJSON("pi_3A1", "requires_payment_method", "pay_ref_01", nil)))
	f.on("GET /v1/charges", func(w http.ResponseWriter, _ call) { _, _ = w.Write(fixture(t, "charges_of_intent")) })
	got, err := newConn(t, f).FetchPaymentAttempts(bg, connector.PaymentRef{ProviderOrderID: "cs_test_1", ProviderRef: "pay_ref_01"})
	if err != nil {
		t.Fatal(err)
	}
	if calls := f.to("GET /v1/charges"); len(calls) != 1 || calls[0].Query.Get("payment_intent") != "pi_3A1" {
		t.Fatalf("calls = %+v", calls)
	}
	if len(got) != 2 {
		t.Fatalf("got = %+v", got)
	}
	failed := got[slices.IndexFunc(got, func(s connector.PaymentSnapshot) bool { return s.TryID == "ch_failed_1" })]
	if failed.Status != connector.PaymentFailed || failed.ErrorCode != "insufficient_funds" || failed.ProviderPaymentID != "pi_3A1" ||
		failed.ProviderOrderID != "cs_test_1" || failed.ProviderRef != "pay_ref_01" || failed.Amount != 49900 || failed.Currency != "INR" || failed.CreatedAt != 1760000000000 {
		t.Errorf("failed = %+v", failed)
	}
}

// A list filter is not proof: a charge naming another intent isn't this payment's.
func TestFetchPaymentAttemptsSkipsOtherIntents(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/checkout/sessions/cs_test_1", 200, sessionJSON("cs_test_1", "pay_ref_01", "open", "unpaid", intentJSON("pi_3A1", "requires_payment_method", "pay_ref_01", nil)))
	f.on("GET /v1/charges", func(w http.ResponseWriter, _ call) {
		_, _ = w.Write([]byte(`{"data":[{"id":"ch_ours","payment_intent":"pi_3A1","status":"failed"},{"id":"ch_theirs","payment_intent":"pi_other","status":"failed"}]}`))
	})
	got, err := newConn(t, f).FetchPaymentAttempts(bg, connector.PaymentRef{ProviderOrderID: "cs_test_1", ProviderRef: "pay_ref_01"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TryID != "ch_ours" {
		t.Fatalf("got = %+v", got)
	}
}

func TestChargeMapsChecks(t *testing.T) {
	var l list[charge]
	if err := json.Unmarshal(fixture(t, "charges_of_intent"), &l); err != nil {
		t.Fatal(err)
	}
	ok := l.Data[0]
	risk, threeDS := chargeRisk(&ok), chargeThreeDS(&ok)
	if risk.AddressCheck != connector.CheckNoMatch || risk.PostalCheck != connector.CheckMatch || risk.CVVCheck != "" {
		t.Errorf("checks = %+v", risk)
	}
	if risk.Score == nil || *risk.Score != 18 {
		t.Errorf("score = %v", risk.Score)
	}
	if threeDS != connector.ThreeDSPassed {
		t.Errorf("3D Secure = %q", threeDS)
	}
	if details := methodDetailsOf(&ok); details.Card == nil || details.Card.Issuer != "Sample Bank" {
		t.Errorf("details = %+v", details)
	}
	if got := chargeThreeDS(&l.Data[1]); got != "" {
		t.Errorf("no 3D Secure reported, got %q", got)
	}
	for result, want := range map[string]string{"authenticated": connector.ThreeDSPassed, "attempt_acknowledged": connector.ThreeDSPassed,
		"failed": connector.ThreeDSFailed, "exempted": connector.ThreeDSNotVerified, "not_supported": connector.ThreeDSNotVerified, "processing_error": connector.ThreeDSNotVerified} {
		var c charge
		raw := `{"payment_method_details":{"type":"card","card":{"three_d_secure":{"result":"` + result + `"}}}}`
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		if got := chargeThreeDS(&c); got != want {
			t.Errorf("%s = %q, want %q", result, got, want)
		}
	}
}

func TestNoStripeNameOnCard(t *testing.T) {
	if name := parsedFailure(t).Payment.MethodDetails.Card.Name; name != "" {
		t.Fatalf("a billing name is not the name on the card: %q", name)
	}
}

func TestWebhookEventsIncludePaymentFailed(t *testing.T) {
	if !slices.Contains(stripeWebhookEvents, "payment_intent.payment_failed") {
		t.Fatal("not subscribed")
	}
}

func TestExpiredSessionKeepsDeclineReason(t *testing.T) {
	f := newFake()
	pi := intentJSON("pi_3A1", "requires_payment_method", "pay_ref_01", nil)
	pi["last_payment_error"] = map[string]any{"code": "card_declined", "decline_code": "insufficient_funds", "message": "Your card has insufficient funds."}
	f.reply("GET /v1/checkout/sessions/cs_test_1", 200, sessionJSON("cs_test_1", "pay_ref_01", "expired", "unpaid", pi))
	snap, err := newConn(t, f).FetchPayment(bg, connector.PaymentRef{ProviderOrderID: "cs_test_1", ProviderRef: "pay_ref_01"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != connector.PaymentExpired || snap.ErrorCode != "insufficient_funds" {
		t.Fatalf("snap = %+v", snap)
	}
}

func TestSessionSnapshotCarriesTryDetails(t *testing.T) {
	f := newFake()
	pi := intentJSON("pi_3A1", "succeeded", "pay_ref_01", map[string]any{"id": "ch_ok_2", "payment_method_details": map[string]any{"type": "card", "card": map[string]any{
		"brand": "visa", "last4": "4242", "three_d_secure": map[string]any{"result": "authenticated"}, "checks": map[string]any{"cvc_check": "pass"}}}})
	f.reply("GET /v1/checkout/sessions/cs_test_1", 200, sessionJSON("cs_test_1", "pay_ref_01", "complete", "paid", pi))
	snap, err := newConn(t, f).FetchPayment(bg, connector.PaymentRef{ProviderOrderID: "cs_test_1", ProviderRef: "pay_ref_01"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.TryID != "ch_ok_2" || snap.ThreeDS != connector.ThreeDSPassed || snap.Risk.CVVCheck != connector.CheckMatch {
		t.Fatalf("snap = %+v", snap)
	}
}
