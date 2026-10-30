package housekeeping_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/domains"
	"github.com/velixio/paymux/internal/housekeeping"
	"github.com/velixio/paymux/internal/store"
)

// txtZone is a fake DNS: TXT records by name. It never touches the network.
type txtZone map[string][]string

func (z txtZone) lookup(_ context.Context, name string) ([]string, error) {
	r, ok := z[name]
	if !ok {
		return nil, errors.New("no such host")
	}
	return r, nil
}

// provedTXT is a resolver that has d's challenge record.
func provedTXT(d store.Domain) housekeeping.TXTFunc {
	return txtZone{domains.ChallengeName(d.Host): {domains.ChallengeValue(d.Token)}}.lookup
}

func okFetch(token string) *fetcher {
	return &fetcher{reply: func(string) (int, string, error) { return 200, token, nil }}
}

func (e *env) checker(f *fetcher, z txtZone) *housekeeping.DomainChecker {
	return housekeeping.NewDomainChecker(housekeeping.DomainOptions{DB: e.db, Alerts: e.alerts, Fetch: f.fetch, LookupTXT: z.lookup, Now: e.clk.Now})
}

func (e *env) lookupHost(t *testing.T, host string) domains.Domain {
	t.Helper()
	d, ok, err := e.core.LookupHost(ctx, host)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	return d
}

func (e *env) makeActive(t *testing.T, d store.Domain) {
	t.Helper()
	if err := e.db.Write(ctx, func(tx store.Tx) error {
		return store.UpdateDomainCheck(ctx, tx, d.ID, domains.StatusActive, "", e.clk.Now())
	}); err != nil {
		t.Fatal(err)
	}
}

// B-8: a hostname whose DNS already points at the hub serves the token over
// https, so the fetch alone proves nothing. Without the TXT record the domain
// stays pending and nothing is even fetched.
func TestDomainWithoutTXTStaysPending(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		d := e.domain("pay.shop.example.org")
		f := okFetch(d.Token)
		if err := e.checker(f, txtZone{}).Run(ctx); err != nil {
			t.Fatal(err)
		}
		got := e.get(d.Host)
		if got.Status != "pending" || !strings.Contains(got.LastError, "_paymux-challenge.pay.shop.example.org") || !got.TXTVerifiedAt.IsZero() {
			t.Fatalf("domain %+v", got)
		}
		if len(f.urls) != 0 {
			t.Errorf("fetched %v before the TXT record was proven", f.urls)
		}
	})
}

func TestDomainWithWrongTXTTokenStaysPending(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		d := e.domain("pay.shop.example.org")
		other := strings.Repeat("0", len(d.Token))
		for name, recs := range map[string][]string{
			"wrong token":  {domains.ChallengeValue(other)},
			"bare token":   {d.Token},
			"longer token": {"v=spf1 -all", "paymux-verify=" + d.Token + "x"},
		} {
			f := okFetch(d.Token)
			if err := e.checker(f, txtZone{domains.ChallengeName(d.Host): recs}).Run(ctx); err != nil {
				t.Fatal(err)
			}
			if got := e.get(d.Host); got.Status != "pending" || !got.TXTVerifiedAt.IsZero() || len(f.urls) != 0 {
				t.Errorf("%s: %+v fetched %v", name, got, f.urls)
			}
		}
	})
}

func TestDomainActivatesWithTXTAndFetch(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		d := e.domain("pay.shop.example.org")
		// The TXT proof is what lets TLSAllow give it a certificate; with the
		// fetch failing (no certificate yet, say) it is recorded all the same.
		bad := &fetcher{reply: func(string) (int, string, error) { return 0, "", errors.New("tls: handshake failure") }}
		z := txtZone{domains.ChallengeName(d.Host): {"unrelated", domains.ChallengeValue(d.Token)}}
		if err := e.checker(bad, z).Run(ctx); err != nil {
			t.Fatal(err)
		}
		if got := e.get(d.Host); got.Status != "pending" || got.TXTVerifiedAt.IsZero() {
			t.Fatalf("after the TXT proof %+v", got)
		}
		if lk := e.lookupHost(t, d.Host); !lk.TXTVerified || lk.Status != domains.StatusPending {
			t.Errorf("LookupHost %+v", lk)
		}
		if err := e.checker(okFetch(d.Token), z).Run(ctx); err != nil {
			t.Fatal(err)
		}
		if got := e.get(d.Host); got.Status != "active" || got.LastError != "" {
			t.Fatalf("domain %+v", got)
		}
	})
}

