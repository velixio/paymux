package housekeeping_test

import (
	"slices"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/domains"
)

// A domain whose TXT proof was stored is looked up again: when the record is
// removed later (domain sold, zone changed) the proof is dropped, the grace
// period starts and the domain goes back to pending after it.
func TestProvenActiveDomainWhoseRecordIsRemovedIsDemoted(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		d := e.domain("pay.shop.example.org")
		z := txtZone{domains.ChallengeName(d.Host): {domains.ChallengeValue(d.Token)}}
		f := okFetch(d.Token)
		if err := e.checker(f, z).Run(ctx); err != nil {
			t.Fatal(err)
		}
		if got := e.get(d.Host); got.Status != "active" || got.TXTVerifiedAt.IsZero() {
			t.Fatalf("activation: %+v", got)
		}
		// Rechecked at most hourly.
		delete(z, domains.ChallengeName(d.Host))
		_ = e.checker(f, z).Run(ctx)
		if got := e.get(d.Host); got.TXTVerifiedAt.IsZero() || !got.GraceStartedAt.IsZero() {
			t.Fatalf("rechecked within the hour: %+v", got)
		}
		e.clk.Add(2 * time.Hour)
		dc := e.checker(f, z)
		_ = dc.Run(ctx)
		got := e.get(d.Host)
		if got.Status != "active" || !got.TXTVerifiedAt.IsZero() || got.GraceStartedAt.IsZero() || len(e.alerts.kinds()) != 0 {
			t.Fatalf("record removed: %+v alerts %v", got, e.alerts.kinds())
		}
		if lk := e.lookupHost(t, d.Host); lk.TXTVerified {
			t.Error("a removed record still allows a certificate")
		}
		e.clk.Add(domainTXTGraceForTest + time.Hour)
		_ = dc.Run(ctx)
		got = e.get(d.Host)
		if got.Status != "pending" || !slices.Equal(e.alerts.kinds(), []string{"domain.unverified"}) {
			t.Fatalf("after the grace period: %+v alerts %v", got, e.alerts.kinds())
		}
	})
}

const domainTXTGraceForTest = 7 * 24 * time.Hour
