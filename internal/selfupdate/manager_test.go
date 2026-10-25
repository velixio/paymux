package selfupdate_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/selfupdate"
	"github.com/paymux/paymux/internal/selfupdate/selfupdatetest"
)

type rig struct {
	t        *testing.T
	rel      *selfupdatetest.Releases
	exe      string
	m        *selfupdate.Manager
	restarts atomic.Int32
	now      atomic.Int64
}

// newRig has a running v1.3.2 in a temp dir and a signed v1.4.0 release.
func newRig(t *testing.T, current string, mut func(*selfupdate.Config)) *rig {
	t.Helper()
	r := &rig{t: t, rel: selfupdatetest.NewReleases(t, "v1.4.0", "v1.4.0", true), exe: selfupdatetest.WriteBinary(t, current)}
	r.now.Store(time.Unix(1_800_000_000, 0).UnixNano())
	selfupdatetest.FakeCosign(t, 0)
	cfg := selfupdate.Config{API: r.rel.Srv.URL, Current: func() string { return current }, Exe: r.exe,
		Restart: func() { r.restarts.Add(1) }, RestartDelay: time.Millisecond, CacheTTL: 5 * time.Minute,
		Now: func() time.Time { return time.Unix(0, r.now.Load()) }}
	if mut != nil {
		mut(&cfg)
	}
	r.m = selfupdate.New(cfg)
	return r
}

func (r *rig) advance(d time.Duration) { r.now.Add(int64(d)) }

// install starts an update and waits for it to finish.
func (r *rig) install(target string, force bool) (selfupdate.Status, error) {
	r.t.Helper()
	ch := make(chan selfupdate.Status, 1)
	if err := r.m.Start(target, force, func(s selfupdate.Status) { ch <- s }); err != nil {
		return selfupdate.Status{}, err
	}
	select {
	case s := <-ch:
		return s, nil
	case <-time.After(20 * time.Second):
		r.t.Fatal("the update did not finish")
		return selfupdate.Status{}, nil
	}
}

func (r *rig) binary() []byte { b, _ := os.ReadFile(r.exe); return b }

func TestInfoReportsTheLatestReleaseAndCachesIt(t *testing.T) {
	r := newRig(t, "v1.3.2", nil)
	info := r.m.Info(context.Background(), false)
	if info.Current != "v1.3.2" || info.Latest != "v1.4.0" || !info.Newer || info.Dev || info.ReleaseURL != r.rel.NotesURL() || info.Blocked != "" || info.FetchError != "" {
		t.Fatalf("info = %+v", info)
	}
	r.m.Info(context.Background(), false)
	if n := r.rel.Hits("/repos/"); n != 1 {
		t.Errorf("%d API calls for two views", n)
	}
	r.advance(6 * time.Minute)
	r.m.Info(context.Background(), false)
	if n := r.rel.Hits("/repos/"); n != 2 {
		t.Errorf("the cache did not expire: %d calls", n)
	}
	r.m.Info(context.Background(), true)
	if n := r.rel.Hits("/repos/"); n != 3 {
		t.Errorf("refresh did not ask again: %d calls", n)
	}
	// Up to date: not newer.
	if i := newRig(t, "v1.4.0", nil).m.Info(context.Background(), false); i.Newer {
		t.Errorf("up to date reported newer: %+v", i)
	}
}

func TestInfoShowsWhyAnInstallIsNotOffered(t *testing.T) {
	t.Run("cosign missing", func(t *testing.T) {
		r := newRig(t, "v1.3.2", nil)
		selfupdatetest.NoCosign(t)
		if b := r.m.Info(context.Background(), false).Blocked; !strings.Contains(b, "cosign") {
			t.Errorf("blocked = %q", b)
		}
	})
	t.Run("binary not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes anywhere")
		}
		r := newRig(t, "v1.3.2", nil)
		dir := filepath.Dir(r.exe)
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		b := r.m.Info(context.Background(), false).Blocked
		if !strings.Contains(b, "not writable") || !strings.Contains(b, "package") {
			t.Errorf("blocked = %q", b)
		}
	})
	t.Run("no restart", func(t *testing.T) {
		r := newRig(t, "v1.3.2", func(c *selfupdate.Config) { c.Restart = nil })
		if b := r.m.Info(context.Background(), false).Blocked; b == "" {
			t.Error("a build that cannot restart itself offers an install")
		}
	})
	t.Run("blocked installs are refused", func(t *testing.T) {
		r := newRig(t, "v1.3.2", func(c *selfupdate.Config) { c.Restart = nil })
		if _, err := r.install("v1.4.0", false); !errors.Is(err, selfupdate.ErrBlocked) {
			t.Errorf("err = %v", err)
		}
		if !bytes.Equal(r.binary(), selfupdatetest.Script("v1.3.2")) {
			t.Error("the binary changed")
		}
	})
}

