package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Config configures a Manager.
type Config struct {
	Repo string // owner/name (default DefaultRepo)
	API  string // releases API base (default DefaultAPI)
	// Current is the running version.
	Current func() string
	// Exe is the binary to replace (default: the running one, found when the
	// Manager is made, because a replaced binary no longer has its own path).
	Exe string
	// Restart asks the process to stop gracefully and start the new binary. A
	// Manager without it cannot install.
	Restart func()
	// RestartDelay is how long after a successful install the restart is asked
	// for, so the page can show that it is coming (default 1s).
	RestartDelay time.Duration
	CacheTTL     time.Duration // how long the latest release is remembered (default 5m)
	Now          func() time.Time
	// APIClient and DownloadClient are the HTTP clients for the releases API and for the
	// assets (defaults: guarded clients with timeouts).
	APIClient, DownloadClient *http.Client
	// CosignInstalled reports whether signatures can be verified (default: cosign
	// is on the PATH).
	CosignInstalled func() bool
	Logger          *slog.Logger
}

// State of the update the Manager is running or ran last.
type State string

const (
	StateIdle       State = "idle"
	StateRunning    State = "running"
	StateRestarting State = "restarting"
	StateFailed     State = "failed"
)

// Status is the progress of an install.
type Status struct {
	State      State
	From, To   string
	Log        []string
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Info is what the update page shows.
type Info struct {
	Current    string
	Latest     string
	ReleaseURL string // the release page, with the notes
	Newer      bool   // Latest is newer than Current
	Dev        bool   // Current is not a release version
	CheckedAt  time.Time
	FetchError string // why the latest release is not known
	// Blocked is why an install is not offered ("" when it is).
	Blocked string
	Exe     string
}

// Reasons Start refuses.
var (
	ErrBusy     = errors.New("an update is already running")
	ErrBlocked  = errors.New("this server cannot update itself")
	ErrNotNewer = errors.New("the release is not newer than the running version")
	ErrDevBuild = errors.New("this is a development build")
)

// Manager runs one update at a time and remembers the latest release.
type Manager struct {
	cfg Config

	mu      sync.Mutex
	status  Status
	latest  Release
	fetched time.Time
	fetchEr string
}

// New makes a Manager.
func New(cfg Config) *Manager {
	if cfg.Repo == "" {
		cfg.Repo = DefaultRepo
	}
	if cfg.API == "" {
		cfg.API = DefaultAPI
	}
	if cfg.Current == nil {
		cfg.Current = func() string { return "" }
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.RestartDelay == 0 {
		cfg.RestartDelay = time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CosignInstalled == nil {
		cfg.CosignInstalled = CosignInstalled
	}
	if cfg.APIClient == nil {
		cfg.APIClient = guardedClient(15 * time.Second)
	}
	if cfg.DownloadClient == nil {
		cfg.DownloadClient = guardedClient(10 * time.Minute)
	}
	if cfg.Exe == "" {
		cfg.Exe, _ = ExecutablePath()
	}
	return &Manager{cfg: cfg, status: Status{State: StateIdle}}
}

// guardedClient is an HTTP client with a timeout that follows a few redirects
// and never from https to http. Release assets are redirected to GitHub's
// storage hosts, so the host is not pinned.
func guardedClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" && via[0].URL.Scheme == "https" {
			return fmt.Errorf("refusing to follow a redirect to %s", req.URL.Redacted())
		}
		return nil
	}}
}

func (m *Manager) client(c *http.Client) *Client {
	return &Client{API: strings.TrimRight(m.cfg.API, "/"), HTTP: c}
}

// blocked is why an install is not possible here, or "".
func (m *Manager) blocked() string {
	switch {
	case m.cfg.Restart == nil:
		return "This build cannot restart itself, so it cannot update itself. Update it with your package manager or image instead."
	case runtime.GOOS == "windows":
		return "Updating from the dashboard is not supported on Windows."
	case m.cfg.Exe == "":
		return "The path of the running binary is unknown. Update it with your package manager or image instead."
	}
	if err := CheckWritable(m.cfg.Exe); err != nil {
		return "Paymux cannot replace its own binary here (" + err.Error() + "). Update it with your package manager or image instead."
	}
	if !m.cfg.CosignInstalled() {
		return "The signature of a release can only be verified with cosign, which is not installed on this server (https://docs.sigstore.dev/cosign/). Install it, or update with the paymux command line."
	}
	return ""
}

