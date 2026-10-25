package housekeeping

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Audit 2026-10: the custom-domain activation fetch has its own dial guard
// (net.IP.IsPrivate and friends), not connector.IsPrivateIP. It lets carrier-
// grade NAT, NAT64, benchmarking and reserved ranges through: the dial is
// attempted instead of refused. A dashboard user adds a domain whose DNS points
// there; the stored error text (refused / timeout / TLS) is a probe oracle.
func TestAuditDomainFetchGuardRanges(t *testing.T) {
	fetch := DefaultFetch(false)
	for _, host := range []string{"100.64.0.1", "[64:ff9b::a00:1]", "198.18.0.1", "240.0.0.1"} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		_, _, err := fetch(ctx, "https://"+host+"/.well-known/paymux/x")
		cancel()
		if err == nil || !strings.Contains(err.Error(), "non-public") {
			t.Errorf("%s: dial was attempted, not refused by the guard: %v", host, err)
		}
	}
}
