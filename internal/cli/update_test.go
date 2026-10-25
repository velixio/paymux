package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// newScript is a stand-in binary: a shell script that answers "version".
func newScript(version string) []byte {
	return []byte("#!/bin/sh\necho \"paymux " + version + "\"\n")
}

func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// A README first, so extraction must pick the binary by name.
	for _, f := range []struct {
		name string
		body []byte
		mode int64
	}{{"README.txt", []byte("hello"), 0o644}, {name, body, 0o755}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// fakeReleases is a GitHub-like releases API with assets.
type fakeReleases struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	hits     []string
	tag      string
	assets   map[string][]byte // name -> content
	missing  map[string]bool   // asset names listed in the release but 404 on download
	noLatest bool
}

func newFakeReleases(t *testing.T, tag string, binVersion string) *fakeReleases {
	f := &fakeReleases{t: t, tag: tag, assets: map[string][]byte{}, missing: map[string]bool{}}
	asset := fmt.Sprintf("paymux_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
	archive := tarGz(t, "paymux", newScript(binVersion))
	f.assets[asset] = archive
	f.assets["SHA256SUMS"] = []byte(sum(archive) + "  " + asset + "\n" + strings.Repeat("0", 64) + "  paymux_other_os_arch.tar.gz\n")
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeReleases) release() map[string]any {
	var assets []map[string]any
	for name, body := range f.assets {
		assets = append(assets, map[string]any{"name": name, "size": len(body), "browser_download_url": f.srv.URL + "/dl/" + name})
	}
	return map[string]any{"tag_name": f.tag, "draft": false, "prerelease": false, "assets": assets}
}

func (f *fakeReleases) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits = append(f.hits, r.URL.Path)
	f.mu.Unlock()
	switch {
	case r.URL.Path == "/repos/velixio/paymux/releases/latest" && !f.noLatest:
		_ = json.NewEncoder(w).Encode(f.release())
	case strings.HasPrefix(r.URL.Path, "/repos/velixio/paymux/releases/tags/"):
		if strings.TrimPrefix(r.URL.Path, "/repos/velixio/paymux/releases/tags/") != f.tag {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(f.release())
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		body, ok := f.assets[name]
		if !ok || f.missing[name] {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeReleases) downloads() (n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.hits {
		if strings.HasPrefix(h, "/dl/") {
			n++
		}
	}
	return n
}

func (f *fakeReleases) assetName() string {
	return fmt.Sprintf("paymux_%s_%s_%s.tar.gz", strings.TrimPrefix(f.tag, "v"), runtime.GOOS, runtime.GOARCH)
}

// updateEnv installs the "running binary" in a temp dir and fixes the version.
func updateEnv(t *testing.T, current string) (dest string) {
	t.Helper()
	old := currentVersion
	currentVersion = func() string { return current }
	t.Cleanup(func() { currentVersion = old })
	dest = filepath.Join(t.TempDir(), "paymux")
	if err := os.WriteFile(dest, newScript(current), 0o755); err != nil {
		t.Fatal(err)
	}
	return dest
}

func runUpdateCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), append([]string{"update"}, args...), func(string) (string, bool) { return "", false }, &out, &errb)
	return code, out.String(), errb.String()
}

func TestUpdateCheckOnlyReports(t *testing.T) {
	f := newFakeReleases(t, "v1.4.0", "v1.4.0")
	dest := updateEnv(t, "v1.3.2")
	code, out, errs := runUpdateCmd(t, "--check", "--api-url", f.srv.URL, "--dest", dest)
	if code != 0 || !strings.Contains(out, "v1.3.2") || !strings.Contains(out, "v1.4.0") || !strings.Contains(strings.ToLower(out), "available") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if f.downloads() != 0 {
		t.Error("--check downloaded something")
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, newScript("v1.3.2")) {
		t.Error("--check changed the binary")
	}

	// Up to date: nothing to do, and nothing downloaded.
	dest = updateEnv(t, "v1.4.0")
	code, out, _ = runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--insecure-skip-signature")
	if code != 0 || !strings.Contains(strings.ToLower(out), "up to date") || f.downloads() != 0 {
		t.Errorf("up to date: code=%d out=%q downloads=%d", code, out, f.downloads())
	}
	// A newer binary than the release is not "downgraded" without --force.
	dest = updateEnv(t, "v2.0.0")
	if code, out, _ = runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest); code != 0 || !strings.Contains(strings.ToLower(out), "up to date") || f.downloads() != 0 {
		t.Errorf("newer than latest: code=%d out=%q", code, out)
	}
}

