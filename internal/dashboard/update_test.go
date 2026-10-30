package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/selfupdate"
	"github.com/velixio/paymux/internal/selfupdate/selfupdatetest"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// The Update page installs a newer release from GitHub: admin only, behind a
// recent sign-in and CSRF, audited, and never without the cosign signature.

type updateRig struct {
	*env
	rel      *selfupdatetest.Releases
	exe      string
	restarts atomic.Int32
}

// newUpdateEnv runs v1.3.2 (a stand-in binary in a temp dir) against a signed
// v1.4.0 release, with a fake cosign that accepts it.
func newUpdateEnv(t *testing.T, db *store.DB, current string, mut func(*selfupdate.Config)) *updateRig {
	t.Helper()
	u := &updateRig{rel: selfupdatetest.NewReleases(t, "v1.4.0", "v1.4.0", true), exe: selfupdatetest.WriteBinary(t, current)}
	selfupdatetest.FakeCosign(t, 0)
	cfg := selfupdate.Config{API: u.rel.Srv.URL, Current: func() string { return current }, Exe: u.exe,
		Restart: func() { u.restarts.Add(1) }, RestartDelay: time.Millisecond}
	if mut != nil {
		mut(&cfg)
	}
	mgr := selfupdate.New(cfg)
	u.env = newEnv(t, db, func(o *Options) { o.Updater = mgr })
	return u
}

func (u *updateRig) binary() []byte { b, _ := os.ReadFile(u.exe); return b }

// finished waits until the install is over and returns the status page's JSON.
func (u *updateRig) finished(c *http.Client) map[string]any {
	u.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, body := u.get(c, "/dashboard/update/status")
		var st map[string]any
		if err := json.Unmarshal([]byte(body), &st); err == nil && (st["state"] == "failed" || st["state"] == "restarting") {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	u.t.Fatal("the update did not finish")
	return nil
}

func (u *updateRig) audits(action string) []store.AuditRow {
	var rows []store.AuditRow
	_ = u.db.Read(ctx, func(q store.Querier) (err error) {
		rows, err = store.ListAudit(ctx, q, store.DefaultOrgID, action, 10)
		return
	})
	return rows
}

func form(tok string, kv ...string) url.Values {
	v := url.Values{"csrf": {tok}}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func TestUpdatePageShowsTheVersionsAndOffersTheUpdate(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		u := newUpdateEnv(t, db, "v1.3.2", nil)
		admin := u.roleClient("admin")
		body := u.pageOK(admin, "/dashboard/update")
		for _, want := range []string{"v1.3.2", "v1.4.0", u.rel.NotesURL(), "Update to v1.4.0", `action="/dashboard/update"`} {
			if !strings.Contains(body, want) {
				t.Errorf("page lacks %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, "disabled") {
			t.Errorf("the button is disabled although an update is possible:\n%s", body)
		}
		if !strings.Contains(u.pageOK(admin, "/dashboard/overview"), `href="/dashboard/update"`) {
			t.Error("the admin's menu has no Update")
		}
	})
}

func TestUpdateIsForAdminsOnly(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		u := newUpdateEnv(t, db, "v1.3.2", nil)
		for _, role := range []string{"operator", "viewer"} {
			c := u.roleClient(role)
			tok := csrfOf(t, u.pageOK(c, "/dashboard/payments"))
			if r, _ := u.get(c, "/dashboard/update"); r.StatusCode != 403 {
				t.Errorf("%s GET: %d", role, r.StatusCode)
			}
			if r, _ := u.get(c, "/dashboard/update/status"); r.StatusCode != 403 {
				t.Errorf("%s status: %d", role, r.StatusCode)
			}
			if r, _ := u.post(c, "/dashboard/update", form(tok, "target", "v1.4.0")); r.StatusCode != 403 {
				t.Errorf("%s POST: %d", role, r.StatusCode)
			}
			if strings.Contains(u.pageOK(c, "/dashboard/payments"), `href="/dashboard/update"`) {
				t.Errorf("%s sees Update in the menu", role)
			}
		}
		if !bytes.Equal(u.binary(), selfupdatetest.Script("v1.3.2")) || u.restarts.Load() != 0 {
			t.Error("a refused request changed something")
		}
	})
}

func TestUpdateNeedsCSRFAndARecentSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		u := newUpdateEnv(t, db, "v1.3.2", nil)
		c := staleAdmin(t, u.env)
		tok := csrfOf(t, u.pageOK(c, "/dashboard/update"))
		wantReauth(t, "POST /dashboard/update", mustPost(u.env, c, "/dashboard/update", form(tok, "target", "v1.4.0")))
		if r, _ := u.post(c, "/dashboard/update", url.Values{"target": {"v1.4.0"}}); r.StatusCode != 403 {
			t.Errorf("no CSRF token: %d", r.StatusCode)
		}
		if !bytes.Equal(u.binary(), selfupdatetest.Script("v1.3.2")) {
			t.Error("the binary changed")
		}
	})
}

func mustPost(e *env, c *http.Client, path string, f url.Values) *http.Response {
	r, _ := e.post(c, path, f)
	return r
}

func TestUpdateInstallsReplacesTheBinaryAndAsksForARestart(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		u := newUpdateEnv(t, db, "v1.3.2", nil)
		admin := u.roleClient("admin")
		tok := csrfOf(t, u.pageOK(admin, "/dashboard/update"))
		r, _ := u.post(admin, "/dashboard/update", form(tok, "target", "v1.4.0"))
		if r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/update" {
			t.Fatalf("POST: %d %q", r.StatusCode, r.Header.Get("Location"))
		}
		st := u.finished(admin)
		if st["state"] != "restarting" || st["to"] != "v1.4.0" || st["from"] != "v1.3.2" {
			t.Fatalf("status = %v", st)
		}
		if !bytes.Equal(u.binary(), selfupdatetest.Script("v1.4.0")) {
			t.Errorf("binary = %q", u.binary())
		}
		deadline := time.Now().Add(5 * time.Second)
		for u.restarts.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if n := u.restarts.Load(); n != 1 {
			t.Errorf("restart asked %d times", n)
		}
		// Audited: who, from, to, result.
		rows := u.audits("system.update")
		if len(rows) != 1 || rows[0].ActorType != "user" || rows[0].ActorID != "usr_admin" {
			t.Fatalf("audit = %+v", rows)
		}
		for _, want := range []string{"v1.3.2", "v1.4.0", `"ok"`} {
			if !strings.Contains(rows[0].After, want) {
				t.Errorf("audit lacks %s: %s", want, rows[0].After)
			}
		}
		if body := u.pageOK(admin, "/dashboard/update"); !strings.Contains(body, "restart") {
			t.Errorf("the page does not say it is restarting:\n%s", body)
		}
	})
}

func TestUpdateWithABadSignatureInstallsNothingAndSaysSo(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		u := newUpdateEnv(t, db, "v1.3.2", nil)
		selfupdatetest.FakeCosign(t, 1)
		admin := u.roleClient("admin")
		tok := csrfOf(t, u.pageOK(admin, "/dashboard/update"))
		u.post(admin, "/dashboard/update", form(tok, "target", "v1.4.0"))
		st := u.finished(admin)
		if st["state"] != "failed" || !strings.Contains(st["error"].(string), "did not verify") {
			t.Fatalf("status = %v", st)
		}
		if !bytes.Equal(u.binary(), selfupdatetest.Script("v1.3.2")) || u.restarts.Load() != 0 {
			t.Error("an unverified release was installed")
		}
		if body := u.pageOK(admin, "/dashboard/update"); !strings.Contains(body, "did not verify") {
			t.Errorf("the page does not show the error:\n%s", body)
		}
		rows := u.audits("system.update")
		if len(rows) != 1 || !strings.Contains(rows[0].After, `"failed"`) || !strings.Contains(rows[0].After, "did not verify") {
			t.Errorf("audit = %+v", rows)
		}
	})
}

