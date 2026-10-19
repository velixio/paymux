package redact

import (
	"strings"
	"testing"
)

func TestSpacedAndDashedIndianPhonesAreRedacted(t *testing.T) {
	r := New()
	for _, in := range []string{
		"customer 98765 43210 failed", "customer 98765-43210 failed", "customer +91 98765 43210 failed", "customer +91-98765-43210 failed",
		"customer 987-654-3210 failed", "customer 987 654 3210 failed", "customer 9876543210 failed",
	} {
		out := r.Text(in)
		if !strings.Contains(out, "[phone]") || strings.ContainsAny(out, "0123456789") && strings.Count(out, "[phone]") == 0 {
			t.Errorf("%q => %q", in, out)
		}
		digits := strings.Map(func(c rune) rune {
			if c >= '0' && c <= '9' {
				return c
			}
			return -1
		}, out)
		if len(digits) > 1 { // "091" prefix may stay; the number itself must not
			t.Errorf("%q => %q still has digits", in, out)
		}
	}
	// Not phone numbers.
	for _, keep := range []string{"amount 49900 paid", "order 2026-10-19 12:00", "code 123 456", "version 1.2.3", "1760000000000"} {
		if got := r.Text(keep); got != keep {
			t.Errorf("%q changed to %q", keep, got)
		}
	}
}

func TestAadhaarNumbersAreRedacted(t *testing.T) {
	r := New()
	for _, in := range []string{"aadhaar 2345 6789 0123", "aadhaar 2345-6789-0123", "aadhaar: 234567890124 end"} {
		out := r.Text(in)
		if strings.Contains(out, "2345") || strings.Contains(out, "6789") || strings.Contains(out, "0123") || strings.Contains(out, "234567890124") {
			t.Errorf("%q => %q", in, out)
		}
	}
	// A 12-digit run that is not grouped and fails the checksum is not an Aadhaar number.
	for _, keep := range []string{"order 123456789012 shipped", "ts 1760000000000", "amount 100000000000"} {
		if got := r.Text(keep); got != keep {
			t.Errorf("%q changed to %q", keep, got)
		}
	}
}

func TestDotSeparatedCardNumbersAreMasked(t *testing.T) {
	r := New()
	for _, in := range []string{"card 4111.1111.1111.1111 end", "card 4111/1111/1111/1111 end", "card 4111_1111_1111_1111 end"} {
		if out := r.Text(in); !strings.Contains(out, "****1111") || strings.Contains(out, "4111") {
			t.Errorf("%q => %q", in, out)
		}
	}
	for _, keep := range []string{"version 1.2.3.4", "ip 192.168.100.200", "4111.1111.1111.1112"} {
		if got := r.Text(keep); got != keep {
			t.Errorf("%q changed to %q", keep, got)
		}
	}
}

func TestMoreCardSecurityFieldNames(t *testing.T) {
	r := New()
	for _, name := range []string{"ccvv", "cvc", "cvv2", "cvn", "CCVV", "card_cvn", "cvcCode"} {
		if !IsCredentialName(name) {
			t.Errorf("%s is not a credential name", name)
		}
		if out := r.Body("application/json", []byte(`{"`+name+`":"123"}`)); strings.Contains(out, "123") {
			t.Errorf("json %s: %s", name, out)
		}
		if out := r.Body("application/x-www-form-urlencoded", []byte(name+`=123&x=1`)); strings.Contains(out, "123") {
			t.Errorf("form %s: %s", name, out)
		}
	}
	out := r.Body("application/x-www-form-urlencoded", []byte(`ccnum=4111111111111111&ccvv=123&ccexpmon=12&ccexpyear=2030`))
	for _, leak := range []string{"4111111111111111", "123", "2030"} {
		if strings.Contains(out, leak) {
			t.Errorf("form leaks %s: %s", leak, out)
		}
	}
}

func TestWebhookTokensInURLPathsAreRedacted(t *testing.T) {
	r := New()
	for name, tc := range map[string]struct{ in, secret string }{
		"telegram": {"https://api.telegram.org/bot123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw/sendMessage", "AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"},
		"slack":    {"https://hooks.slack.com/services/T01ABCDEF/B02GHIJKL/abcdEFGH1234ijklMNOP5678", "abcdEFGH1234ijklMNOP5678"},
		"generic":  {"https://hooks.example.net/in/Ab3dEf6hIj9kLm2nOp5qRs8tUv/notify", "Ab3dEf6hIj9kLm2nOp5qRs8tUv"},
		"discord":  {"https://discord.com/api/webhooks/123456789012345678/Zx9Yw8Vu7Ts6Rq5Po4Nm3Lk2Ji1Hg0Fe9Dc8Ba7Zy6Xw5Vu4Ts3Rq2Po1Nm0Lk", "Zx9Yw8Vu7Ts6Rq5Po4Nm3Lk2Ji1Hg0Fe9Dc8Ba7Zy6Xw5Vu4Ts3Rq2Po1Nm0Lk"},
	} {
		for where, out := range map[string]string{"URL": r.URL(tc.in), "Text": r.Text("posting to " + tc.in + " failed")} {
			if strings.Contains(out, tc.secret) || strings.Contains(out, tc.secret[:12]) {
				t.Errorf("%s in %s: %s", name, where, out)
			}
		}
	}
	// Our ids, ULIDs, ordinary words and short mixed-case segments stay readable.
	for _, keep := range []string{
		"https://pay.example.com/v1/payments/pay_01m598mztreem817jhfkaz534g/refunds",
		"https://pay.example.com/pay/pay_01m598mztreem817jhfkaz534g",
		"https://pay.example.com/v1/refunds/rf_01m598mztreem817jhfkaz534g",
		"https://pay.example.com/v1/refunds/ref_01m598mztreem817jhfkaz534g",
		"https://pay.example.com/orders/01M598MZTREEM817JHFKAZ534G",
		"https://pay.example.com/api/v2/subscriptions/invoices/downloads",
		"https://api.razorpay.com/v1/payments/pay_Nx5AbCdEfGhIjK/capture",
		"https://pay.example.com/documentation/getting-started-with-the-hosted-checkout",
		"https://pay.example.com/a/550e8400-e29b-41d4-a716-446655440000",
	} {
		if got := r.URL(keep); got != keep {
			t.Errorf("URL changed %s\n   to %s", keep, got)
		}
	}
}
