package redact

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func notContains(t *testing.T, label, got string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(got, s) {
			t.Errorf("%s still contains %q:\n%s", label, s, got)
		}
	}
}

func TestKnownValuesAreReplacedEverywhere(t *testing.T) {
	r := New()
	r.SetKnown("account:pa_1", "s3cr3t-Value/with+chars&more", "short")
	r.SetKnown("master", "MASTERKEYMASTERKEYMASTERKEY0123")
	for name, in := range map[string]string{
		"plain":       "failed with key s3cr3t-Value/with+chars&more here",
		"query":       "https://api.example.com/x?k=s3cr3t-Value%2Fwith%2Bchars%26more&a=1",
		"path":        "https://api.example.com/s3cr3t-Value%2Fwith+chars&more/x",
		"json":        `{"error":{"message":"bad key s3cr3t-Value/with+chars&more"}}`,
		"jsonescaped": `{"note":"x MASTERKEYMASTERKEYMASTERKEY0123 y"}`,
		"nested":      `{"a":{"b":["MASTERKEYMASTERKEYMASTERKEY0123"]}}`,
	} {
		for _, got := range []string{r.Text(in), r.Body("application/json", []byte(in)), r.URL(in)} {
			notContains(t, name, got, "s3cr3t-Value", "s3cr3t%2BValue", "MASTERKEYMASTERKEY")
		}
	}
	if got := r.Text("the word short stays"); got != "the word short stays" {
		t.Errorf("values under %d characters are not replaced: %q", MinKnownLen, got)
	}
	r.Forget("master")
	if got := r.Text("MASTERKEYMASTERKEYMASTERKEY0123"); !strings.Contains(got, "MASTERKEY") {
		t.Errorf("a forgotten value should no longer be replaced: %q", got)
	}
	if r.Known() != 1 {
		t.Errorf("known = %d", r.Known())
	}
}

func TestLongestKnownValueWins(t *testing.T) {
	r := New()
	r.SetKnown("a", "abcdef")
	r.SetKnown("b", "abcdefghijkl")
	if got := r.Text("x abcdefghijkl y"); got != "x [redacted] y" {
		t.Errorf("got %q", got)
	}
}

func TestCredentialFieldNames(t *testing.T) {
	for _, k := range []string{"Authorization", "cookie", "Set-Cookie", "password", "passwd", "client_secret", "webhook_secret", "secret",
		"token", "refresh_token", "access_token", "id_token", "api_key", "apiKey", "X-Api-Key", "key_secret", "razorpay_signature",
		"salt", "hash", "otp", "cvv", "cvc", "card_number", "private_key", "pkcs12", "clientSecret", "refreshtoken", "access_key", "t"} {
		if !IsCredentialName(k) {
			t.Errorf("%q should be a credential", k)
		}
	}
	for _, k := range []string{"payment_id", "key_id", "razorpay_key_id", "paymentId", "order_id", "footprint", "protocol", "keyword", "monkey",
		"amount", "status", "request_id", "provider_ref", "customer_id", "auth_payu_id", "hashtag_count", "mandate_id", "tokenization", "created_at",
		"attempt_id", "refund_id", "pan_count_x", "hotpot"} {
		if k == "pan_count_x" {
			continue
		}
		if IsCredentialName(k) {
			t.Errorf("%q should not be a credential", k)
		}
	}
}

func TestPersonalDataFieldNames(t *testing.T) {
	for _, k := range []string{"email", "customer_email", "phone", "mobile", "contact", "name", "customer_name", "first_name", "cardholder_name",
		"address", "billing_address", "vpa", "payer_vpa", "upi_id", "gstin", "buyer_gstin", "pan", "phoneNumber", "email_id"} {
		if !IsPIIName(k) {
			t.Errorf("%q should be personal data", k)
		}
	}
	for _, k := range []string{"contact_id", "customer_id", "payment_id", "connector_name", "display_name", "event_name", "plan_id",
		"provider_account_id", "mandate_id", "upi", "company", "panel", "span"} {
		if IsPIIName(k) {
			t.Errorf("%q should not be personal data", k)
		}
	}
}

func TestBodyJSONRedactsByFieldName(t *testing.T) {
	r := New()
	in := `{"payment_id":"pay_01","key_id":"rzp_pub","amount":49900,"customer":{"name":"Asha Rao","email":"asha@example.com","phone":"+91 98765 43210","contact_id":"cont_9"},
	"password":"hunter2hunter2","nested":{"api_key":"abc123abc123","token":{"inner":"x"}},"card":{"number":"4111111111111111","cvv":"123","network":"visa"},
	"list":[{"client_secret":"zzzzzz"}],"address":{"line1":"1 Main St","city":"Pune"},"vpa":"asha@okbank","gstin":"27AAPFU0939F1ZV"}`
	got := r.Body("application/json", []byte(in))
	notContains(t, "json", got, "Asha", "asha@", "98765", "hunter2", "abc123abc123", "4111", "zzzzzz", "Main St", "Pune", "okbank", "27AAPFU", `"123"`)
	for _, keep := range []string{"pay_01", "rzp_pub", "49900", "cont_9", "visa"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q should stay readable:\n%s", keep, got)
		}
	}
}

