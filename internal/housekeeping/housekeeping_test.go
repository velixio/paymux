package housekeeping_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/housekeeping"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var (
	ctx = context.Background()
	org = core.OrgScope{OrgID: store.DefaultOrgID}
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type alertLog struct {
	mu     sync.Mutex
	alerts []notify.Alert
}

func (a *alertLog) Raise(_ context.Context, al notify.Alert) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts = append(a.alerts, al)
	return nil
}

func (a *alertLog) kinds() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, x := range a.alerts {
		out = append(out, x.Kind)
	}
	return out
}

type env struct {
	t      *testing.T
	db     *store.DB
	core   *core.Service
	clk    *clock
	alerts *alertLog
	q      *jobs.Queue
}

func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	e := &env{t: t, db: db, clk: clk, alerts: &alertLog{}, q: jobs.New(db, jobs.Options{Clock: clk.Now})}
	set := settings.New(db)
	e.core = core.NewService(core.Options{DB: db, Settings: set, Jobs: e.q, Now: clk.Now})
	if _, err := core.Bootstrap(ctx, db, set); err != nil {
		t.Fatal(err)
	}
	if err := db.Write(ctx, func(tx store.Tx) error {
		return store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: store.DefaultOrgID, Name: "A", Slug: "a", CreatedAt: clk.t})
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func each(t *testing.T, fn func(t *testing.T, e *env)) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) { fn(t, newEnv(t, db)) })
}

func (e *env) domain(host string) store.Domain {
	d, err := e.core.AddDomain(ctx, audit.System, org, "app_1", host)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *env) get(host string) store.Domain {
	var d store.Domain
	_ = e.db.Read(ctx, func(q store.Querier) (err error) { d, err = store.GetDomainByHost(ctx, q, host); return })
	return d
}

type fetcher struct {
	mu    sync.Mutex
	urls  []string
	reply func(url string) (int, string, error)
}

func (f *fetcher) fetch(_ context.Context, url string) (int, string, error) {
	f.mu.Lock()
	f.urls = append(f.urls, url)
	f.mu.Unlock()
	return f.reply(url)
}

func TestDomainActivation(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		d := e.domain("pay.shop.example.org")
		f := &fetcher{reply: func(string) (int, string, error) { return 200, d.Token + "\n", nil }}
		dc := housekeeping.NewDomainChecker(housekeeping.DomainOptions{DB: e.db, Alerts: e.alerts, Fetch: f.fetch, LookupTXT: provedTXT(d), Now: e.clk.Now})
		if err := dc.Run(ctx); err != nil {
			t.Fatal(err)
		}
		got := e.get("pay.shop.example.org")
		if got.Status != "active" || got.LastError != "" || got.LastCheckAt.IsZero() {
			t.Fatalf("domain %+v", got)
		}
		if want := "https://pay.shop.example.org/.well-known/paymux/" + d.Token; len(f.urls) != 1 || f.urls[0] != want {
			t.Errorf("fetched %v, want %s", f.urls, want)
		}
		// Active domains are not checked again.
		_ = dc.Run(ctx)
		if len(f.urls) != 1 {
			t.Error("an active domain was re-checked")
		}
	})
}

