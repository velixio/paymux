package payu

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

// PayU's mandate webhooks (docs "Set up WebHook to Receive Cancellation or
// Modification Update from the Issuer Bank"): cards post JSON with a hash over
// status|authpayuId|notificationType|billingAmount|paymentStartDate|paymentEndDate|
// message|eventDate|key|udf1..udf5|salt; UPI posts a form with a hash over
// status|action|authpayuid|dateTime|amount|endDate|salt; net-banking e-NACH
// cancellations carry no hash at all. The hashes below were computed
// independently with Python hashlib.
const (
	cardModifyHash = "2d2144eb33538ecc26602b4713e3025f829e522bfa81ae986f6354f6b3175912ee6dab85c9ca2ede3652e03cbee92100e16be471b3ba20b8f487d517cbc95cdd"
	cardCancelHash = "59db5188b906df4597851a6d229ab6946964923dbe0ed1987f0fb6cc4e1f120f7837eaa77ebe04c4e0198e2b1191e06aae509750bca26a5f718ecce3fb254d01"
	upiRevokeHash  = "a4fe2a9d3be0405ec46e3dd76a4f37eeb5dc3e8e27cced736dd7bbd6309d4edf64840d818d00fc2079cfd1d0a927847f20107a8fc1ce365d7ee068a85151b3fb"
	upiPauseHash   = "1eeb1195cde47ed9f5afe4677405ae8a74abd9a311f53b1fb151f8555c4fa59edf942353c9c361f60b6dce9e900276bdb65e04d6b0c91770834b73d2f272902b"
)

func cardNotice(typ, status, message, hash string) string {
	return `{"status":"` + status + `","authpayuid":"20000064468","notificationType":"` + typ + `","si_details":{"billingAmount":"400.00","paymentStartDate":"2026-10-18","paymentEndDate":"2030-04-13"},` +
		`"eventDate":"2026-10-19","message":"` + message + `","key":"testkey1","udf1":"","udf2":"","udf3":"","udf4":"","udf5":"","hash":"` + hash + `"}`
}

func upiNotice(status, action, hash string) string {
	f := url.Values{"status": {status}, "action": {action}, "authpayuid": {"20000064468"}, "key": {"testkey1"}, "dateTime": {"2026-10-18"}, "amount": {"1500.00"},
		"endDate": {"2031-10-18"}, "mandateNumber": {"abc@mybank"}, "hash": {hash}}
	return f.Encode()
}

func rawHook(body string) connector.WebhookRequest {
	return connector.WebhookRequest{Method: "POST", Body: []byte(body), ReceivedAt: 1}
}

