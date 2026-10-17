package paymux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/inbound"
	"github.com/paymux/paymux/internal/logging"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func testConfig(t *testing.T, databaseURL, dataDir string) config.Config {
	t.Helper()
	base, _ := url.Parse("http://localhost:8080")
	return config.Config{
		Env:            config.EnvDevelopment,
		BaseURL:        base,
		Listen:         "127.0.0.1:0",
		InternalListen: "127.0.0.1:0",
		DataDir:        dataDir,
		DatabaseURL:    databaseURL,
		MasterKey:      make([]byte, 32),
		Location:       time.UTC,
	}
}

type running struct {
	app  *App
	logs *syncBuf
	done chan struct{} // closed when Start has returned
	err  error         // Start's result; valid after done is closed
	stop context.CancelFunc
}

func startApp(t *testing.T, cfg config.Config) *running {
	t.Helper()
	logs := &syncBuf{}
	app, err := New(cfg, WithLogger(logging.New(logs, -4)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{app: app, logs: logs, done: make(chan struct{}), stop: cancel}
	go func() { r.err = app.Start(ctx); close(r.done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(15 * time.Second):
		}
	})
	return r
}

func fetch(t *testing.T, addr, path string) (int, string) {
	t.Helper()
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitReady(t *testing.T, r *running) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-r.done:
			t.Fatalf("Start returned early: %v\nlogs: %s", r.err, r.logs)
		default:
		}
		if addr := r.app.Addr(); addr != "" {
			if code, _ := fetch(t, addr, "/readyz"); code == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("not ready within 10s; logs: %s", r.logs)
}

func shutdown(t *testing.T, r *running) {
	t.Helper()
	r.stop()
	select {
	case <-r.done:
		if r.err != nil {
			t.Fatalf("Start returned %v", r.err)
		}
	case <-time.After(11 * time.Second):
		t.Fatal("Start did not return within 11s")
	}
}

func TestServeBoots(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		dir := t.TempDir()
		cfg := testConfig(t, "sqlite://"+dir+"/paymux.db", dir)
		r := startApp(t, cfg)
		waitReady(t, r)
		checkMetrics(t, r, "sqlite")

		// A second instance on the same data dir must be refused.
		app2, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := app2.Start(context.Background()); !errors.Is(err, store.ErrLocked) {
			t.Fatalf("second instance: %v", err)
		}
		shutdown(t, r)
	})
	t.Run("postgres", func(t *testing.T) {
		cfg := testConfig(t, storetest.PostgresURL(t), t.TempDir())
		r := startApp(t, cfg)
		waitReady(t, r)
		checkMetrics(t, r, "postgres")
		shutdown(t, r)
	})
}

