// Package housekeeping holds the small periodic jobs: expiring unpaid
// payments, activating custom domains, and the provider silence detector.
package housekeeping

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/domains"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

// Alerter raises ops alerts (*notify.Alerts).
type Alerter interface {
	Raise(ctx context.Context, al notify.Alert) error
}

// Job kinds.
const (
	JobExpire  = "payments.expire"
	JobDomains = "domains.check"
	JobSilence = "provider.silence"
)

// Options configures Service.
type Options struct {
	Core        *core.Service
	Domains     *DomainChecker
	Silence     *SilenceDetector
	ExpireBatch int // payments expired per run (default 200)
}

// Service registers the housekeeping jobs.
type Service struct{ o Options }

// New builds a Service.
func New(o Options) *Service {
	if o.ExpireBatch <= 0 {
		o.ExpireBatch = 200
	}
	return &Service{o: o}
}

// Register installs the jobs and their schedules on q.
func (s *Service) Register(q *jobs.Queue) error {
	q.Register(JobExpire, func(ctx context.Context, _ json.RawMessage) error {
		_, err := s.o.Core.ExpireDue(ctx, s.o.ExpireBatch)
		return err
	})
	q.Register(JobDomains, func(ctx context.Context, _ json.RawMessage) error { return s.o.Domains.Run(ctx) })
	q.Register(JobSilence, func(ctx context.Context, _ json.RawMessage) error { return s.o.Silence.Run(ctx) })
	for _, sc := range []jobs.Schedule{
		{Name: JobExpire, Kind: JobExpire, Every: time.Minute},
		{Name: JobDomains, Kind: JobDomains, Every: 5 * time.Minute},
		{Name: JobSilence, Kind: JobSilence, Every: 30 * time.Minute},
	} {
		if err := q.AddSchedule(sc); err != nil {
			return err
		}
	}
	return nil
}

// ---- custom domain activation ----

// DomainGrace is how long a domain may keep failing its check before it is
// marked failed.
const DomainGrace = 24 * time.Hour

// FetchFunc GETs a URL and returns the status and (limited) body.
type FetchFunc func(ctx context.Context, url string) (status int, body string, err error)

// DomainOptions configures a DomainChecker.
type DomainOptions struct {
	DB     *store.DB
	Alerts Alerter
	Fetch  FetchFunc // default: DefaultFetch(false)
	Logger *slog.Logger
	Now    func() time.Time
}

// DomainChecker activates pending custom domains by fetching
// https://<host>/.well-known/paymux/<token> over the public internet.
type DomainChecker struct{ o DomainOptions }

// NewDomainChecker builds a DomainChecker.
func NewDomainChecker(o DomainOptions) *DomainChecker {
	if o.Fetch == nil {
		o.Fetch = DefaultFetch(false)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &DomainChecker{o: o}
}

// Run checks every pending domain once. A domain whose check succeeds becomes
// active; one that has kept failing for DomainGrace since it was added becomes
// failed (with one alert) and is left alone until an operator rechecks it.
func (c *DomainChecker) Run(ctx context.Context) error {
	var pending []store.Domain
	if err := c.o.DB.Read(ctx, func(q store.Querier) (err error) {
		pending, err = store.ListDomainsByStatus(ctx, q, domains.StatusPending)
		return
	}); err != nil {
		return err
	}
	for _, d := range pending {
		now := c.o.Now().UTC()
		status, errText := domains.StatusPending, ""
		code, body, err := c.o.Fetch(ctx, "https://"+d.Host+"/.well-known/paymux/"+d.Token)
		switch {
		case err != nil:
			errText = "fetching the activation file: " + err.Error()
		case code != http.StatusOK:
			errText = fmt.Sprintf("the activation file returned HTTP %d", code)
		case strings.TrimSpace(body) != d.Token:
			errText = "the activation file does not contain the expected token"
		default:
			status = domains.StatusActive
		}
		if errText != "" && now.Sub(d.CreatedAt) >= DomainGrace {
			status = domains.StatusFailed
		}
		err = c.o.DB.Write(ctx, func(tx store.Tx) error { return store.UpdateDomainCheck(ctx, tx, d.ID, status, errText, now) })
		if err != nil {
			return err
		}
		if status == domains.StatusFailed && c.o.Alerts != nil {
			if aerr := c.o.Alerts.Raise(ctx, notify.Alert{Severity: notify.SeverityWarning, Kind: "domain.failed",
				Message: fmt.Sprintf("Custom domain %s could not be activated: %s. Fix its DNS and recheck it.", d.Host, errText),
				Refs:    map[string]string{"domain": d.ID, "app": d.AppID}}); aerr != nil {
				c.o.Logger.Warn("housekeeping: alert not delivered", "err", aerr)
			}
		}
	}
	return nil
}

// DefaultFetch returns a FetchFunc over the public internet: 10 second
// timeout, no redirects, at most 4 KiB of body. Unless allowPrivate, it
// refuses to connect to loopback, private and link-local addresses, since the
// host name is operator-supplied.
func DefaultFetch(allowPrivate bool) FetchFunc {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			// The connector guard's list (CGNAT, NAT64, benchmarking, reserved...),
			// so the three outbound guards agree.
			if ip := net.ParseIP(host); ip == nil || connector.IsPrivateIP(ip) {
				return errors.New("refusing to connect to a non-public address")
			}
			return nil
		}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(ctx context.Context, url string) (int, string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, "", err
		}
		res, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer res.Body.Close()
		b, err := io.ReadAll(io.LimitReader(res.Body, 4096))
		return res.StatusCode, string(b), err
	}
}

