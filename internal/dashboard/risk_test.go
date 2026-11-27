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

func TestPaymentPageShowsRiskReviewInItsPanel(t *testing.T) {
	e := riskEnv(t)
	c := e.roleClient("viewer")
	e.seedRiskPayment("pay_risk", `{"score":19,"ip_address":"203.0.113.24","city":"Testville","region":"Sample State","country":"US",
		"address_check":"no_match","postal_check":"match","cvv_check":"match","three_d_secure":false,"authorization_code":"A1B2C3"}`)
	body := e.pageOK(c, "/dashboard/payments/pay_risk")
	has(t, body, `pt-risk"`, "19 Low", "Testville, Sample State, US", "203.0.113.24",
		"Address", "ZIP / postal", "No match", "Match", "Not verified", "A1B2C3")
	lacks(t, regexpFind(t, `(?s)(pt-risk".*?</div>)`, body), "19 Low")
}

func TestPaymentPageHidesRiskReviewWithoutData(t *testing.T) {
	e := riskEnv(t)
	c := e.roleClient("viewer")
	e.seedRiskPayment("pay_plain", "")
	body := e.pageOK(c, "/dashboard/payments/pay_plain")
	lacks(t, body, `pt-risk"`, "risk-score")
	has(t, body, "Not provided")
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
		`commandfor="risk-review-pay_b"`, `<dialog id="risk-review-pay_b"`)
	for id, ip := range map[string]string{"pay_a": "203.0.113.24", "pay_b": "203.0.113.25"} {
		_, after, _ := strings.Cut(body, `<dialog id="risk-review-`+id+`"`)
		dialog, _, _ := strings.Cut(after, "</dialog>")
		has(t, dialog, ip)
	}
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

func TestRiskTagsShowOnlyPassedChecksAndTheIPCountry(t *testing.T) {
	v := paymentRiskView(store.Attempt{Status: "captured", RiskDetails: `{"score":19,"country":"CO",
		"address_check":"no_match","postal_check":"match","cvv_check":"match","three_d_secure":true}`})
	var got []string
	for _, tag := range v.Tags {
		got = append(got, tag.Label+"|"+tag.Title)
	}
	want := []string{"CVV|CVV matched", "AVS|ZIP or postal code matched", "3DS|3D Secure verified"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tags %v, want %v", got, want)
	}
	if v.Flag != "🇨🇴" || v.FlagTitle != "Buyer IP in Colombia" {
		t.Fatalf("flag %q %q", v.Flag, v.FlagTitle)
	}

	v = paymentRiskView(store.Attempt{Status: "captured", RiskDetails: `{"score":40,"country":"zz9",
		"address_check":"unavailable","postal_check":"unavailable","cvv_check":"unchecked","three_d_secure":false}`})
	if len(v.Tags) != 0 || v.Flag != "" {
		t.Fatalf("nothing verified and no valid country: %+v %q", v.Tags, v.Flag)
	}
	v = paymentRiskView(store.Attempt{Status: "captured", RiskDetails: `{"address_check":"match"}`})
	if len(v.Tags) != 1 || v.Tags[0].Title != "Address matched" {
		t.Fatalf("address match: %+v", v.Tags)
	}
}

func TestPaymentsListAndPageShowRiskTags(t *testing.T) {
	e := riskEnv(t)
	c := e.roleClient("viewer")
	e.seedRiskPayment("pay_tags", `{"score":19,"country":"US","cvv_check":"match","address_check":"match"}`)
	row := regexpFind(t, `(?s)(<tr>\s*<td><a href="/dashboard/payments/pay_tags".*?</tr>)`, e.pageOK(c, "/dashboard/payments"))
	has(t, row, `class="risk-tag" title="CVV matched">CVV</span>`, `class="risk-tag" title="Address matched">AVS</span>`,
		`class="risk-flag" title="Buyer IP in United States" aria-label="Buyer IP in United States">🇺🇸</span>`)
	lacks(t, row, ">3DS<")
	has(t, e.pageOK(c, "/dashboard/payments/pay_tags"), `title="CVV matched">CVV</span>`, "Buyer IP in United States")
}

func TestPaymentPageShowsCheckoutIPs(t *testing.T) {
	e := riskEnv(t)
	c := e.roleClient("viewer")
	e.seedRiskPayment("pay_net", `{"score":30,"ip_address":"73.30.247.196","country":"IQ"}`)
	err := e.db.Write(ctx, func(tx store.Tx) error {
		if err := store.NoteCheckoutIP(ctx, tx, "pay_net", "170.62.103.12", store.CheckoutIPApp, e.clk.Now()); err != nil {
			return err
		}
		return store.PutIPLookup(ctx, tx, "170.62.103.12", `{"country":"US","city":"Dallas","region":"Texas","org":"EXAMPLE-VPN","vpn":true}`, e.clk.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	body := e.pageOK(c, "/dashboard/payments/pay_net")
	panel := regexpFind(t, `(?s)(pt-network".*?</div>)`, body)
	has(t, panel, "Checkout IPs", `<span class="net-ip">170.62.103.12</span>`, "🇺🇸", `risk-tag risk-tag-warn"`, ">VPN<",
		"Dallas, Texas, United States · EXAMPLE-VPN", "sent by your app", `<span class="net-ip">73.30.247.196</span>`, ">Not a checkout IP<")

	// No IP opened the checkout: no panel.
	e.seedRiskPayment("pay_nonet", `{"score":30,"ip_address":"73.30.247.196"}`)
	lacks(t, e.pageOK(c, "/dashboard/payments/pay_nonet"), `pt-network"`)
}