func TestUpdateReplacesTheBinaryAtomicallyAfterVerifying(t *testing.T) {
	f := newFakeReleases(t, "v1.4.0", "v1.4.0")
	dest := updateEnv(t, "v1.3.2")
	code, out, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--insecure-skip-signature")
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, newScript("v1.4.0")) {
		t.Errorf("binary not replaced: %q", b)
	}
	if st, err := os.Stat(dest); err != nil || st.Mode().Perm()&0o111 == 0 {
		t.Errorf("replacement is not executable: %v %v", st, err)
	}
	if b, err := os.ReadFile(dest + ".prev"); err != nil || !bytes.Equal(b, newScript("v1.3.2")) {
		t.Errorf("previous binary not kept for rollback: %q %v", b, err)
	}
	for _, want := range []string{"v1.3.2", "v1.4.0", "restart", dest, "docs/upgrades.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(dest))
	for _, e := range entries {
		if e.Name() != "paymux" && e.Name() != "paymux.prev" {
			t.Errorf("leftover file %s", e.Name())
		}
	}
}

func TestUpdateRefusesWhatItCannotVerify(t *testing.T) {
	cases := map[string]func(f *fakeReleases){
		"checksum mismatch": func(f *fakeReleases) {
			f.assets["SHA256SUMS"] = []byte(strings.Repeat("a", 64) + "  " + f.assetName() + "\n")
		},
		"no entry for the asset": func(f *fakeReleases) {
			f.assets["SHA256SUMS"] = []byte(strings.Repeat("a", 64) + "  something_else.tar.gz\n")
		},
		"no SHA256SUMS at all": func(f *fakeReleases) { delete(f.assets, "SHA256SUMS") },
		"tampered archive": func(f *fakeReleases) {
			f.assets[f.assetName()] = tarGz(t, "paymux", newScript("v6.6.6"))
		},
		"binary reports another version": func(f *fakeReleases) {
			a := tarGz(t, "paymux", newScript("v9.9.9"))
			f.assets[f.assetName()] = a
			f.assets["SHA256SUMS"] = []byte(sum(a) + "  " + f.assetName() + "\n")
		},
		"archive without the binary": func(f *fakeReleases) {
			a := tarGz(t, "not-paymux", newScript("v1.4.0"))
			f.assets[f.assetName()] = a
			f.assets["SHA256SUMS"] = []byte(sum(a) + "  " + f.assetName() + "\n")
		},
		"no asset for this platform": func(f *fakeReleases) { delete(f.assets, f.assetName()) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeReleases(t, "v1.4.0", "v1.4.0")
			mutate(f)
			dest := updateEnv(t, "v1.3.2")
			code, _, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--insecure-skip-signature")
			if code == 0 || errs == "" {
				t.Fatalf("expected a refusal: code=%d err=%q", code, errs)
			}
			if b, _ := os.ReadFile(dest); !bytes.Equal(b, newScript("v1.3.2")) {
				t.Errorf("the binary was replaced despite the failure: %q", b)
			}
			entries, _ := os.ReadDir(filepath.Dir(dest))
			if len(entries) != 1 {
				t.Errorf("leftovers after a failed update: %v", entries)
			}
		})
	}
}

func fakeCosign(t *testing.T, exit int) (args string) {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" > %q\nexit %d\n", argsFile, exit)
	if err := os.WriteFile(filepath.Join(dir, "cosign"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func TestUpdateVerifiesACosignSignatureWhenThereIsOne(t *testing.T) {
	f := newFakeReleases(t, "v1.4.0", "v1.4.0")
	f.assets["SHA256SUMS.sig"] = []byte("c2lnbmF0dXJl")

	// Signature present, cosign installed and a key configured: it must verify.
	argsFile := fakeCosign(t, 0)
	dest := updateEnv(t, "v1.3.2")
	key := filepath.Join(t.TempDir(), "cosign.pub")
	_ = os.WriteFile(key, []byte("key"), 0o600)
	code, out, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--cosign-key", key)
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "verify-blob") || !strings.Contains(string(args), "--key "+key) || !strings.Contains(string(args), "--signature") ||
		!strings.Contains(out+errs, "signature verified") {
		t.Errorf("cosign args %q, output %q", args, out)
	}

	// A signature that does not verify aborts the update.
	fakeCosign(t, 1)
	dest = updateEnv(t, "v1.3.2")
	code, _, errs = runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--cosign-key", key)
	if code == 0 || !strings.Contains(errs, "signature") {
		t.Errorf("bad signature: code=%d err=%q", code, errs)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, newScript("v1.3.2")) {
		t.Error("binary replaced despite a failing signature check")
	}

	// Keyless identity flags are passed through.
	argsFile = fakeCosign(t, 0)
	dest = updateEnv(t, "v1.3.2")
	if code, _, errs = runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--cosign-identity", "https://github.com/velixio/paymux/.github/workflows/release.yml@refs/tags/v1.4.0",
		"--cosign-issuer", "https://token.actions.githubusercontent.com"); code != 0 {
		t.Fatalf("keyless: %q", errs)
	}
	args, _ = os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--certificate-identity") || !strings.Contains(string(args), "--certificate-oidc-issuer") {
		t.Errorf("keyless args %q", args)
	}
}