// Info reports the running and the latest version, and whether an install is
// possible. refresh asks GitHub again instead of using the cached answer.
func (m *Manager) Info(ctx context.Context, refresh bool) Info {
	cur := m.cfg.Current()
	info := Info{Current: cur, Dev: !Parsable(cur), Exe: m.cfg.Exe, Blocked: m.blocked()}
	m.mu.Lock()
	if refresh || m.fetched.IsZero() || m.cfg.Now().Sub(m.fetched) >= m.cfg.CacheTTL {
		m.mu.Unlock()
		rel, err := m.client(m.cfg.APIClient).FetchRelease(ctx, m.cfg.Repo, "")
		m.mu.Lock()
		m.fetched = m.cfg.Now()
		if err != nil {
			m.latest, m.fetchEr = Release{}, redact(err.Error())
		} else {
			m.latest, m.fetchEr = rel, ""
		}
	}
	info.Latest, info.ReleaseURL, info.FetchError, info.CheckedAt = m.latest.Tag, m.latest.HTMLURL, m.fetchEr, m.fetched
	m.mu.Unlock()
	info.Newer = info.Latest != "" && !info.Dev && CompareVersions(cur, info.Latest) < 0
	return info
}

// Start begins installing the release tagged target in the background and
// returns at once. force allows the same version, an older one, or an update
// of a development build. done, if set, is called with the final status of the
// install before a restart is asked for.
func (m *Manager) Start(target string, force bool, done func(Status)) error {
	cur := m.cfg.Current()
	if !Parsable(target) {
		return fmt.Errorf("%q is not a release version", target)
	}
	if why := m.blocked(); why != "" {
		return fmt.Errorf("%w: %s", ErrBlocked, why)
	}
	switch {
	case !Parsable(cur) && !force:
		return fmt.Errorf("%w (version %q), so it cannot tell whether %s is newer; tick \"force\" to install it anyway", ErrDevBuild, cur, target)
	case Parsable(cur) && !force && CompareVersions(cur, target) >= 0:
		return fmt.Errorf("%w: %s is not newer than %s; tick \"force\" to install it anyway", ErrNotNewer, target, cur)
	}
	m.mu.Lock()
	if m.status.State == StateRunning || m.status.State == StateRestarting {
		m.mu.Unlock()
		return ErrBusy
	}
	m.status = Status{State: StateRunning, From: cur, To: target, StartedAt: m.cfg.Now()}
	m.mu.Unlock()
	go m.run(cur, target, done)
	return nil
}

func (m *Manager) logf(format string, args ...any) {
	line := redact(fmt.Sprintf(format, args...))
	m.cfg.Logger.Info("selfupdate: " + line)
	m.mu.Lock()
	m.status.Log = append(m.status.Log, line)
	m.mu.Unlock()
}

func (m *Manager) run(cur, target string, done func(Status)) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	err := func() error {
		rel, err := m.client(m.cfg.APIClient).FetchRelease(ctx, m.cfg.Repo, target)
		if err != nil {
			return err
		}
		// Signature verification is never skipped from here.
		_, err = m.client(m.cfg.DownloadClient).Install(ctx, rel, InstallOptions{Repo: m.cfg.Repo, Dest: m.cfg.Exe, Current: cur, Logf: m.logf})
		return err
	}()
	m.mu.Lock()
	final := m.status
	final.Log = append([]string(nil), final.Log...)
	m.mu.Unlock()
	final.FinishedAt = m.cfg.Now()
	if err != nil {
		final.State, final.Error = StateFailed, redact(err.Error())
		m.cfg.Logger.Warn("selfupdate: failed", "from", cur, "to", target, "err", final.Error)
	} else {
		final.State = StateRestarting
	}
	// done (the audit) runs before the result is visible, so whoever sees it
	// finish finds it recorded.
	if done != nil {
		done(final)
	}
	m.mu.Lock()
	m.status.State, m.status.Error, m.status.FinishedAt = final.State, final.Error, final.FinishedAt
	m.mu.Unlock()
	if err == nil {
		time.Sleep(m.cfg.RestartDelay)
		m.cfg.Restart()
	}
}

// Status is the state of the current or last install.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status
	s.Log = append([]string(nil), s.Log...)
	return s
}

var secretRE = regexp.MustCompile(`(?i)(bearer|token|authorization:?)\s+[A-Za-z0-9._~+/=-]{6,}|\bgh[pousr]_[A-Za-z0-9]{20,}|[?&](token|access_token|sig|signature|key)=[^&\s]+`)

// redact removes what looks like a credential from text shown to a person.
func redact(s string) string { return secretRE.ReplaceAllString(s, "[redacted]") }
