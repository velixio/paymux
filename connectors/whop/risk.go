package whop

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/velixio/paymux/pkg/connector"
)

// risk reads the payment's risk review. Every field is decoded on its own and
// a bad one is skipped: this data is only displayed, so an unexpected shape
// must never stop the payment itself from being processed.
func (p paymentEntity) risk() connector.PaymentRisk {
	var out connector.PaymentRisk
	var score float64
	if json.Unmarshal(p.RiskScore, &score) == nil && p.hasValue(p.RiskScore) {
		rounded := int(math.Round(score))
		out.Score = &rounded
	}
	_ = json.Unmarshal(p.IPAddress, &out.IPAddress)
	var verified bool
	if json.Unmarshal(p.ThreeDSVerified, &verified) == nil && p.hasValue(p.ThreeDSVerified) {
		out.ThreeDSecure = &verified
	}
	var address struct {
		City    string `json:"city"`
		State   string `json:"state"`
		Country string `json:"country"`
	}
	if json.Unmarshal(p.BillingAddress, &address) == nil {
		out.City, out.Region, out.Country = address.City, address.State, address.Country
	}
	var checks struct {
		AddressLine1      json.RawMessage `json:"address_line1"`
		ZipCode           json.RawMessage `json:"zip_code"`
		CardSecurityCode  json.RawMessage `json:"card_security_code"`
		AuthorizationCode json.RawMessage `json:"authorization_code"`
	}
	if json.Unmarshal(p.VerificationChecks, &checks) == nil {
		out.AddressCheck = checkResult(checks.AddressLine1)
		out.PostalCheck = checkResult(checks.ZipCode)
		out.CVVCheck = checkResult(checks.CardSecurityCode)
		_ = json.Unmarshal(checks.AuthorizationCode, &out.AuthorizationCode)
	}
	return out
}

func (paymentEntity) hasValue(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// checkResult maps a verification result to connector.CheckMatch or
// connector.CheckNoMatch. Any other result is kept, lower case, for display.
func checkResult(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "pass", "passed", "match", "matched":
		return connector.CheckMatch
	case "fail", "failed", "no_match", "mismatch":
		return connector.CheckNoMatch
	}
	return value
}