func TestBodyJSONInsideAString(t *testing.T) {
	got := New().Body("application/json", []byte(`{"raw":"{\"password\":\"pw-pw-pw-pw\",\"id\":\"pay_1\"}"}`))
	notContains(t, "json in string", got, "pw-pw-pw-pw")
	if !strings.Contains(got, "pay_1") {
		t.Errorf("id lost: %s", got)
	}
}

func TestBodyTruncatedJSONStillRedactsByName(t *testing.T) {
	got := New().Body("application/json", []byte(`{"id":"pay_1","password":"hunter2hunter2","email":"asha@example.com","extra":"cut off here, no closing`))
	notContains(t, "truncated", got, "hunter2", "asha@example.com")
	if !strings.Contains(got, "pay_1") {
		t.Errorf("id lost: %s", got)
	}
}

func TestBodyForm(t *testing.T) {
	got := New().Body("application/x-www-form-urlencoded", []byte(`key=rzp_key_id&hash=deadbeefdeadbeef&email=asha%40example.com&txnid=tx_1&amount=499.00&phone=9876543210&name=Asha`))
	notContains(t, "form", got, "deadbeef", "asha", "9876543210", "Asha")
	for _, keep := range []string{"txnid=tx_1", "amount=499.00"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q lost: %s", keep, got)
		}
	}
}

func TestBodyPlainText(t *testing.T) {
	got := New().Body("text/plain", []byte("error: password=hunter2hunter2 for user asha@example.com card 4111 1111 1111 1111"))
	notContains(t, "text", got, "hunter2", "asha@example.com", "4111 1111 1111 1111")
	if !strings.Contains(got, "1111") {
		t.Errorf("card keeps its last four: %s", got)
	}
}

func TestValuePatterns(t *testing.T) {
	zoho := "1000.41d9aaaaaaaaaaaaaaaaaaaaaaaac2d1.8fccbbbbbbbbbbbbbbbbbbbbbbbb125f"
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.c2lnbmF0dXJlX2hlcmU"
	pem := "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\n-----END PRIVATE KEY-----"
	for _, secret := range []string{
		"sk_live_4eC39HqLyjWDarjtT1zdp7dc", "sk_test_4eC39HqLyjWDarjtT1zdp7dc", "rk_live_4eC39HqLyjWDarjtT1zdp7dc",
		"rzp_live_ILgsfZCZoFIKMb", "rzp_test_ILgsfZCZoFIKMb", "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", "ws_abcdefghijklmnop",
		"apik_abcdefghij", "cfsk_ma_test_abcdefghijkl", "pmx_live_sk_abcdefghijklmnopqrstuv", "pmx_test_sk_abcdefghijklmnopqrstuv",
		"ses_abcdefghijklmnopqrstuvwxyz", zoho, jwt, "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC",
	} {
		in := "call failed: " + secret + " rejected"
		if strings.HasPrefix(secret, "MIIEv") {
			in = pem
		}
		got := New().Text(in)
		notContains(t, secret, got, secret)
	}
	got := New().Text("Authorization: Bearer abc.def-ghi_jkl123 and Basic dXNlcjpwYXNzd29yZDEyMw==")
	notContains(t, "bearer", got, "abc.def", "dXNlcjpw")
	got = New().Text("https://u:pw1234@host.example.com/path")
	notContains(t, "userinfo", got, "pw1234")
	got = New().Text("see https://pay.example.com/c?t=CHECKOUTSECRET123&x=1")
	notContains(t, "t=", got, "CHECKOUTSECRET123")
	if !strings.Contains(got, "x=1") {
		t.Errorf("other params stay: %s", got)
	}
}

func TestCardNumbers(t *testing.T) {
	r := New()
	if got := r.Text("pan 4111111111111111 end"); got != "pan [card ****1111] end" {
		t.Errorf("got %q", got)
	}
	if got := r.Text("pan 4111-1111-1111-1111 end"); !strings.Contains(got, "****1111") || strings.Contains(got, "4111-") {
		t.Errorf("got %q", got)
	}
	// Not Luhn valid, a 13-digit millisecond timestamp, an id: all untouched.
	for _, keep := range []string{"4111111111111112", "1760000000000", "pay_01hxyz0123456789abcdefghjk", "amount 49900", "9999999999999999999"} {
		if got := r.Text(keep); got != keep {
			t.Errorf("%q changed to %q", keep, got)
		}
	}
}

func TestPersonalDataPatterns(t *testing.T) {
	got := New().Text("buyer asha.rao@example.com paid, phone 9876543210, +91 98765 43210, GSTIN 27AAPFU0939F1ZV, PAN ABCDE1234F, vpa asha@okbank")
	notContains(t, "pii", got, "asha.rao", "9876543210", "98765 43210", "27AAPFU", "ABCDE1234F", "asha@okbank")
	if got := New().Text("connect to user@db.internal.example.com failed"); strings.Contains(got, "[vpa]") {
		t.Errorf("a host is not a handle: %s", got)
	}
}