// ---- silence detector ----

// SilenceOptions configures a SilenceDetector.
type SilenceOptions struct {
	DB     *store.DB
	Alerts Alerter
	// Window is how far back to look (default 6h). Cooldown is how long an
	// account stays quiet after an alert (default 6h).
	Window, Cooldown time.Duration
	Logger           *slog.Logger
	Now              func() time.Time
}

// SilenceDetector raises an alert when a provider account created attempts in
// the window but received no verified webhook: providers disable failing
// webhooks (Razorpay after 24h, Zoho after 2 days, Whop after 72h).
type SilenceDetector struct {
	o     SilenceOptions
	mu    sync.Mutex
	alert map[string]time.Time
}

// NewSilenceDetector builds a SilenceDetector.
func NewSilenceDetector(o SilenceOptions) *SilenceDetector {
	if o.Window <= 0 {
		o.Window = 6 * time.Hour
	}
	if o.Cooldown <= 0 {
		o.Cooldown = 6 * time.Hour
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &SilenceDetector{o: o, alert: map[string]time.Time{}}
}

// Run checks every active account once. The cooldown is kept in memory, so a
// restart can repeat an alert at most once.
func (d *SilenceDetector) Run(ctx context.Context) error {
	var accounts []store.ProviderAccount
	if err := d.o.DB.Read(ctx, func(q store.Querier) (err error) { accounts, err = store.ListActiveProviderAccounts(ctx, q); return }); err != nil {
		return err
	}
	now := d.o.Now().UTC()
	since := now.Add(-d.o.Window)
	for _, pa := range accounts {
		var attempts, events int
		err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
			if attempts, err = store.CountAttemptsSince(ctx, q, pa.ID, since); err != nil {
				return err
			}
			events, err = store.CountVerifiedEventsSince(ctx, q, pa.ID, since)
			return
		})
		if err != nil {
			return err
		}
		if attempts == 0 || events > 0 {
			continue
		}
		d.mu.Lock()
		last, seen := d.alert[pa.ID]
		quiet := seen && now.Sub(last) < d.o.Cooldown
		if !quiet {
			d.alert[pa.ID] = now
		}
		d.mu.Unlock()
		if quiet || d.o.Alerts == nil {
			continue
		}
		err = d.o.Alerts.Raise(ctx, notify.Alert{Severity: notify.SeverityWarning, Kind: "provider.silent",
			Message: fmt.Sprintf("%s account %s created %d attempt(s) in the last %s but received no verified webhook. The provider may have disabled the webhook; check %s's dashboard.",
				pa.Connector, pa.Name, attempts, d.o.Window, pa.Connector),
			Refs: map[string]string{"provider_account": pa.ID}})
		if err != nil {
			d.mu.Lock()
			delete(d.alert, pa.ID) // delivery failed: try again next run
			d.mu.Unlock()
			d.o.Logger.Warn("housekeeping: silence alert not delivered", "account", pa.ID, "err", err)
		}
	}
	return nil
}