func TestDomainActivationFailureModes(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		d := e.domain("pay.shop.example.org")
		cases := map[string]func(string) (int, string, error){
			"wrong body":  func(string) (int, string, error) { return 200, "something else", nil },
			"not found":   func(string) (int, string, error) { return 404, "", nil },
			"unreachable": func(string) (int, string, error) { return 0, "", errors.New("no such host") },
		}
		for name, reply := range cases {
			f := &fetcher{reply: reply}
			dc := housekeeping.NewDomainChecker(housekeeping.DomainOptions{DB: e.db, Alerts: e.alerts, Fetch: f.fetch, LookupTXT: provedTXT(d), Now: e.clk.Now})
			if err := dc.Run(ctx); err != nil {
				t.Fatal(err)
			}
			got := e.get(d.Host)
			if got.Status != "pending" || got.LastError == "" || got.LastCheckAt.IsZero() {
				t.Errorf("%s: %+v (a young domain keeps trying)", name, got)
			}
		}
		if len(e.alerts.kinds()) != 0 {
			t.Errorf("alerts too early: %v", e.alerts.kinds())
		}
		// After 24 hours without success the domain is failed, with one alert.
		e.clk.Add(25 * time.Hour)
		f := &fetcher{reply: cases["not found"]}
		dc := housekeeping.NewDomainChecker(housekeeping.DomainOptions{DB: e.db, Alerts: e.alerts, Fetch: f.fetch, LookupTXT: provedTXT(d), Now: e.clk.Now})
		_ = dc.Run(ctx)
		got := e.get(d.Host)
		if got.Status != "failed" || !slices.Equal(e.alerts.kinds(), []string{"domain.failed"}) {
			t.Fatalf("domain %+v alerts %v", got, e.alerts.kinds())
		}
		// Failed domains are left alone until an operator rechecks them.
		n := len(f.urls)
		_ = dc.Run(ctx)
		if len(f.urls) != n || len(e.alerts.kinds()) != 1 {
			t.Error("a failed domain was retried or re-alerted")
		}
		if err := e.core.RecheckDomain(ctx, audit.System, org, got.ID); err != nil {
			t.Fatal(err)
		}
		if got := e.get(d.Host); got.Status != "pending" || got.LastError != "" {
			t.Errorf("after recheck %+v", got)
		}
		ok := &fetcher{reply: func(string) (int, string, error) { return 200, d.Token, nil }}
		dc = housekeeping.NewDomainChecker(housekeeping.DomainOptions{DB: e.db, Alerts: e.alerts, Fetch: ok.fetch, LookupTXT: provedTXT(d), Now: e.clk.Now})
		_ = dc.Run(ctx)
		if e.get(d.Host).Status != "active" {
			t.Error("recheck did not lead to activation")
		}
		if err := e.core.RecheckDomain(ctx, audit.System, org, got.ID); !errors.Is(err, core.ErrInvalidState) {
			t.Errorf("recheck of an active domain: %v", err)
		}
		if err := e.core.RecheckDomain(ctx, audit.System, core.OrgScope{OrgID: "org_other"}, got.ID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("cross-org recheck: %v", err)
		}
	})
}

func TestVerifyDomainToken(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		d := e.domain("pay.shop.example.org")
		for name, c := range map[string]struct {
			host, token string
			want        bool
		}{
			"match":          {"pay.shop.example.org", d.Token, true},
			"host case/port": {"Pay.Shop.Example.org:443", d.Token, true},
			"wrong token":    {"pay.shop.example.org", "nope", false},
			"unknown host":   {"other.example.org", d.Token, false},
			"empty token":    {"pay.shop.example.org", "", false},
		} {
			if got := e.core.VerifyDomainToken(ctx, c.host, c.token); got != c.want {
				t.Errorf("%s: %v", name, got)
			}
		}
	})
}

