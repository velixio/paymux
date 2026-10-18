package redact

import (
	"strings"
	"unicode"
)

// words splits a field name into lower-case words at separators and at
// camelCase boundaries: "razorpayPaymentId" -> razorpay payment id,
// "X-Api-Key" -> x api key. Letters and digits stay together ("pkcs12").
func words(key string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(key)
	for i, r := range rs {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) && len(cur) > 0 {
				prev := rs[i-1]
				nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
				if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
					flush()
				}
			}
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// credWords are words that make a field a credential wherever they appear.
var credWords = set("authorization", "cookie", "cookies", "setcookie", "password", "passwords", "passwd", "pwd",
	"passphrase", "secret", "secrets", "token", "tokens", "salt", "hash", "signature", "signatures", "otp", "cvv", "cvv2",
	"cvc", "cvc2", "pkcs12", "apikey", "privatekey", "credential", "credentials", "bearer", "pin")

// credKeyPrev are the words that make "<word> key" a credential.
var credKeyPrev = set("api", "private", "secret", "access", "signing", "master", "encryption", "auth", "license")

// credSquashed are credential names that are run together in some APIs
// (clientsecret, refreshtoken): matched inside the key with separators removed.
var credSquashed = []string{"secret", "password", "passwd", "apikey", "authorization", "signature", "privatekey",
	"cardnumber", "accesstoken", "refreshtoken", "idtoken", "passphrase", "cookie", "pkcs12"}

// piiWords are words that make a field customer personal data.
var piiWords = set("email", "emails", "phone", "phones", "mobile", "msisdn", "vpa", "gstin", "pan", "address", "addresses",
	"street", "line1", "line2", "pincode", "zip", "city", "dob", "birthday")

// nameLead are the words that make "<word> name" a person's name.
var nameLead = set("first", "last", "full", "middle", "customer", "card", "cardholder", "holder", "billing", "shipping",
	"buyer", "payer", "payee", "account", "legal", "recipient", "beneficiary", "contact", "user", "cust")

func set(ws ...string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

// isCredentialName reports whether a field called key holds a credential.
// Whole words are matched, not substrings, so "footprint" (otp) and
// "payment_id", "key_id", "razorpay_key_id" stay readable.
func isCredentialName(key string) bool {
	if key == "t" || key == "cs" { // the checkout page's secret query parameters
		return true
	}
	ws := words(key)
	for i, w := range ws {
		if credWords[w] {
			return true
		}
		if w == "key" && i > 0 && credKeyPrev[ws[i-1]] {
			return true
		}
		if w == "number" && i > 0 && ws[i-1] == "card" {
			return true
		}
	}
	sq := strings.ToLower(strings.Join(ws, ""))
	for _, c := range credSquashed {
		if strings.Contains(sq, c) {
			return true
		}
	}
	return false
}

// isPIIName reports whether a field called key holds a customer's personal
// data (name, e-mail, phone, address, UPI handle, GSTIN, PAN). Our own ids
// and the provider's ids ("contact_id", "customer_id") are kept.
func isPIIName(key string) bool {
	ws := words(key)
	if len(ws) == 0 {
		return false
	}
	last := ws[len(ws)-1]
	idLike := last == "id" || last == "ids" || last == "ref" || last == "reference"
	for i, w := range ws {
		if w == "upi" && i+1 < len(ws) && (ws[i+1] == "id" || ws[i+1] == "handle" || ws[i+1] == "vpa") {
			return true
		}
		if piiWords[w] {
			return true
		}
		if w == "contact" && !idLike {
			return true
		}
		if w == "name" && (len(ws) == 1 || (i > 0 && nameLead[ws[i-1]])) {
			return true
		}
		if (w == "firstname" || w == "lastname" || w == "fullname" || w == "cardholder" || w == "holdername") && !idLike {
			return true
		}
	}
	return false
}