func TestCardMandateNotices(t *testing.T) {
	ctx := context.Background()
	c := newConn(t, &fake{})
	mod := rawHook(cardNotice("MANDATE_MODIFICATION", "active", "Mandate modified", cardModifyHash))
	if err := c.VerifyWebhook(ctx, mod); err != nil {
		t.Fatalf("a real card notice: %v", err)
	}
	ns, err := c.ParseWebhook(ctx, mod)
	if err != nil || len(ns) != 1 || ns[0].Kind != connector.NotifyMandate || ns[0].Mandate == nil {
		t.Fatalf("%+v %v", ns, err)
	}
	m := ns[0].Mandate
	if m.ProviderMandateID != "20000064468" || m.Status != "active" || m.Method != "card" || m.MaxAmount != 40000 || m.ValidUntil == 0 || ns[0].EventID == "" {
		t.Errorf("modify: %+v", ns[0])
	}
	can := rawHook(cardNotice("MANDATE_CANCELLATION", "deleted", "Mandate cancelled", cardCancelHash))
	if err := c.VerifyWebhook(ctx, can); err != nil {
		t.Fatalf("cancel notice: %v", err)
	}
	if ns, _ = c.ParseWebhook(ctx, can); len(ns) != 1 || ns[0].Mandate.Status != "revoked" {
		t.Errorf("cancel: %+v", ns)
	}
	if ns[0].EventID == "" || ns[0].EventID == func() string { n, _ := c.ParseWebhook(ctx, mod); return n[0].EventID }() {
		t.Error("distinct notices need distinct event ids")
	}
	// Tampered or forged.
	for name, body := range map[string]string{
		"wrong hash":     cardNotice("MANDATE_CANCELLATION", "deleted", "Mandate cancelled", strings.Repeat("0", 128)),
		"changed amount": strings.Replace(cardNotice("MANDATE_MODIFICATION", "active", "Mandate modified", cardModifyHash), "400.00", "9400.00", 1),
		"changed type":   cardNotice("MANDATE_CANCELLATION", "active", "Mandate modified", cardModifyHash),
	} {
		if err := c.VerifyWebhook(ctx, rawHook(body)); !errors.Is(err, connector.ErrInvalidSignature) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestUPIMandateNotices(t *testing.T) {
	ctx := context.Background()
	c := newConn(t, &fake{})
	rev := rawHook(upiNotice("revoked", "MANDATE_REVOKE", upiRevokeHash))
	if err := c.VerifyWebhook(ctx, rev); err != nil {
		t.Fatalf("a real UPI notice: %v", err)
	}
	ns, err := c.ParseWebhook(ctx, rev)
	if err != nil || len(ns) != 1 || ns[0].Mandate == nil || ns[0].Mandate.Status != "revoked" || ns[0].Mandate.Method != "upi" || ns[0].Mandate.ProviderMandateID != "20000064468" {
		t.Fatalf("%+v %v", ns, err)
	}
	pause := rawHook(upiNotice("pause", "MANDATE_PAUSE", upiPauseHash))
	if err := c.VerifyWebhook(ctx, pause); err != nil {
		t.Fatal(err)
	}
	if ns, _ = c.ParseWebhook(ctx, pause); len(ns) != 1 || ns[0].Mandate.Status != "pending" {
		t.Errorf("a pause is not a cancellation: %+v", ns)
	}
	for name, body := range map[string]string{
		"no hash":    strings.Replace(upiNotice("revoked", "MANDATE_REVOKE", ""), "&hash=", "&x=", 1),
		"wrong hash": upiNotice("revoked", "MANDATE_REVOKE", strings.Repeat("a", 128)),
		"wrong body": upiNotice("active", "MANDATE_REVOKE", upiRevokeHash),
	} {
		if err := c.VerifyWebhook(ctx, rawHook(body)); !errors.Is(err, connector.ErrInvalidSignature) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A net-banking cancellation notice has no hash, so it is only a hint: what the
// connector reports is what PayU's status command says.
func TestENACHCancellationIsAHintConfirmedByTheStatusCommand(t *testing.T) {
	ctx := context.Background()
	body := `{"statusCode":1,"notificationType":"MANDATE_CANCELLATION","status":"CANCEL_SUCCESS","authPayuId":"20000064468"}`
	f := &fake{cmds: map[string]string{"NB_mandate_status": `{"status":"CANCEL_SUCCESS","action":"NB_mandate_status","authpayuid":"20000064468","amount":"1500.00","deletedon":"2026-10-20"}`}}
	c := newConn(t, f)
	if err := c.VerifyWebhook(ctx, rawHook(body)); !errors.Is(err, connector.ErrUnsigned) {
		t.Fatalf("err = %v, want ErrUnsigned", err)
	}
	ns, err := c.ParseWebhook(ctx, rawHook(body))
	if err != nil || len(ns) != 1 || ns[0].Mandate == nil || ns[0].Mandate.Status != "revoked" || ns[0].Mandate.Method != "emandate" {
		t.Fatalf("%+v %v", ns, err)
	}
	// A forged notice for a mandate that is in fact active changes nothing.
	f.cmds["NB_mandate_status"] = `{"status":"SUCCESS","action":"NB_mandate_status","authpayuid":"20000064468","amount":"1500.00"}`
	if ns, err = c.ParseWebhook(ctx, rawHook(body)); err != nil || len(ns) != 1 || ns[0].Mandate.Status != "active" {
		t.Errorf("forged: %+v %v", ns, err)
	}
	// If PayU cannot confirm, nothing is reported (the job retries).
	f.cmds["NB_mandate_status"] = `{"status":0,"message":"ENACH Mandate entry not found","action":"NB_mandate_status"}`
	if _, err = c.ParseWebhook(ctx, rawHook(body)); err == nil {
		t.Error("an unconfirmed notice was reported")
	}
}

// Payment webhooks (the debit results of si_transaction come as ordinary
// payment notices keyed by the txnid Paymux sent) are untouched.
func TestPaymentWebhookStillWorksBesideMandateNotices(t *testing.T) {
	c := newConn(t, &fake{})
	ok := successForm(testSalt, "")
	if err := c.VerifyWebhook(context.Background(), webhook(ok)); err != nil {
		t.Fatal(err)
	}
	ns, err := c.ParseWebhook(context.Background(), webhook(ok))
	if err != nil || len(ns) != 1 || ns[0].Kind != connector.NotifyPayment {
		t.Fatalf("%+v %v", ns, err)
	}
}

// L3: naming an unsigned notice must not call PayU. The inbound handler asks for
// the event id on every delivery, so a flood of forged e-NACH notices would
// otherwise use up the account's PayU rate limit. The fetch that confirms the
// notice belongs to the background job (ParseWebhook).
func TestMandateNoticeEventIDNeedsNoNetworkCall(t *testing.T) {
	ctx := context.Background()
	f := &fake{cmds: map[string]string{"NB_mandate_status": `{"status":"CANCEL_SUCCESS","authpayuid":"20000064468","amount":"1500.00"}`}}
	c := newConn(t, f)
	ider, ok := c.(connector.WebhookEventIDer)
	if !ok {
		t.Fatal("the PayU connector does not implement connector.WebhookEventIDer")
	}
	enach := `{"statusCode":1,"notificationType":"MANDATE_CANCELLATION","status":"CANCEL_SUCCESS","authPayuId":"20000064468"}`
	id1, ok := ider.WebhookEventID(rawHook(enach))
	if !ok || id1 == "" {
		t.Fatalf("no id for an unsigned notice: %q %v", id1, ok)
	}
	for i := 0; i < 50; i++ {
		ider.WebhookEventID(rawHook(enach))
	}
	if n := len(f.commands()); n != 0 {
		t.Fatalf("naming 51 unsigned notices made %d PayU calls", n)
	}
	if id2, _ := ider.WebhookEventID(rawHook(enach)); id2 != id1 {
		t.Errorf("a retried notice must get the same id: %q vs %q", id1, id2)
	}
	other := strings.Replace(enach, "20000064468", "20000064469", 1)
	if id3, _ := ider.WebhookEventID(rawHook(other)); id3 == id1 {
		t.Error("a notice about another mandate got the same id")
	}
	// It is the id ParseWebhook reports (that one fetches, in the job).
	ns, err := c.ParseWebhook(ctx, rawHook(enach))
	if err != nil || len(ns) != 1 || ns[0].EventID != id1 {
		t.Errorf("ParseWebhook id %v (%v), want %q", ns, err, id1)
	}
	// Signed notices are named from the body too; a payment webhook is not this
	// method's business (the caller parses it).
	if id, ok := ider.WebhookEventID(rawHook(cardNotice("MANDATE_CANCELLATION", "deleted", "Mandate cancelled", cardCancelHash))); !ok || id == "" {
		t.Errorf("signed card notice: %q %v", id, ok)
	}
	if _, ok := ider.WebhookEventID(webhook(successForm(testSalt, ""))); ok {
		t.Error("a payment webhook was claimed")
	}
}