func TestInstallReplacesTheBinaryThenAsksForARestart(t *testing.T) {
	r := newRig(t, "v1.3.2", nil)
	st, err := r.install("v1.4.0", false)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != selfupdate.StateRestarting || st.From != "v1.3.2" || st.To != "v1.4.0" || st.Error != "" {
		t.Fatalf("status = %+v", st)
	}
	if !bytes.Equal(r.binary(), selfupdatetest.Script("v1.4.0")) {
		t.Errorf("binary = %q", r.binary())
	}
	if b, _ := os.ReadFile(r.exe + ".prev"); !bytes.Equal(b, selfupdatetest.Script("v1.3.2")) {
		t.Error("previous binary not kept")
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.restarts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := r.restarts.Load(); n != 1 {
		t.Errorf("restart asked %d times", n)
	}
	if got := r.m.Status(); got.State != selfupdate.StateRestarting || got.To != "v1.4.0" || len(got.Log) == 0 {
		t.Errorf("status after = %+v", got)
	}
}

func TestInstallNeverInstallsAnUnverifiedRelease(t *testing.T) {
	for name, c := range map[string]struct {
		mut    func(r *rig)
		cosign int
		want   string
	}{
		"bad signature": {func(*rig) {}, 1, "did not verify"},
		"unsigned":      {func(r *rig) { delete(r.rel.Assets, "SHA256SUMS.sig"); delete(r.rel.Assets, "SHA256SUMS.pem") }, 0, "not signed"},
		"bad checksum": {func(r *rig) {
			r.rel.Assets["SHA256SUMS"] = []byte(strings.Repeat("a", 64) + "  " + r.rel.AssetName() + "\n")
		}, 0, "checksum mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "v1.3.2", nil)
			selfupdatetest.FakeCosign(t, c.cosign)
			c.mut(r)
			st, err := r.install("v1.4.0", false)
			if err != nil {
				t.Fatal(err)
			}
			if st.State != selfupdate.StateFailed || !strings.Contains(st.Error, c.want) {
				t.Errorf("status = %+v", st)
			}
			if !bytes.Equal(r.binary(), selfupdatetest.Script("v1.3.2")) {
				t.Error("the binary was replaced")
			}
			time.Sleep(20 * time.Millisecond)
			if r.restarts.Load() != 0 {
				t.Error("a restart was asked for after a failed install")
			}
			if r.m.Status().State != selfupdate.StateFailed {
				t.Errorf("state = %s", r.m.Status().State)
			}
		})
	}
}

func TestInstallRefusesDowngradesAndDevBuildsUnlessForced(t *testing.T) {
	r := newRig(t, "v2.0.0", nil)
	if _, err := r.install("v1.4.0", false); !errors.Is(err, selfupdate.ErrNotNewer) {
		t.Errorf("downgrade: %v", err)
	}
	if _, err := newRig(t, "v1.4.0", nil).install("v1.4.0", false); !errors.Is(err, selfupdate.ErrNotNewer) {
		t.Errorf("same version: %v", err)
	}
	if _, err := newRig(t, "dev", nil).install("v1.4.0", false); !errors.Is(err, selfupdate.ErrDevBuild) {
		t.Errorf("dev build: %v", err)
	}
	if !bytes.Equal(r.binary(), selfupdatetest.Script("v2.0.0")) {
		t.Error("a refused update changed the binary")
	}
	st, err := r.install("v1.4.0", true)
	if err != nil || st.State != selfupdate.StateRestarting {
		t.Errorf("forced: %+v %v", st, err)
	}
	if !bytes.Equal(r.binary(), selfupdatetest.Script("v1.4.0")) {
		t.Error("a forced update did not install")
	}
}

func TestOnlyOneUpdateRunsAtATime(t *testing.T) {
	dir := t.TempDir()
	slow := filepath.Join(dir, "cosign")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 1\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, "v1.3.2", nil)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ch := make(chan selfupdate.Status, 1)
	if err := r.m.Start("v1.4.0", false, func(s selfupdate.Status) { ch <- s }); err != nil {
		t.Fatal(err)
	}
	if st := r.m.Status(); st.State != selfupdate.StateRunning {
		t.Errorf("state = %s", st.State)
	}
	if err := r.m.Start("v1.4.0", false, nil); !errors.Is(err, selfupdate.ErrBusy) {
		t.Errorf("a second update: %v", err)
	}
	<-ch
}

func TestFailureTextHidesSecrets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cosign"), []byte("#!/bin/sh\necho 'Authorization: Bearer ghp_abcdefghijklmnopqrstuvwxyz0123456789 failed'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, "v1.3.2", nil)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	st, err := r.install("v1.4.0", false)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != selfupdate.StateFailed || !strings.Contains(st.Error, "failed") || strings.Contains(st.Error, "ghp_") || strings.Contains(strings.Join(st.Log, "\n"), "ghp_") {
		t.Errorf("status = %+v", st)
	}
}
