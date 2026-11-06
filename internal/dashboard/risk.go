package dashboard

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// riskView is an attempt's risk review as the payment pages draw it.
type riskView struct {
	Score, Tone, Location, IPAddress, AuthorizationCode, Provider string
	// DialogID names the modal, unique on the page: a list draws one per row.
	DialogID string
	Checks   []riskCheck
}

type riskCheck struct{ Label, Text, Tone, Mark string }

// riskBand labels a 0-100 risk score. These bands are Paymux's own display
// choice, not a provider's verdict.
func riskBand(score int) (label, tone string) {
	switch {
	case score < 40:
		tone, label = "low", "Low"
	case score < 75:
		tone, label = "medium", "Medium"
	default:
		tone, label = "high", "High"
	}
	return strconv.Itoa(score) + " " + label, tone
}

// paymentRiskView returns nil when the attempt has no risk data to show.
func paymentRiskView(a store.Attempt) *riskView {
	return riskViewFromJSON(a.RiskDetails, a.Status)
}

// tryRiskView is riskViewFromJSON for a try, whose 3D Secure result is more
// exact than the provider's verified flag: it can say the check failed.
func tryRiskView(raw, status, threeDS string) *riskView {
	v := riskViewFromJSON(raw, status)
	if v == nil || threeDS == "" {
		return v
	}
	check := riskCheck{Label: "3D Secure", Text: "Not verified", Tone: "none", Mark: "–"}
	switch threeDS {
	case connector.ThreeDSPassed:
		check.Text, check.Tone, check.Mark = "Verified", "pass", "✓"
	case connector.ThreeDSFailed:
		check.Text, check.Tone, check.Mark = "Failed", "fail", "✕"
	}
	for i := range v.Checks {
		if v.Checks[i].Label == check.Label {
			v.Checks[i] = check
			return v
		}
	}
	v.Checks = append(v.Checks, check)
	return v
}

// riskViewFromJSON maps stored risk JSON of an attempt or try in the given
// status. It returns nil when there is nothing to show.
func riskViewFromJSON(raw, status string) *riskView {
	var r connector.PaymentRisk
	if raw == "" || json.Unmarshal([]byte(raw), &r) != nil || r.Empty() {
		return nil
	}
	v := &riskView{DialogID: "risk-review", Tone: "none", Score: "Pending", IPAddress: r.IPAddress, AuthorizationCode: r.AuthorizationCode}
	if r.Score != nil {
		v.Score, v.Tone = riskBand(*r.Score)
	} else if status == string(state.AttemptFailed) {
		v.Score = "Not scored"
	}
	location := make([]string, 0, 3)
	for _, part := range []string{r.City, r.Region, r.Country} {
		if part != "" {
			location = append(location, part)
		}
	}
	v.Location = strings.Join(location, ", ")
	for _, c := range []struct{ label, result string }{
		{"Billing address", r.AddressCheck}, {"Postal code", r.PostalCheck}, {"CVV", r.CVVCheck},
	} {
		if c.result != "" {
			v.Checks = append(v.Checks, verificationCheck(c.label, c.result))
		}
	}
	if r.ThreeDSecure != nil {
		check := riskCheck{Label: "3D Secure", Text: "Not verified", Tone: "none", Mark: "–"}
		if *r.ThreeDSecure {
			check.Text, check.Tone, check.Mark = "Verified", "pass", "✓"
		}
		v.Checks = append(v.Checks, check)
	}
	return v
}

func verificationCheck(label, result string) riskCheck {
	switch result {
	case connector.CheckMatch:
		return riskCheck{Label: label, Text: "Match", Tone: "pass", Mark: "✓"}
	case connector.CheckNoMatch:
		return riskCheck{Label: label, Text: "No match", Tone: "fail", Mark: "✕"}
	}
	text := strings.ReplaceAll(result, "_", " ")
	return riskCheck{Label: label, Text: strings.ToUpper(text[:1]) + text[1:], Tone: "none", Mark: "–"}
}