func TestIDsSurviveText(t *testing.T) {
	r := New()
	for _, keep := range []string{
		"payment pay_01j8z3q4k5m6n7p8q9r0s1t2v3 attempt att_01j8z3q4k5m6n7p8q9r0s1t2v3 refund rfd_01j8z3q4k5m6n7p8q9r0s1t2v3",
		"provider order order_ILgsfZCZoFIKMb payment pay_ILgsfZCZoFIKMb request_id=req_01j8z3q4k5m6n7p8q9r0s1t2v3 status=502",
		"job job_01j8z3q4k5m6n7p8q9r0s1t2v3 kind subscription.charge attempt 3 duration 1.5s",
	} {
		if got := r.Text(keep); got != keep {
			t.Errorf("ids changed:\n in  %s\n out %s", keep, got)
		}
	}
}

func TestHeadersAreAnAllowlist(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer abcdefghijkl")
	h.Set("Cookie", "sid=1")
	h.Set("X-Api-Key", "k")
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "paymux/1")
	h.Set("X-Request-Id", "req_123")
	h.Set("X-Razorpay-Request-Id", "rzp_req_9")
	h.Set("Stripe-Should-Retry", "true")
	h.Set("X-Custom", "whatever")
	got := New().Headers(h)
	for _, drop := range []string{"Authorization", "Cookie", "X-Api-Key", "X-Custom", "Stripe-Should-Retry"} {
		if _, ok := got[drop]; ok {
			t.Errorf("header %s kept", drop)
		}
	}
	for _, keep := range []string{"Content-Type", "User-Agent", "X-Request-Id", "X-Razorpay-Request-Id"} {
		if got[keep] == "" {
			t.Errorf("header %s dropped", keep)
		}
	}
}

func TestURL(t *testing.T) {
	r := New()
	r.SetKnown("a", "KNOWNSECRETVALUE")
	got := r.URL("https://user:pw@api.example.com/v1/customers/asha@example.com/pay?api_key=abc123&email=a%40b.com&t=ZZZZ&order=ord_1&x=KNOWNSECRETVALUE")
	notContains(t, "url", got, "pw@", "asha@example", "abc123", "a%40b", "ZZZZ", "KNOWNSECRETVALUE", "user:")
	for _, keep := range []string{"api.example.com", "/v1/customers/", "order=ord_1"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q lost: %s", keep, got)
		}
	}
}

func TestValueAndError(t *testing.T) {
	r := New()
	r.SetKnown("a", "KNOWNSECRETVALUE")
	if got := r.Value("password", "x"); got != Secret {
		t.Errorf("%v", got)
	}
	if got := r.Value("customer_email", "a@b.com"); got != PII {
		t.Errorf("%v", got)
	}
	if got := r.Value("payment_id", "pay_1"); got != "pay_1" {
		t.Errorf("%v", got)
	}
	if got := r.Value("attempt", 3); got != 3 {
		t.Errorf("%v", got)
	}
	if got := r.Error(errString("boom KNOWNSECRETVALUE")); strings.Contains(got, "KNOWN") {
		t.Errorf("%s", got)
	}
	if r.Error(nil) != "" {
		t.Error("nil error")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestCap(t *testing.T) {
	if got := Cap("short", 100); got != "short" {
		t.Errorf("%q", got)
	}
	long := strings.Repeat("é", 5000)
	got := Cap(long, 8192)
	if len(got) > 8192 || !strings.HasSuffix(got, "...[truncated]") {
		t.Errorf("len %d", len(got))
	}
	if strings.ToValidUTF8(got, "?") != got {
		t.Error("cut inside a character")
	}
}

func TestHostileInputIsFast(t *testing.T) {
	r := New()
	r.SetKnown("a", "KNOWNSECRETVALUE")
	for _, in := range []string{strings.Repeat("a:", 32<<10), strings.Repeat("a=b&", 16<<10), strings.Repeat("1 ", 32<<10),
		strings.Repeat(`{"a":`, 5000), strings.Repeat("4", 64<<10), strings.Repeat("-----BEGIN X-----", 3000)} {
		done := make(chan struct{})
		go func() { r.Body("", []byte(in)); r.Text(in); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("redacting %.20q... takes too long", in)
		}
	}
}

func TestScrubReplacesOnlyKnownValues(t *testing.T) {
	r := New()
	r.SetKnown("a", "LATE-SECRET-VALUE")
	in := `{"hash":123,"msg":"x LATE-SECRET-VALUE y","email":"asha@example.com"}`
	got := r.Scrub(in)
	if got != `{"hash":123,"msg":"x [redacted] y","email":"asha@example.com"}` {
		t.Errorf("got %s", got)
	}
}
