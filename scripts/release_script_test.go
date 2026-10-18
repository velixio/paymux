package scripts

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// hostTarget is the release target of the machine running the test, or ""
// when release.sh does not build for it.
func hostTarget() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64":
		return runtime.GOOS + "/" + runtime.GOARCH
	}
	return ""
}

func runRelease(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"scripts/release.sh"}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestReleaseScriptBuildsChecksumsAndStampsTheVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	target := hostTarget()
	if target == "" {
		t.Skip("release.sh does not build for this platform")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out := t.TempDir()
	// A shim PATH entry without cosign: signing must be skipped, not fail.
	text, err := runRelease(t, []string{"TARGETS=" + target, "OUT=" + out, "PAYMUX_NO_COSIGN=1"}, "v9.8.7-test")
	if err != nil {
		t.Fatalf("release.sh: %v\n%s", err, text)
	}
	osName, arch, _ := strings.Cut(target, "/")
	// Archive names drop the tag's "v" so they match what `paymux update` looks for.
	tarball := filepath.Join(out, "paymux_9.8.7-test_"+osName+"_"+arch+".tar.gz")
	raw, err := os.ReadFile(tarball)
	if err != nil {
		t.Fatalf("expected %s: %v\n%s", tarball, err, text)
	}

	// SHA256SUMS lists every archive with the right hash, in sha256sum format.
	sums, err := os.Open(filepath.Join(out, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	defer sums.Close()
	h := sha256.Sum256(raw)
	want := hex.EncodeToString(h[:]) + "  " + filepath.Base(tarball)
	found := false
	sc := bufio.NewScanner(sums)
	for sc.Scan() {
		if sc.Text() == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("SHA256SUMS lacks %q", want)
	}

	// The archive holds the binary, and the binary knows its version.
	gz, err := gzip.NewReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	bin := filepath.Join(t.TempDir(), "paymux")
	names := []string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		if hdr.Name == "paymux" {
			f, err := os.OpenFile(bin, os.O_CREATE|os.O_WRONLY, 0o755)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("archive entries %v lack the paymux binary", names)
	}
	got, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil || !strings.Contains(string(got), "v9.8.7-test") {
		t.Fatalf("paymux version = %q (%v)", got, err)
	}
	if !strings.Contains(text, "cosign") {
		t.Errorf("the script should say what it did about signing:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(out, "SHA256SUMS.sig")); err == nil {
		t.Error("nothing should be signed when cosign is skipped")
	}
}

func TestReleaseScriptSignsWithCosignWhenInstalled(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	target := hostTarget()
	if target == "" {
		t.Skip("release.sh does not build for this platform")
	}
	shim := t.TempDir()
	log := filepath.Join(shim, "calls.txt")
	script := "#!/bin/sh\necho \"$@\" >> '" + log + "'\n" +
		// cosign writes the files it is asked to write
		"while [ $# -gt 0 ]; do case \"$1\" in --output-signature) echo sig > \"$2\";; --output-certificate) echo cert > \"$2\";; esac; shift; done\n"
	if err := os.WriteFile(filepath.Join(shim, "cosign"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	text, err := runRelease(t, []string{"TARGETS=" + target, "OUT=" + out, "PATH=" + shim + string(os.PathListSeparator) + os.Getenv("PATH")}, "v1.2.3")
	if err != nil {
		t.Fatalf("release.sh: %v\n%s", err, text)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "sign-blob") || !strings.Contains(string(calls), "SHA256SUMS") {
		t.Fatalf("cosign was not asked to sign SHA256SUMS: %q\n%s", calls, text)
	}
	for _, f := range []string{"SHA256SUMS.sig", "SHA256SUMS.pem"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
}

func TestReleaseScriptRejectsUnknownTargetsAndMissingVersion(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out := t.TempDir()
	if text, err := runRelease(t, []string{"TARGETS=plan9/mips", "OUT=" + out}, "v1.0.0"); err == nil {
		t.Errorf("an unsupported target should fail:\n%s", text)
	}
	if text, err := runRelease(t, []string{"TARGETS=linux/amd64", "OUT=" + out}, "not a version!"); err == nil {
		t.Errorf("a version that is not a plain tag should fail:\n%s", text)
	}
}

// The release script and its outputs are named from the repository root.
func init() {
	if err := os.Chdir(".."); err != nil {
		panic(err)
	}
}