func (e *env) attemptOn(account string, at time.Time) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		n := at.UnixNano()
		pid := "pay_" + at.Format("150405.000000000")
		if err := store.InsertPayment(ctx, tx, store.Payment{ID: pid, OrgID: store.DefaultOrgID, AppID: "app_1", Mode: "test", Kind: "one_time",
			Amount: 1, Currency: "INR", Status: "created", ClientSecretHash: "x", ExpiresAt: at, Version: 1, CreatedAt: at, UpdatedAt: at}); err != nil {
			return err
		}
		return store.InsertAttempt(ctx, tx, store.Attempt{ID: "att_" + at.Format("150405.000000000"), OrgID: store.DefaultOrgID, Mode: "test",
			PaymentID: pid, ProviderAccountID: account, ProviderRef: "REF" + at.Format("150405.000000000") + string(rune('A'+n%26)), Status: "created", CreatedAt: at, UpdatedAt: at})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) event(account, id string, verified bool, at time.Time) {
	e.t.Helper()
	row := store.ProviderEventRow{ID: "pev_" + id, ProviderAccountID: account, ProviderEventID: id, Headers: []byte(`{}`), Body: []byte("{}"), Verified: verified, ReceivedAt: at}
	err := e.db.Write(ctx, func(tx store.Tx) error { _, err := e.db.InsertProviderEvent(ctx, tx, row); return err })
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) account(id string) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		return store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: id, OrgID: store.DefaultOrgID, Mode: "test", Connector: "razorpay",
			Name: "main", Status: "active", ConfigEnc: []byte{1}, CreatedAt: e.clk.Now()})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func TestSilenceDetector(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.account("pa_silent")
		e.account("pa_ok")
		e.account("pa_idle")
		e.account("pa_forged")
		now := e.clk.Now()
		e.attemptOn("pa_silent", now.Add(-2*time.Hour))
		e.attemptOn("pa_ok", now.Add(-2*time.Hour+time.Second))
		e.event("pa_ok", "e1", true, now.Add(-time.Hour))
		e.attemptOn("pa_forged", now.Add(-3*time.Hour))
		e.event("pa_forged", "e2", false, now.Add(-time.Hour)) // unverified deliveries do not count
		// pa_idle: no attempts at all, nothing to expect.
		e.attemptOn("pa_idle", now.Add(-10*time.Hour)) // too old to matter

		sd := housekeeping.NewSilenceDetector(housekeeping.SilenceOptions{DB: e.db, Alerts: e.alerts, Now: e.clk.Now})
		if err := sd.Run(ctx); err != nil {
			t.Fatal(err)
		}
		var refs []string
		for _, a := range e.alerts.alerts {
			if a.Kind != "provider.silent" || a.Severity != notify.SeverityWarning {
				t.Errorf("alert %+v", a)
			}
			refs = append(refs, a.Refs["provider_account"])
		}
		slices.Sort(refs)
		if !slices.Equal(refs, []string{"pa_forged", "pa_silent"}) {
			t.Fatalf("silent accounts %v", refs)
		}
		// Repeated runs stay quiet until the cooldown passes.
		_ = sd.Run(ctx)
		if len(e.alerts.alerts) != 2 {
			t.Errorf("repeat alerts: %d", len(e.alerts.alerts))
		}
		e.clk.Add(7 * time.Hour)
		e.attemptOn("pa_silent", e.clk.Now().Add(-time.Hour))
		_ = sd.Run(ctx)
		if len(e.alerts.alerts) != 3 {
			t.Errorf("no re-alert after the cooldown: %d", len(e.alerts.alerts))
		}
	})
}

func TestRegisterInstallsJobs(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		hk := housekeeping.New(housekeeping.Options{Core: e.core,
			Domains: housekeeping.NewDomainChecker(housekeeping.DomainOptions{DB: e.db, Alerts: e.alerts, Fetch: (&fetcher{reply: func(string) (int, string, error) { return 404, "", nil }}).fetch, LookupTXT: func(context.Context, string) ([]string, error) { return nil, nil }, Now: e.clk.Now}),
			Silence: housekeeping.NewSilenceDetector(housekeeping.SilenceOptions{DB: e.db, Alerts: e.alerts, Now: e.clk.Now})})
		if err := hk.Register(e.q); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			e.clk.Add(31 * time.Minute)
			if _, err := e.q.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
		}
		var dead, done int
		_ = e.db.Read(ctx, func(q store.Querier) error {
			_ = q.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE state = 'dead'").Scan(&dead)
			return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE state = 'done' AND kind IN ('payments.expire','domains.check','provider.silence')").Scan(&done)
		})
		if dead != 0 || done == 0 {
			t.Errorf("dead=%d done=%d", dead, done)
		}
	})
}
