package main

import (
	"os"
	"strings"
	"testing"

	"github.com/paymux/paymux/pkg/connector"
)

func infos() []connector.Info {
	return []connector.Info{
		{Name: "zeta", DisplayName: "Zeta Pay", Capabilities: []connector.Capability{connector.CapRefund, connector.CapList, connector.CapDisputeRead},
			Currencies: []string{"INR", "USD"}, Methods: []string{"card", "upi"}, SubscriptionModes: []connector.BillingMode{connector.ProviderManaged}},
		{Name: "alpha", DisplayName: "Alpha | Pay", MerchantOfRecord: true, Capabilities: []connector.Capability{connector.CapRefund, connector.CapMandateManager},
			Currencies: []string{"INR"}, SubscriptionModes: []connector.BillingMode{connector.MerchantTriggered},
			MandateLimits: []connector.MandateLimit{{Method: "upi", Currency: "INR", MaxAmount: 1500000}}, PreDebitPerAttempt: true, PreDebitMinHours: 48},
		{Name: "remote", DisplayName: "Remote connector (HTTP)"},
	}
}

func row(t *testing.T, out, name string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "| ["+name+"]") || strings.HasPrefix(line, "| "+name+" ") {
			cells := strings.Split(strings.Trim(line, "| "), " | ")
			return cells
		}
	}
	t.Fatalf("no row for %q in:\n%s", name, out)
	return nil
}

func TestRenderListsEveryConnectorWithItsCapabilities(t *testing.T) {
	out := Render(infos())
	head := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "| Connector ") {
			head = line
			break
		}
	}
	if head == "" {
		t.Fatalf("no capability table:\n%s", out)
	}
	cols := strings.Split(strings.Trim(head, "| "), " | ")
	idx := func(label string) int {
		for i, c := range cols {
			if strings.Contains(c, label) {
				return i
			}
		}
		t.Fatalf("no column %q in %v", label, cols)
		return -1
	}
	zeta := row(t, out, "zeta")
	if zeta[idx("Refunds")] != "yes" || zeta[idx("Disputes: read")] != "yes" || zeta[idx("Disputes: respond")] != "-" || zeta[idx("Mandates")] != "-" {
		t.Errorf("zeta row %v", zeta)
	}
	alpha := row(t, out, "alpha")
	if alpha[idx("Mandates")] != "yes" || alpha[idx("List")] != "-" {
		t.Errorf("alpha row %v", alpha)
	}
	if !strings.Contains(out, `Alpha \| Pay`) {
		t.Error("a pipe in a name must be escaped so the table stays intact")
	}
	// The remote connector's capabilities are whatever the service reports.
	remote := row(t, out, "remote")
	if remote[idx("Refunds")] != "per service" {
		t.Errorf("remote row %v", remote)
	}
}

func TestRenderIsSortedAndStable(t *testing.T) {
	a := Render(infos())
	rev := infos()
	rev[0], rev[2] = rev[2], rev[0]
	if b := Render(rev); a != b {
		t.Error("the output depends on the order of the registry")
	}
	if i, j := strings.Index(a, "| [alpha]"), strings.Index(a, "| [zeta]"); i < 0 || j < 0 || i > j {
		t.Errorf("rows should be in name order (alpha at %d, zeta at %d)", i, j)
	}
}

func TestRenderDetailsTable(t *testing.T) {
	out := Render(infos())
	for _, want := range []string{"INR, USD", "card, upi", "provider_managed", "merchant_triggered", "upi INR up to 15000.00", "pre-debit notice per attempt", "notice at least 48 hours before the debit"} {
		if !strings.Contains(out, want) {
			t.Errorf("details lack %q:\n%s", want, out)
		}
	}
	if alpha := row(t, out, "alpha"); len(alpha) == 0 {
		t.Fatal("no alpha row")
	}
	if !strings.Contains(out, "merchant of record") {
		t.Error("merchant-of-record connectors should be marked")
	}
}

func TestACapabilityNobodyListedStillGetsAColumn(t *testing.T) {
	in := append(infos(), connector.Info{Name: "novel", DisplayName: "Novel", Capabilities: []connector.Capability{"teleport"}})
	out := Render(in)
	if !strings.Contains(out, "teleport") {
		t.Fatalf("an unknown capability must not be dropped silently:\n%s", out)
	}
}

func TestTheCommittedFileMatchesTheRegistry(t *testing.T) {
	want, err := os.ReadFile("../../docs/capabilities.md")
	if err != nil {
		t.Fatalf("docs/capabilities.md is missing; run `go run ./scripts/capmatrix -w`: %v", err)
	}
	if got := Render(connector.List()); got != string(want) {
		t.Fatalf("docs/capabilities.md is stale; run `go run ./scripts/capmatrix -w`\n--- generated ---\n%s", got)
	}
	// Every shipped connector is in it.
	for _, name := range []string{"payu", "razorpay", "whop", "zoho", "remote"} {
		if !strings.Contains(string(want), "["+name+"]") {
			t.Errorf("%s is missing from the matrix", name)
		}
	}
}
