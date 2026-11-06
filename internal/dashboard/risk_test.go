package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func (e *env) seedRiskPayment(id, risk string) {
	e.t.Helper()
	app := e.seedApp(strings.ReplaceAll(id, "_", "-"))
	p := e.seedPayment(id, app.ID, "live", "succeeded", "", 2900, time.Now())
	e.seedAttempt(store.Attempt{ID: "att_" + id, Mode: "live", PaymentID: p.ID, ProviderAccountID: "pa_whop",
		ProviderRef: "pr_" + id, Status: "captured", AmountReceived: 2900, CurrencyReceived: p.Currency, RiskDetails: risk})
}

func TestPaymentPageShowsRiskReviewInAModal(t *testing.T) {
	e := riskEnv(t)
	c := e.roleClient("viewer")
	e.seedRiskPayment("pay_risk", `{"score":19,"ip_address":"203.0.113.24","city":"Testville","region":"Sample State","country":"US",
		"address_check":"no_match","postal_check":"match","cvv_check":"match","three_d_secure":false,"authorization_code":"A1B2C3"}`)
	body := e.pageOK(c, "/dashboard/payments/pay_risk")
	has(t, body,
		`command="show-modal"`, `commandfor="risk-review"`, `<dialog id="risk-review"`, `command="close"`,
		`risk-score risk-low`, "19 Low", "Testville, Sample State, US", "203.0.113.24",
		"No match", "Match", "Not verified", "A1B2C3")
}

func TestPaymentPageHidesRiskReviewWithoutData(t *testing.T) {
	e := riskEnv(t)
	c := e.roleClient("viewer")
	e.seedRiskPayment("pay_plain", "")
	lacks(t, e.pageOK(c, "/dashboard/payments/pay_plain"), "risk-review", "risk-score")
}

func TestRiskScoreBands(t *testing.T) {
	for _, tc := range []struct {
		score       int
		label, tone string
	}{{0, "0 Low", "low"}, {39, "39 Low", "low"}, {40, "40 Medium", "medium"}, {74, "74 Medium", "medium"}, {75, "75 High", "high"}, {100, "100 High", "high"}} {
		label, tone := riskBand(tc.score)
		if label != tc.label || tone != tc.tone {
			t.Errorf("riskBand(%d) = %q, %q", tc.score, label, tone)
		}
	}
}

func TestRiskWithoutAScoreIsPendingUntilTheAttemptFails(t *testing.T) {
	risk := `{"cvv_check":"match"}`
	if v := paymentRiskView(store.Attempt{Status: "captured", RiskDetails: risk}); v == nil || v.Score != "Pending" {
		t.Fatalf("captured: %+v", v)
	}
	if v := paymentRiskView(store.Attempt{Status: "failed", RiskDetails: risk}); v == nil || v.Score != "Not scored" {
		t.Fatalf("failed: %+v", v)
	}
}

func TestPaymentsListShowsRiskBadge(t *testing.T) {
	e := riskEnv(t)
	c := e.roleClient("viewer")
	e.seedRiskPayment("pay_high", `{"score":88}`)
	e.seedRiskPayment("pay_none", "")
	body := e.pageOK(c, "/dashboard/payments")
	has(t, body, `risk-score risk-high`, "88 High")
	lacks(t, body, "risk-low", "risk-medium", "risk-review-pay_none")
}

func TestPaymentsListBadgeOpensThatPaymentsRiskModal(t *testing.T) {
	e := riskEnv(t)
	c := e.roleClient("viewer")
	e.seedRiskPayment("pay_a", `{"score":19,"ip_address":"203.0.113.24"}`)
	e.seedRiskPayment("pay_b", `{"score":88,"ip_address":"203.0.113.25"}`)
	body := e.pageOK(c, "/dashboard/payments")
	has(t, body, `commandfor="risk-review-pay_a"`, `<dialog id="risk-review-pay_a"`, `aria-labelledby="risk-review-pay_a-title"`,
		`commandfor="risk-review-pay_b"`, `<dialog id="risk-review-pay_b"`, "203.0.113.24", "203.0.113.25")
}

func riskEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, storetest.SQLiteMigrated(t))
	err := e.db.Write(ctx, func(tx store.Tx) error {
		return store.InsertProviderAccountInfo(ctx, tx, store.ProviderAccountInfo{ID: "pa_whop", OrgID: store.DefaultOrgID, Mode: "live",
			Connector: "whop", Name: "Whop main", Status: "active", Currencies: []string{"USD"}, Purposes: []string{"one_time"}}, e.clk.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
