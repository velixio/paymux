package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Releases signed by cosign v3 carry one Sigstore bundle, SHA256SUMS.sigstore.json,
// instead of SHA256SUMS.sig and SHA256SUMS.pem (cosign v3 cannot write those).
// "paymux update" verifies the bundle against the release workflow's identity,
// and still verifies the old pair for releases published before.

func bundleRelease(t *testing.T) *fakeReleases {
	f := newFakeReleases(t, "v1.4.0", "v1.4.0")
	f.assets["SHA256SUMS.sigstore.json"] = []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)
	return f
}

func TestUpdateVerifiesTheSigstoreBundleByDefault(t *testing.T) {
	f := bundleRelease(t)
	argsFile := fakeCosign(t, 0)
	dest := updateEnv(t, "v1.3.2")
	code, out, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest)
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"verify-blob", "--bundle ", "--new-bundle-format", "--certificate-identity-regexp",
		`velixio/paymux/\.github/workflows/release\.yml@refs/tags/`, "--certificate-oidc-issuer https://token.actions.githubusercontent.com"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("cosign args lack %q: %s", want, args)
		}
	}
	for _, unwanted := range []string{"--signature", "--certificate "} {
		if strings.Contains(string(args), unwanted) {
			t.Errorf("cosign args carry %q for a bundle: %s", unwanted, args)
		}
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, newScript("v1.4.0")) {
		t.Error("a verified release was not installed")
	}
}

func TestUpdatePrefersTheBundleToTheOldSignature(t *testing.T) {
	f := bundleRelease(t)
	f.assets["SHA256SUMS.sig"] = []byte("c2lnbmF0dXJl")
	f.assets["SHA256SUMS.pem"] = []byte("-----BEGIN CERTIFICATE-----\n")
	argsFile := fakeCosign(t, 0)
	dest := updateEnv(t, "v1.3.2")
	if code, out, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest); code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--bundle ") || strings.Contains(string(args), "--signature") {
		t.Errorf("the bundle was not preferred: %s", args)
	}
}

func TestUpdateVerifiesABundleAgainstAKey(t *testing.T) {
	f := bundleRelease(t)
	argsFile := fakeCosign(t, 0)
	dest := updateEnv(t, "v1.3.2")
	key := filepath.Join(t.TempDir(), "cosign.pub")
	_ = os.WriteFile(key, []byte("key"), 0o600)
	if code, out, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest, "--cosign-key", key); code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"--bundle ", "--new-bundle-format", "--key " + key} {
		if !strings.Contains(string(args), want) {
			t.Errorf("cosign args lack %q: %s", want, args)
		}
	}
}

func TestUpdateRefusesABundleThatDoesNotVerify(t *testing.T) {
	f := bundleRelease(t)
	fakeCosign(t, 1)
	dest := updateEnv(t, "v1.3.2")
	code, _, errs := runUpdateCmd(t, "--api-url", f.srv.URL, "--dest", dest)
	if code == 0 || !strings.Contains(errs, "did not verify") {
		t.Fatalf("a bad bundle must refuse: code=%d err=%q", code, errs)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, newScript("v1.3.2")) {
		t.Error("the binary was replaced although the bundle did not verify")
	}
}
