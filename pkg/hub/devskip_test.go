package hub

import (
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/logging"
)

// Config.Load already refuses PAYMUX_DEV_SKIP_2FA outside development, but a
// program that builds the Config itself must not be able to start a production
// hub without two-factor sign-in either.
func TestDevSkip2FARefusedOutsideDevelopment(t *testing.T) {
	for _, env := range []config.Env{config.EnvProduction, "", "staging"} {
		dir := t.TempDir()
		cfg := testConfig(t, "sqlite://"+dir+"/paymux.db", dir)
		cfg.Env, cfg.DevSkip2FA = env, true
		_, err := New(cfg, WithLogger(logging.New(&syncBuf{}, -4)))
		if err == nil {
			t.Errorf("env %q: the hub was built with 2FA disabled", env)
		} else if !strings.Contains(err.Error(), "PAYMUX_DEV_SKIP_2FA") {
			t.Errorf("env %q: unhelpful error: %v", env, err)
		}
	}
}