// A signed release with no cosign on the machine is refused by default (see
// TestUpdateRefusesWhenTheDefaultVerificationFails); with the escape hatch it
// installs on the checksum, and says the signature was not verified.
func TestUpdateWarnsWhenASignatureIsSkipped(t *testing.T) {
	f := newFakeReleases(t, "v1.4.0", "v1.4.0")
	f.assets["SHA256SUMS.sig"] = []byte("c2ln")
	t.Setenv("PATH", t.TempDir()) // no cosign anywhere
	dest := updateEnv(t, "v1.3.2")
	code, out, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--insecure-skip-signature")
	if code != 0 {
		t.Fatalf("the escape hatch must allow a checksum-verified update: %q", errs)
	}
	if !strings.Contains(strings.ToLower(out+errs), "not verified") {
		t.Errorf("no warning about the unchecked signature: out=%q err=%q", out, errs)
	}
}

func TestUpdateUsageAndDevBuilds(t *testing.T) {
	f := newFakeReleases(t, "v1.4.0", "v1.4.0")
	dest := updateEnv(t, "dev")
	code, _, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest)
	if code == 0 || !strings.Contains(errs, "development build") {
		t.Errorf("a dev build cannot say whether it is current: code=%d err=%q", code, errs)
	}
	if code, _, _ = runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--insecure-skip-signature", "--force"); code != 0 {
		t.Error("--force should update a dev build")
	}
	if code, _, errs = runUpdateCmd(t, "extra"); code != 2 || !strings.Contains(errs, "usage: paymux update") {
		t.Errorf("usage: code=%d err=%q", code, errs)
	}
	// A specific version, and a missing one.
	dest = updateEnv(t, "v1.3.2")
	if code, out, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--insecure-skip-signature", "--version", "v1.4.0"); code != 0 || !strings.Contains(out, "v1.4.0") {
		t.Errorf("--version: code=%d out=%q err=%q", code, out, errs)
	}
	if code, _, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--version", "v7.7.7"); code == 0 || !strings.Contains(errs, "v7.7.7") {
		t.Errorf("unknown version: code=%d err=%q", code, errs)
	}
}

func TestUpdateReportsAnUnwritableDestination(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	f := newFakeReleases(t, "v1.4.0", "v1.4.0")
	dest := updateEnv(t, "v1.3.2")
	if err := os.Chmod(filepath.Dir(dest), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Dir(dest), 0o700)
	code, _, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--insecure-skip-signature")
	if code == 0 || !strings.Contains(errs, filepath.Dir(dest)) {
		t.Errorf("code=%d err=%q", code, errs)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{{"v1.2.3", "v1.2.4", -1}, {"1.2.3", "v1.2.3", 0}, {"v1.10.0", "v1.9.9", 1}, {"v2.0.0", "v10.0.0", -1}, {"v1.2.3-rc.1", "v1.2.3", -1},
		{"v1.2.3", "v1.2.3-rc.1", 1}, {"v1.2.3-rc.1", "v1.2.3-rc.2", -1}, {"v1.2", "v1.2.0", 0}} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// When the operator asks for signature verification it is mandatory: a
// stripped signature, a missing cosign or an identity without an issuer must
// refuse the update instead of falling back to checksums.
func TestUpdateRequiredSignatureFailsClosed(t *testing.T) {
	key := filepath.Join(t.TempDir(), "cosign.pub")
	_ = os.WriteFile(key, []byte("key"), 0o600)
	cases := map[string]struct {
		sig    bool
		cosign bool
		args   []string
		want   string
	}{
		"unsigned release with --cosign-key":      {sig: false, cosign: true, args: []string{"--cosign-key", key}, want: "not signed"},
		"unsigned release with --cosign-identity": {sig: false, cosign: true, args: []string{"--cosign-identity", "x", "--cosign-issuer", "y"}, want: "not signed"},
		"cosign missing with --cosign-key":        {sig: true, cosign: false, args: []string{"--cosign-key", key}, want: "cosign"},
		"identity without issuer":                 {sig: true, cosign: true, args: []string{"--cosign-identity", "x"}, want: "--cosign-issuer"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeReleases(t, "v1.4.0", "v1.4.0")
			if c.sig {
				f.assets["SHA256SUMS.sig"] = []byte("c2ln")
			}
			if c.cosign {
				fakeCosign(t, 0)
			} else {
				t.Setenv("PATH", t.TempDir())
			}
			dest := updateEnv(t, "v1.3.2")
			code, _, errs := runUpdateCmd(t, append([]string{"--api-url", f.srv.URL, "--dest", dest}, c.args...)...)
			if code == 0 || !strings.Contains(errs, c.want) {
				t.Fatalf("expected refusal mentioning %q: code=%d err=%q", c.want, code, errs)
			}
			if b, _ := os.ReadFile(dest); !bytes.Equal(b, newScript("v1.3.2")) {
				t.Error("binary replaced although the required signature check failed")
			}
		})
	}
}