func checkMetrics(t *testing.T, r *running, dialect string) {
	t.Helper()
	code, body := fetch(t, r.app.InternalAddr(), "/metrics")
	if code != 200 {
		t.Fatalf("metrics %d", code)
	}
	for _, want := range []string{
		`paymux_build_info{version="` + Version + `",db="` + dialect + `"} 1`,
		"paymux_jobs_oldest_pending_seconds",
		"paymux_heartbeat_last_run_timestamp_seconds",
		`paymux_jobs_depth{state="pending"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
	if code, _ := fetch(t, r.app.InternalAddr(), "/debug/pprof/"); code != 200 {
		t.Errorf("pprof %d", code)
	}
}

func post(t *testing.T, addr, path, body string) int {
	t.Helper()
	resp, err := http.Post("http://"+addr+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestHooksRouteMounted(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, "sqlite://"+dir+"/paymux.db", dir)
	r := startApp(t, cfg)
	waitReady(t, r)
	// No connector lane yet: every account is unknown.
	if code := post(t, r.app.Addr(), "/hooks/pa_x", `{}`); code != 404 {
		t.Fatalf("POST /hooks/pa_x = %d, want 404", code)
	}
	if code, _ := fetch(t, r.app.Addr(), "/hooks/pa_x"); code == 200 {
		t.Fatal("GET must not be accepted on /hooks")
	}
	shutdown(t, r)
}

type okVerifier struct{}

func (okVerifier) Verify(http.Header, []byte) (string, error) { return "ev_1", nil }

func TestHooksUseInjectedVerifiers(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, "sqlite://"+dir+"/paymux.db", dir)
	logs := &syncBuf{}
	app, err := New(cfg, WithLogger(logging.New(logs, -4)),
		WithVerifiers(func(_ context.Context, id string) (inbound.Verifier, error) {
			if id == "pa_ok" {
				return okVerifier{}, nil
			}
			return nil, inbound.ErrUnknownAccount
		}))
	if err != nil {
		t.Fatal(err)
	}
	r := &running{app: app, logs: logs, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	go func() { r.err = app.Start(ctx); close(r.done) }()
	waitReady(t, r)
	if code := post(t, app.Addr(), "/hooks/pa_ok", `{"a":1}`); code != 200 {
		t.Fatalf("known account = %d", code)
	}
	if code := post(t, app.Addr(), "/hooks/pa_other", `{}`); code != 404 {
		t.Fatalf("unknown account = %d", code)
	}
	shutdown(t, r)
}

func TestStartupLogRedactsPassword(t *testing.T) {
	raw := storetest.PostgresURL(t)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	pw, ok := u.User.Password()
	if !ok || pw == "" {
		t.Skip("test database URL has no password")
	}
	r := startApp(t, testConfig(t, raw, t.TempDir()))
	waitReady(t, r)
	shutdown(t, r)
	logs := r.logs.String()
	// Match the credential form: a short password such as "paymux" also
	// appears legitimately in the database name.
	if strings.Contains(logs, ":"+pw+"@") || strings.Contains(logs, ":"+url.QueryEscape(pw)+"@") {
		t.Fatalf("log leaks password:\n%s", logs)
	}
	if !strings.Contains(logs, "xxxxx") {
		t.Fatalf("startup line should show redacted URL:\n%s", logs)
	}
}

func TestStartupLogFields(t *testing.T) {
	dir := t.TempDir()
	r := startApp(t, testConfig(t, "sqlite://"+dir+"/paymux.db", dir))
	waitReady(t, r)
	shutdown(t, r)
	logs := r.logs.String()
	for _, want := range []string{`"msg":"paymux starting"`, `"dialect":"sqlite"`, `"env":"development"`, `"version":"` + Version + `"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("startup log missing %s:\n%s", want, logs)
		}
	}
}

func TestDevSkipWarningLogged(t *testing.T) {
	for _, skip := range []bool{true, false} {
		dir := t.TempDir()
		cfg := testConfig(t, "sqlite://"+dir+"/paymux.db", dir)
		cfg.DevSkip2FA = skip
		r := startApp(t, cfg)
		waitReady(t, r)
		shutdown(t, r)
		has := strings.Contains(r.logs.String(), "2FA is disabled (development)")
		if has != skip {
			t.Errorf("skip=%v warning present=%v\n%s", skip, has, r.logs)
		}
		if skip && !strings.Contains(r.logs.String(), `"level":"WARN"`) {
			t.Error("warning not at WARN level")
		}
	}
}

func TestSchedulerHealthy(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	cases := []struct {
		name    string
		now     time.Time
		last    time.Time
		haveRow bool
		want    bool
	}{
		{"young and never run", start.Add(2 * time.Minute), time.Time{}, false, true},
		{"old and never run", start.Add(3 * time.Minute), time.Time{}, false, false},
		{"old and recent", start.Add(10 * time.Minute), start.Add(9 * time.Minute), true, true},
		{"old and stale", start.Add(10 * time.Minute), start.Add(6 * time.Minute), true, false},
	}
	for _, tc := range cases {
		if got := schedulerHealthy(tc.now, start, tc.last, tc.haveRow); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}