func TestUpdateIsOfferedOnlyWhereItCanBeSafe(t *testing.T) {
	t.Run("cosign missing", func(t *testing.T) {
		storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
			u := newUpdateEnv(t, db, "v1.3.2", nil)
			selfupdatetest.NoCosign(t)
			admin := u.roleClient("admin")
			body := u.pageOK(admin, "/dashboard/update")
			if !strings.Contains(body, "cosign") || !strings.Contains(body, "disabled") {
				t.Errorf("no explanation and disabled button:\n%s", body)
			}
			tok := csrfOf(t, body)
			if r, _ := u.post(admin, "/dashboard/update", form(tok, "target", "v1.4.0")); r.StatusCode != 400 && r.StatusCode != 409 {
				t.Errorf("POST without cosign: %d", r.StatusCode)
			}
			if !bytes.Equal(u.binary(), selfupdatetest.Script("v1.3.2")) {
				t.Error("installed without cosign")
			}
		})
	})
	t.Run("binary not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes anywhere")
		}
		storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
			u := newUpdateEnv(t, db, "v1.3.2", nil)
			dir := filepath.Dir(u.exe)
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			admin := u.roleClient("admin")
			body := u.pageOK(admin, "/dashboard/update")
			if !strings.Contains(body, "package manager or image") || !strings.Contains(body, "not writable") || !strings.Contains(body, "disabled") {
				t.Errorf("no explanation and disabled button:\n%s", body)
			}
			tok := csrfOf(t, body)
			if r, _ := u.post(admin, "/dashboard/update", form(tok, "target", "v1.4.0")); r.StatusCode != 400 && r.StatusCode != 409 {
				t.Errorf("POST on a read-only binary: %d", r.StatusCode)
			}
		})
	})
}

func TestUpdateRefusesDowngradesAndSameVersionUnlessForced(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		u := newUpdateEnv(t, db, "v2.0.0", nil)
		admin := u.roleClient("admin")
		body := u.pageOK(admin, "/dashboard/update")
		if strings.Contains(body, "Update to v1.4.0") || !strings.Contains(body, "force") {
			t.Errorf("a downgrade is offered as an update:\n%s", body)
		}
		tok := csrfOf(t, body)
		r, msg := u.post(admin, "/dashboard/update", form(tok, "target", "v1.4.0"))
		if r.StatusCode != 400 || !strings.Contains(msg, "force") {
			t.Fatalf("downgrade without force: %d\n%s", r.StatusCode, msg)
		}
		if !bytes.Equal(u.binary(), selfupdatetest.Script("v2.0.0")) {
			t.Error("a refused downgrade changed the binary")
		}
		if r, _ := u.post(admin, "/dashboard/update", form(tok, "target", "v1.4.0", "force", "on")); r.StatusCode != 303 {
			t.Fatalf("forced: %d", r.StatusCode)
		}
		if st := u.finished(admin); st["state"] != "restarting" {
			t.Errorf("status = %v", st)
		}
		if !bytes.Equal(u.binary(), selfupdatetest.Script("v1.4.0")) {
			t.Error("a forced update did not install")
		}
	})
}

func TestUpdateOnlyInstallsTheLatestReleaseShownAndOnlyOneAtATime(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		u := newUpdateEnv(t, db, "v1.3.2", nil)
		admin := u.roleClient("admin")
		tok := csrfOf(t, u.pageOK(admin, "/dashboard/update"))
		if r, _ := u.post(admin, "/dashboard/update", form(tok, "target", "v9.9.9")); r.StatusCode != 400 {
			t.Errorf("a release the page did not offer: %d", r.StatusCode)
		}
		if r, _ := u.post(admin, "/dashboard/update", form(tok)); r.StatusCode != 400 {
			t.Errorf("no target: %d", r.StatusCode)
		}
		if !bytes.Equal(u.binary(), selfupdatetest.Script("v1.3.2")) {
			t.Error("the binary changed")
		}
	})
}

func TestUpdateRunsOneAtATime(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		u := newUpdateEnv(t, db, "v1.3.2", nil)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "cosign"), []byte("#!/bin/sh\nsleep 1\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		admin := u.roleClient("admin")
		tok := csrfOf(t, u.pageOK(admin, "/dashboard/update"))
		if r, _ := u.post(admin, "/dashboard/update", form(tok, "target", "v1.4.0")); r.StatusCode != 303 {
			t.Fatalf("first: %d", r.StatusCode)
		}
		r, msg := u.post(admin, "/dashboard/update", form(tok, "target", "v1.4.0"))
		if r.StatusCode != 409 || !strings.Contains(msg, "already running") {
			t.Errorf("second: %d %s", r.StatusCode, msg)
		}
		u.finished(admin)
	})
}

func TestUpdatePageIsAbsentWithoutAnUpdater(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		admin := e.roleClient("admin")
		if r, _ := e.get(admin, "/dashboard/update"); r.StatusCode != 404 {
			t.Errorf("GET: %d", r.StatusCode)
		}
		if strings.Contains(e.pageOK(admin, "/dashboard/overview"), `href="/dashboard/update"`) {
			t.Error("the menu links to a page that is not there")
		}
	})
}
