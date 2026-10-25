package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Audit H-11: "paymux update" verifies the cosign signature of SHA256SUMS by
// default, against the identity of this repository's release workflow, and
// installs an unsigned release only when told to with --insecure-skip-signature.

func signedRelease(t *testing.T) *fakeReleases {
	f := newFakeReleases(t, "v1.4.0", "v1.4.0")
	f.assets["SHA256SUMS.sig"] = []byte("c2lnbmF0dXJl")
	f.assets["SHA256SUMS.pem"] = []byte("-----BEGIN CERTIFICATE-----\n")
	return f
}

func TestUpdateReleaseIdentityRegexp(t *testing.T) {
	re := regexp.MustCompile(releaseIdentityRegexp("velixio/paymux"))
	for id, want := range map[string]bool{
		"https://github.com/velixio/paymux/.github/workflows/release.yml@refs/tags/v1.4.0":      true,
		"https://github.com/velixio/paymux/.github/workflows/release.yml@refs/tags/v1.4.0-rc.1": true,
		"https://github.com/velixio/paymux/.github/workflows/release.yml@refs/heads/main":       false,
		"https://github.com/velixio/paymux/.github/workflows/ci.yml@refs/tags/v1.4.0":           false,
		"https://github.com/velixio/paymux-evil/.github/workflows/release.yml@refs/tags/v1.4.0": false,
		"https://github.com/evil/velixio/paymux/.github/workflows/release.yml@refs/tags/v1.4.0": false,
		"https://github.com/velixioXpaymux/.github/workflows/release.yml@refs/tags/v1.4.0":      false,
		"https://github.com/velixio/paymux/.github/workflows/release.yml@refs/tags/v1.4.0/x":    false,
	} {
		if got := re.MatchString(id); got != want {
			t.Errorf("%s: match=%v, want %v", id, got, want)
		}
	}
	other := regexp.MustCompile(releaseIdentityRegexp("acme/fork"))
	if !other.MatchString("https://github.com/acme/fork/.github/workflows/release.yml@refs/tags/v2.0.0") || other.MatchString("https://github.com/velixio/paymux/.github/workflows/release.yml@refs/tags/v1.4.0") {
		t.Error("the identity follows --repo")
	}
}

func TestUpdateVerifiesTheSignatureByDefault(t *testing.T) {
	f := signedRelease(t)
	argsFile := fakeCosign(t, 0)
	dest := updateEnv(t, "v1.3.2")
	code, out, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest)
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"verify-blob", "--certificate-identity-regexp", `velixio/paymux/\.github/workflows/release\.yml@refs/tags/`,
		"--certificate-oidc-issuer https://token.actions.githubusercontent.com", "--certificate ", "--signature"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("cosign args lack %q: %s", want, args)
		}
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, newScript("v1.4.0")) {
		t.Error("a verified release was not installed")
	}
}

func TestUpdateRefusesWhenTheDefaultVerificationFails(t *testing.T) {
	cases := map[string]struct {
		mutate func(f *fakeReleases)
		cosign int // -1: not installed
		want   string
	}{
		"unsigned release":       {func(f *fakeReleases) { delete(f.assets, "SHA256SUMS.sig"); delete(f.assets, "SHA256SUMS.pem") }, 0, "not signed"},
		"wrong signature":        {func(f *fakeReleases) {}, 1, "did not verify"},
		"cosign not installed":   {func(f *fakeReleases) {}, -1, "cosign is not installed"},
		"signature without cert": {func(f *fakeReleases) { delete(f.assets, "SHA256SUMS.pem") }, 0, "SHA256SUMS.pem"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := signedRelease(t)
			c.mutate(f)
			if c.cosign < 0 {
				t.Setenv("PATH", t.TempDir())
			} else {
				fakeCosign(t, c.cosign)
			}
			dest := updateEnv(t, "v1.3.2")
			code, _, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest)
			if code == 0 || !strings.Contains(errs, c.want) || !strings.Contains(errs, "--insecure-skip-signature") {
				t.Fatalf("expected a refusal mentioning %q and the escape hatch: code=%d err=%q", c.want, code, errs)
			}
			if b, _ := os.ReadFile(dest); !bytes.Equal(b, newScript("v1.3.2")) {
				t.Error("the binary was replaced although the signature was not verified")
			}
		})
	}
}

func TestUpdateInsecureSkipSignature(t *testing.T) {
	f := newFakeReleases(t, "v1.4.0", "v1.4.0") // unsigned
	argsFile := fakeCosign(t, 1)                // would fail if it were asked
	dest := updateEnv(t, "v1.3.2")
	code, out, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--insecure-skip-signature")
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if !strings.Contains(strings.ToLower(errs), "not verified") || !strings.Contains(errs, "--insecure-skip-signature") {
		t.Errorf("no warning that the signature was skipped: %q", errs)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Error("cosign was run although the check was skipped")
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, newScript("v1.4.0")) {
		t.Error("the release was not installed")
	}
	// The checksum still decides: a skipped signature is not a skipped hash.
	f.assets["SHA256SUMS"] = []byte(strings.Repeat("a", 64) + "  " + f.assetName() + "\n")
	dest = updateEnv(t, "v1.3.2")
	if code, _, errs = runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--insecure-skip-signature"); code == 0 || !strings.Contains(errs, "checksum") {
		t.Errorf("checksum mismatch with the signature skipped: code=%d err=%q", code, errs)
	}
	// Skipping and asking for a particular check contradict each other.
	key := filepath.Join(t.TempDir(), "cosign.pub")
	if code, _, _ = runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--insecure-skip-signature", "--cosign-key", key); code != 2 {
		t.Errorf("--insecure-skip-signature with --cosign-key: code=%d, want 2", code)
	}
}

// Releases also carry version-less archives (paymux_linux_amd64.tar.gz) for the
// stable "latest" download links. The updater must keep to the versioned name:
// it is the one the release workflow's SHA256SUMS lists next to the tag.
func TestUpdateUsesTheVersionedArchiveNotTheStableAlias(t *testing.T) {
	f := newFakeReleases(t, "v1.4.0", "v1.4.0")
	stable := tarGz(t, "paymux", newScript("v6.6.6")) // a decoy: it would fail the version check
	name := "paymux_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	f.assets[name] = stable
	f.assets["SHA256SUMS"] = append(f.assets["SHA256SUMS"], []byte(sum(stable)+"  "+name+"\n")...)
	dest := updateEnv(t, "v1.3.2")
	code, out, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--insecure-skip-signature")
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, newScript("v1.4.0")) {
		t.Errorf("installed %q, want the versioned archive's binary", b)
	}
}
