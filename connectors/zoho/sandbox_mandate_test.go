//go:build sandbox

package zoho

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
)

// Mandates against Zoho's sandbox (ZOHO_TEST_SANDBOX=1 or "sandbox": true in
// ZOHO_TEST_CONFIG_FILE, with a connected account: a refresh token granted with
// ZohoPaySandbox.* scopes including customers). Without a refresh token they
// skip. They create a customer and an enrollment session (nothing is debited);
// completing the registration needs the widget in a browser (UPI amounts up to
// Rs 500 succeed in the sandbox), after which ZOHO_TEST_MANDATE_REF names the
// registration so the rest can run.
func TestSandboxMandateEnrollmentSession(t *testing.T) {
	z := sandboxConnector(t)
	if z.cfg.RefreshToken == "" {
		t.Skip("the sandbox account is not connected yet (no refresh token)")
	}
	ctx := context.Background()
	ref := "mdt" + sandboxRef()
	res, err := z.CreateMandateCheckout(ctx, connector.MandateRequest{MandateID: "mdt_sbx", ProviderRef: ref, Amount: 100, MaxAmount: 150000, Currency: "INR", Method: "upi",
		ValidUntil: time.Now().Add(365 * 24 * time.Hour).UnixMilli(), Customer: connector.Customer{Name: "Sandbox Customer", Email: "sbx@example.com", Phone: "9876543210"}})
	if err != nil {
		t.Fatalf("CreateMandateCheckout: %v", err)
	}
	if res.NextAction.Options["payments_session_id"] == "" {
		t.Fatalf("%+v", res)
	}
	s, err := z.FetchMandate(ctx, connector.MandateRef{ProviderMandateID: ref})
	if err != nil || s.Status != "pending" {
		t.Fatalf("a registration nobody authorised: %+v %v", s, err)
	}
	if err := z.RevokeMandate(ctx, connector.MandateRef{ProviderMandateID: "1"}); !errors.Is(err, connector.ErrUnsupported) {
		t.Errorf("revoke: %v", err)
	}
}

// With a mandate authorised in the browser (ZOHO_TEST_MANDATE_ID is Zoho's mandate
// id): read it, announce a debit two days ahead, then execute one (UPI amounts
// up to Rs 500 succeed in the sandbox; NPCI's closed windows are respected, so
// the debit step skips in them).
func TestSandboxMandateDebit(t *testing.T) {
	z := sandboxConnector(t)
	id := os.Getenv("ZOHO_TEST_MANDATE_ID")
	if z.cfg.RefreshToken == "" || id == "" {
		t.Skip("needs a connected sandbox account and ZOHO_TEST_MANDATE_ID (a mandate authorised in the widget)")
	}
	ctx := context.Background()
	m := connector.MandateRef{ProviderMandateID: id}
	s, err := z.FetchMandate(ctx, m)
	if err != nil || s.Status != "active" {
		t.Fatalf("FetchMandate: %+v %v", s, err)
	}
	ref := "deb" + sandboxRef()
	if err := z.NotifyPreDebit(ctx, connector.PreDebitRequest{Mandate: m, ProviderRef: ref, Amount: 20000, Currency: "INR", DebitDate: time.Now().Add(48 * time.Hour).UnixMilli()}); err != nil {
		t.Fatalf("NotifyPreDebit: %v", err)
	}
	p, err := z.ChargeMandate(ctx, connector.ChargeRequest{Mandate: m, ProviderRef: ref, Amount: 20000, Currency: "INR"})
	if errors.Is(err, connector.ErrTemporary) {
		t.Skipf("NPCI's closed window: %v", err)
	}
	if err != nil || p.ProviderRef != ref || p.ProviderOrderID == "" {
		t.Fatalf("ChargeMandate: %+v %v", p, err)
	}
	again, err := z.ChargeMandate(ctx, connector.ChargeRequest{Mandate: m, ProviderRef: ref, Amount: 20000, Currency: "INR"})
	if err != nil || again.ProviderOrderID != p.ProviderOrderID {
		t.Errorf("a repeated charge must return the first debit: %+v %v", again, err)
	}
}
