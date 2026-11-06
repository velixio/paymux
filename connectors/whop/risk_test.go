package whop

import (
	"encoding/json"
	"testing"
)

func decodePayment(t *testing.T, body string) paymentEntity {
	t.Helper()
	var p paymentEntity
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}

func TestSnapshotReadsRiskReview(t *testing.T) {
	s, err := decodePayment(t, `{"id":"pay_1","status":"paid","total":4.99,"currency":"usd",
		"risk_score":19,"ip_address":"203.0.113.24","three_ds_verified":false,
		"billing_address":{"city":"Testville","state":"Sample State","country":"US","line1":"1 Main St"},
		"verification_checks":{"address_line1":"FAIL","zip_code":"PASS","card_security_code":"pass","authorization_code":"A1B2C3"}}`).snapshot()
	if err != nil {
		t.Fatal(err)
	}
	r := s.Risk
	if r.Score == nil || *r.Score != 19 {
		t.Fatalf("score = %v", r.Score)
	}
	if r.IPAddress != "203.0.113.24" || r.City != "Testville" || r.Region != "Sample State" || r.Country != "US" {
		t.Fatalf("buyer facts = %+v", r)
	}
	if r.AddressCheck != "no_match" || r.PostalCheck != "match" || r.CVVCheck != "match" || r.AuthorizationCode != "A1B2C3" {
		t.Fatalf("checks = %+v", r)
	}
	if r.ThreeDSecure == nil || *r.ThreeDSecure {
		t.Fatalf("3DS = %v", r.ThreeDSecure)
	}
}

func TestSnapshotKeepsAnUnknownCheckResult(t *testing.T) {
	s, err := decodePayment(t, `{"id":"pay_1","status":"paid","total":1,"currency":"usd",
		"verification_checks":{"zip_code":"NOT_PROVIDED"}}`).snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if s.Risk.PostalCheck != "not_provided" {
		t.Fatalf("postal = %q", s.Risk.PostalCheck)
	}
}

// Risk data is for display: a field in an unexpected shape must never stop a
// payment from being processed.
func TestUnexpectedRiskShapesDoNotBreakThePayment(t *testing.T) {
	s, err := decodePayment(t, `{"id":"pay_1","status":"paid","total":1,"currency":"usd",
		"risk_score":{"value":1},"ip_address":7,"three_ds_verified":"yes",
		"billing_address":"none","verification_checks":[1]}`).snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Risk.Empty() {
		t.Fatalf("risk = %+v", s.Risk)
	}
}

func TestSnapshotWithoutRiskDataIsEmpty(t *testing.T) {
	s, err := decodePayment(t, `{"id":"pay_1","status":"paid","total":1,"currency":"usd",
		"risk_score":null,"ip_address":null,"three_ds_verified":false,"verification_checks":null}`).snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Risk.Empty() {
		t.Fatalf("a bare false 3DS flag alone should not count as risk data: %+v", s.Risk)
	}
}
