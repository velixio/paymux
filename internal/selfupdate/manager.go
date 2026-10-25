package selfupdate

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
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
	// API and Download are the HTTP clients for the releases API and for the
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
type Manager struct{ cfg Config }

// New makes a Manager.
func New(cfg Config) *Manager { return &Manager{cfg: cfg} }

// Info reports the running and the latest version, and whether an install is
// possible. refresh asks GitHub again instead of using the cached answer.
func (m *Manager) Info(ctx context.Context, refresh bool) Info { return Info{} }

// Start begins installing the release tagged target in the background and
// returns at once. force allows the same version, an older one, or an update
// of a development build. done, if set, is called with the final status of the
// install before a restart is asked for.
func (m *Manager) Start(target string, force bool, done func(Status)) error {
	return errors.New("not implemented")
}

// Status is the state of the current or last install.
func (m *Manager) Status() Status { return Status{State: StateIdle} }