func TestDomainTXTRecordsAreBounded(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		d := e.domain("pay.shop.example.org")
		var recs []string
		for i := 0; i < 50; i++ {
			recs = append(recs, "junk")
		}
		recs = append(recs, domains.ChallengeValue(d.Token)) // beyond the cap
		if err := e.checker(okFetch(d.Token), txtZone{domains.ChallengeName(d.Host): recs}).Run(ctx); err != nil {
			t.Fatal(err)
		}
		if e.get(d.Host).Status != "pending" {
			t.Error("a record beyond the cap counted")
		}
		long := strings.Repeat("a", 5000) + domains.ChallengeValue(d.Token)
		if err := e.checker(okFetch(d.Token), txtZone{domains.ChallengeName(d.Host): {long}}).Run(ctx); err != nil {
			t.Fatal(err)
		}
		if e.get(d.Host).Status != "pending" {
			t.Error("an oversized record counted")
		}
	})
}

// An active domain from before the upgrade has no TXT record yet. It stays
// active for 7 days, then goes back to pending (not deleted) with one alert.
func TestActiveDomainWithoutTXTGetsGraceThenPending(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		d := e.domain("pay.shop.example.org")
		e.makeActive(t, d)
		e.clk.Add(48 * time.Hour)
		f := okFetch(d.Token)
		dc := e.checker(f, txtZone{})
		_ = dc.Run(ctx)
		got := e.get(d.Host)
		if got.Status != "active" || got.GraceStartedAt.IsZero() || len(e.alerts.kinds()) != 0 {
			t.Fatalf("first check: %+v alerts %v", got, e.alerts.kinds())
		}
		start := got.GraceStartedAt
		e.clk.Add(6*24*time.Hour + 23*time.Hour)
		_ = dc.Run(ctx)
		if got := e.get(d.Host); got.Status != "active" || !got.GraceStartedAt.Equal(start) {
			t.Fatalf("inside the grace period: %+v", got)
		}
		e.clk.Add(2 * time.Hour)
		_ = dc.Run(ctx)
		got = e.get(d.Host)
		if got.Status != "pending" || !got.TXTVerifiedAt.IsZero() || !got.GraceStartedAt.IsZero() || got.LastError == "" ||
			!slices.Equal(e.alerts.kinds(), []string{"domain.unverified"}) {
			t.Fatalf("after the grace period: %+v alerts %v", got, e.alerts.kinds())
		}
		if lk := e.lookupHost(t, d.Host); lk.TXTVerified {
			t.Error("a demoted domain may still get a certificate")
		}
		// It is not failed the moment it turns pending: it gets the usual
		// activation period, and the operator can fix the record.
		_ = dc.Run(ctx)
		if e.get(d.Host).Status != "pending" || len(e.alerts.kinds()) != 1 {
			t.Errorf("demoted domain: %+v alerts %v", e.get(d.Host), e.alerts.kinds())
		}
		z := txtZone{domains.ChallengeName(d.Host): {domains.ChallengeValue(d.Token)}}
		_ = e.checker(f, z).Run(ctx)
		if e.get(d.Host).Status != "active" {
			t.Errorf("after adding the record: %+v", e.get(d.Host))
		}
	})
}

// The record turning up inside the grace period ends it, and an active domain
// whose record is proven is not fetched or checked again.
func TestActiveDomainWithTXTNeedsNoGrace(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		d := e.domain("pay.shop.example.org")
		e.makeActive(t, d)
		e.clk.Add(48 * time.Hour)
		f := okFetch(d.Token)
		_ = e.checker(f, txtZone{}).Run(ctx)
		if e.get(d.Host).GraceStartedAt.IsZero() {
			t.Fatal("no grace started")
		}
		e.clk.Add(2 * time.Hour)
		z := txtZone{domains.ChallengeName(d.Host): {domains.ChallengeValue(d.Token)}}
		_ = e.checker(f, z).Run(ctx)
		got := e.get(d.Host)
		if got.Status != "active" || !got.GraceStartedAt.IsZero() || got.TXTVerifiedAt.IsZero() {
			t.Fatalf("%+v", got)
		}
		if len(f.urls) != 0 {
			t.Errorf("an active domain was fetched: %v", f.urls)
		}
	})
}
