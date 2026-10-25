package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fakeRelease serves what install.sh downloads, under the layout of GitHub:
// releases/latest/download/<file> and releases/download/<tag>/<file>.
type fakeRelease struct {
	srv    *httptest.Server
	mu     sync.Mutex
	files  map[string][]byte // path under /releases/ -> body
	served []string
}

const fakeTag = "v1.2.3"

func installPlatform() (string, string) {
	osName, arch := runtime.GOOS, runtime.GOARCH
	return osName, arch
}

func fakeArchive(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho \"paymux " + version + "\"\n")
	for _, f := range []struct {
		name string
		body []byte
		mode int64
	}{{"LICENSE", []byte("license"), 0o644}, {"paymux", body, 0o755}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func newFakeRelease(t *testing.T, signed bool) *fakeRelease {
	t.Helper()
	osName, arch := installPlatform()
	archive := fakeArchive(t, fakeTag)
	versioned := fmt.Sprintf("paymux_%s_%s_%s.tar.gz", strings.TrimPrefix(fakeTag, "v"), osName, arch)
	stable := fmt.Sprintf("paymux_%s_%s.tar.gz", osName, arch)
	h := sha256.Sum256(archive)
	hexsum := hex.EncodeToString(h[:])
	sums := []byte(hexsum + "  " + versioned + "\n" + hexsum + "  " + stable + "\n" + strings.Repeat("0", 64) + "  paymux_other_os_arch.tar.gz\n")
	f := &fakeRelease{files: map[string][]byte{}}
	for _, dir := range []string{"latest/download/", "download/" + fakeTag + "/"} {
		f.files[dir+stable] = archive
		f.files[dir+versioned] = archive
		f.files[dir+"SHA256SUMS"] = sums
		if signed {
			f.files[dir+"SHA256SUMS.sig"] = []byte("c2ln")
			f.files[dir+"SHA256SUMS.pem"] = []byte("cert")
		}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.served = append(f.served, r.URL.Path)
		f.mu.Unlock()
		body, ok := f.files[strings.TrimPrefix(r.URL.Path, "/releases/")]
		if !ok || !strings.HasPrefix(r.URL.Path, "/releases/") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelease) base() string { return f.srv.URL + "/releases" }

type installRun struct {
	dir       string
	out, errs string
	err       error
}

// runInstall runs install.sh against the fake release with a clean environment
// and its own install directory. extra are further NAME=value settings.
func runInstall(t *testing.T, f *fakeRelease, extra ...string) installRun {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append(os.Environ(), "PAYMUX_DOWNLOAD_BASE="+f.base(), "PAYMUX_INSTALL_DIR="+dir, "PAYMUX_VERSION=", "PAYMUX_REQUIRE_SIGNATURE=", "PAYMUX_USE_SUDO=")
	cmd.Env = append(cmd.Env, extra...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	return installRun{dir: dir, out: so.String(), errs: se.String(), err: err}
}

func (r installRun) installed(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(r.dir, "paymux"))
	return err == nil
}

func leftovers(t *testing.T, dir string) []string {
	var names []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// fakeCosignShim returns the path of a cosign stand-in that records its
// arguments and exits with code.
func fakeCosignShim(t *testing.T, code int) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	bin = filepath.Join(dir, "cosign")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" > '%s'\nexit %d\n", argsFile, code)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

const noCosign = "PAYMUX_COSIGN=/nonexistent/cosign"

func TestInstallScriptInstallsTheLatestRelease(t *testing.T) {
	f := newFakeRelease(t, true)
	r := runInstall(t, f, noCosign)
	if r.err != nil {
		t.Fatalf("install.sh: %v\nstdout: %s\nstderr: %s", r.err, r.out, r.errs)
	}
	got, err := exec.Command(filepath.Join(r.dir, "paymux"), "version").Output()
	if err != nil || strings.TrimSpace(string(got)) != "paymux "+fakeTag {
		t.Fatalf("installed binary says %q (%v)", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(r.dir, "paymux")); fi.Mode().Perm()&0o111 == 0 {
		t.Error("the binary is not executable")
	}
	if names := leftovers(t, r.dir); len(names) != 1 {
		t.Errorf("files left in the install directory: %v", names)
	}
	// "latest" uses the stable names.
	osName, arch := installPlatform()
	f.mu.Lock()
	served := strings.Join(f.served, " ")
	f.mu.Unlock()
	if !strings.Contains(served, "/releases/latest/download/paymux_"+osName+"_"+arch+".tar.gz") || !strings.Contains(served, "/releases/latest/download/SHA256SUMS") {
		t.Errorf("did not use the latest/download links: %s", served)
	}
}

func TestInstallScriptRefusesAChecksumMismatch(t *testing.T) {
	f := newFakeRelease(t, true)
	osName, arch := installPlatform()
	f.files["latest/download/paymux_"+osName+"_"+arch+".tar.gz"] = fakeArchive(t, "v6.6.6") // not what SHA256SUMS says
	r := runInstall(t, f, noCosign)
	if r.err == nil || !strings.Contains(strings.ToLower(r.errs), "checksum") {
		t.Fatalf("expected a refusal naming the checksum: err=%v stderr=%q", r.err, r.errs)
	}
	if r.installed(t) || len(leftovers(t, r.dir)) != 0 {
		t.Errorf("something was installed or left behind: %v", leftovers(t, r.dir))
	}
}

func TestInstallScriptRefusesWhenSHA256SUMSHasNoEntry(t *testing.T) {
	f := newFakeRelease(t, true)
	f.files["latest/download/SHA256SUMS"] = []byte(strings.Repeat("a", 64) + "  something_else.tar.gz\n")
	if r := runInstall(t, f, noCosign); r.err == nil || r.installed(t) {
		t.Fatalf("installed without a checksum entry: err=%v stderr=%q", r.err, r.errs)
	}
}

func TestInstallScriptWarnsWhenCosignIsMissing(t *testing.T) {
	f := newFakeRelease(t, true)
	r := runInstall(t, f, noCosign)
	if r.err != nil || !r.installed(t) {
		t.Fatalf("a missing cosign must not block the install: %v %q", r.err, r.errs)
	}
	low := strings.ToLower(r.errs)
	if !strings.Contains(low, "cosign") || !strings.Contains(low, "not verified") || !strings.Contains(r.errs, "cosign verify-blob") {
		t.Errorf("no clear warning with the way to verify by hand: %q", r.errs)
	}
}

func TestInstallScriptCanRequireTheSignature(t *testing.T) {
	f := newFakeRelease(t, true)
	if r := runInstall(t, f, noCosign, "PAYMUX_REQUIRE_SIGNATURE=1"); r.err == nil || r.installed(t) || !strings.Contains(r.errs, "PAYMUX_REQUIRE_SIGNATURE") {
		t.Fatalf("PAYMUX_REQUIRE_SIGNATURE=1 without cosign: err=%v installed=%v stderr=%q", r.err, r.installed(t), r.errs)
	}
	// An unsigned release is refused as well.
	u := newFakeRelease(t, false)
	bin, _ := fakeCosignShim(t, 0)
	if r := runInstall(t, u, "PAYMUX_COSIGN="+bin, "PAYMUX_REQUIRE_SIGNATURE=1"); r.err == nil || r.installed(t) {
		t.Fatalf("PAYMUX_REQUIRE_SIGNATURE=1 with an unsigned release: err=%v stderr=%q", r.err, r.errs)
	}
	// And without the requirement an unsigned release installs with a warning.
	if r := runInstall(t, u, noCosign); r.err != nil || !r.installed(t) || !strings.Contains(strings.ToLower(r.errs), "not signed") {
		t.Fatalf("unsigned release: err=%v stderr=%q", r.err, r.errs)
	}
}

func TestInstallScriptVerifiesTheSignatureWithTheReleaseIdentity(t *testing.T) {
	f := newFakeRelease(t, true)
	bin, argsFile := fakeCosignShim(t, 0)
	r := runInstall(t, f, "PAYMUX_COSIGN="+bin, "PAYMUX_REQUIRE_SIGNATURE=1")
	if r.err != nil || !r.installed(t) {
		t.Fatalf("verified install failed: %v %q", r.err, r.errs)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"verify-blob", `--certificate-identity-regexp ^https://github\.com/velixio/paymux/\.github/workflows/release\.yml@refs/tags/v[^/]+$`,
		"--certificate-oidc-issuer https://token.actions.githubusercontent.com", "--certificate ", "--signature "} {
		if !strings.Contains(string(args), want) {
			t.Errorf("cosign args lack %q: %s", want, args)
		}
	}
	// A signature that does not verify is fatal, with or without the requirement.
	bad, _ := fakeCosignShim(t, 1)
	if r := runInstall(t, f, "PAYMUX_COSIGN="+bad); r.err == nil || r.installed(t) || !strings.Contains(r.errs, "did not verify") {
		t.Fatalf("bad signature: err=%v installed=%v stderr=%q", r.err, r.installed(t), r.errs)
	}
}

func TestInstallScriptPinsAVersion(t *testing.T) {
	f := newFakeRelease(t, true)
	for _, v := range []string{fakeTag, strings.TrimPrefix(fakeTag, "v")} {
		r := runInstall(t, f, noCosign, "PAYMUX_VERSION="+v)
		if r.err != nil || !r.installed(t) {
			t.Fatalf("PAYMUX_VERSION=%s: %v %q", v, r.err, r.errs)
		}
	}
	osName, arch := installPlatform()
	f.mu.Lock()
	served := strings.Join(f.served, " ")
	f.mu.Unlock()
	if !strings.Contains(served, "/releases/download/"+fakeTag+"/paymux_1.2.3_"+osName+"_"+arch+".tar.gz") {
		t.Errorf("a pinned version must use the versioned archive under download/<tag>/: %s", served)
	}
	// A version that does not exist is an error, and a malformed one is refused before any download.
	if r := runInstall(t, f, noCosign, "PAYMUX_VERSION=v9.9.9"); r.err == nil || r.installed(t) {
		t.Errorf("unknown version installed: %v", r.errs)
	}
	for _, bad := range []string{"../x", "v1.2.3;rm -rf /", "1 2"} {
		if r := runInstall(t, f, noCosign, "PAYMUX_VERSION="+bad); r.err == nil || r.installed(t) {
			t.Errorf("PAYMUX_VERSION=%q was accepted", bad)
		}
	}
}

func TestInstallScriptRefusesUnsupportedPlatforms(t *testing.T) {
	f := newFakeRelease(t, true)
	for _, c := range []struct{ os, arch, want string }{{"FreeBSD", "x86_64", "FreeBSD"}, {"Linux", "mips", "mips"}, {"Darwin", "ppc", "ppc"}} {
		shim := t.TempDir()
		script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in -s) echo %s;; -m) echo %s;; *) echo %s;; esac\n", c.os, c.arch, c.os)
		if err := os.WriteFile(filepath.Join(shim, "uname"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		r := runInstall(t, f, noCosign, "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
		if r.err == nil || !strings.Contains(r.errs, c.want) || !strings.Contains(strings.ToLower(r.errs), "unsupported") || r.installed(t) {
			t.Errorf("%s/%s: err=%v stderr=%q", c.os, c.arch, r.err, r.errs)
		}
	}
}

func TestInstallScriptUsesSudoOnlyWhenAsked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	f := newFakeRelease(t, true)
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)
	r := runInstall(t, f, noCosign, "PAYMUX_INSTALL_DIR="+locked)
	if r.err == nil || !strings.Contains(r.errs, "PAYMUX_USE_SUDO") || !strings.Contains(r.errs, "PAYMUX_INSTALL_DIR") {
		t.Fatalf("an unwritable directory must be refused with the two ways out: err=%v stderr=%q", r.err, r.errs)
	}
	// With PAYMUX_USE_SUDO=1 it goes through sudo (here a stand-in that records the call and runs the command).
	shim := t.TempDir()
	log := filepath.Join(shim, "sudo.log")
	script := "#!/bin/sh\necho \"$@\" >> '" + log + "'\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The stand-in does not raise privileges, so use a directory that is writable for the final check only.
	open := filepath.Join(t.TempDir(), "open")
	_ = os.Mkdir(open, 0o755)
	r = runInstall(t, f, noCosign, "PAYMUX_INSTALL_DIR="+open, "PAYMUX_USE_SUDO=1", "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	if r.err != nil {
		t.Fatalf("PAYMUX_USE_SUDO=1: %v %q", r.err, r.errs)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("sudo was used for a directory that is writable: %s", b)
	}
}

// install.sh is part of every release: release.sh publishes it next to the
// archives and lists it in SHA256SUMS, so the signature and the attestation
// cover it.
func TestReleaseScriptPublishesInstallSh(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	target := hostTarget()
	if target == "" {
		t.Skip("release.sh does not build for this platform")
	}
	out := t.TempDir()
	if text, err := runRelease(t, []string{"TARGETS=" + target, "OUT=" + out, "PAYMUX_NO_COSIGN=1"}, "v9.8.7-test"); err != nil {
		t.Fatalf("release.sh: %v\n%s", err, text)
	}
	got, err := os.ReadFile(filepath.Join(out, "install.sh"))
	if err != nil {
		t.Fatalf("install.sh is not among the release files: %v", err)
	}
	want, _ := os.ReadFile("install.sh")
	if !bytes.Equal(got, want) {
		t.Error("the published install.sh differs from the one in the repository")
	}
	h := sha256.Sum256(got)
	sums, _ := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if !strings.Contains(string(sums), hex.EncodeToString(h[:])+"  install.sh\n") {
		t.Errorf("SHA256SUMS does not cover install.sh:\n%s", sums)
	}
}

func TestInstallScriptPassesShellcheck(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
	if out, err := exec.Command(sc, "-s", "sh", "install.sh").CombinedOutput(); err != nil {
		t.Errorf("shellcheck:\n%s", out)
	}
}
