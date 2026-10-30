//go:build sandbox

package zoho

import (
	"context"
	"errors"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

// The Test button's call against the real API, on whichever server the
// credentials belong to; the other setting must be named as the wrong server.
func TestSandboxConnectionTester(t *testing.T) {
	z := sandboxConnector(t)
	ctx := context.Background()
	err := z.TestConnection(ctx)
	t.Logf("as configured (sandbox=%v): %v", z.cfg.Sandbox, err)
	if err != nil {
		t.Fatalf("the credentials in the environment must pass on their own server: %v", err)
	}
	// The same credentials on the other server.
	z.cfg.Sandbox = !z.cfg.Sandbox
	z.api.cfg.Sandbox = z.cfg.Sandbox
	err = z.TestConnection(ctx)
	t.Logf("on the other server (sandbox=%v): %v", z.cfg.Sandbox, err)
	if !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("the wrong server must be refused with ErrInvalidCredentials: %v", err)
	}
}
