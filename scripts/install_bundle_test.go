package scripts

import (
	"os"
	"strings"
	"testing"
)

// A release signed by cosign v3 carries SHA256SUMS.sigstore.json instead of
// SHA256SUMS.sig and SHA256SUMS.pem; install.sh verifies the bundle, with the
// same release identity, and prefers it when a release has both.
func TestInstallScriptVerifiesTheSigstoreBundle(t *testing.T) {
	f := newFakeRelease(t, false)
	for _, dir := range []string{"latest/download/", "download/" + fakeTag + "/"} {
		f.files[dir+"SHA256SUMS.sigstore.json"] = []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)
	}
	bin, argsFile := fakeCosignShim(t, 0)
	r := runInstall(t, f, "PAYMUX_COSIGN="+bin, "PAYMUX_REQUIRE_SIGNATURE=1")
	if r.err != nil || !r.installed(t) {
		t.Fatalf("verified install failed: %v %q", r.err, r.errs)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"verify-blob", "--bundle ", "--new-bundle-format", `--certificate-identity-regexp ^https://github\.com/velixio/paymux/\.github/workflows/release\.yml@refs/tags/v[^/]+$`,
		"--certificate-oidc-issuer https://token.actions.githubusercontent.com"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("cosign args lack %q: %s", want, args)
		}
	}
	if strings.Contains(string(args), "--signature") {
		t.Errorf("a bundle is verified as a bundle: %s", args)
	}
	// With the old pair too, the bundle still wins.
	for _, dir := range []string{"latest/download/", "download/" + fakeTag + "/"} {
		f.files[dir+"SHA256SUMS.sig"] = []byte("c2ln")
		f.files[dir+"SHA256SUMS.pem"] = []byte("cert")
	}
	bin, argsFile = fakeCosignShim(t, 0)
	if r := runInstall(t, f, "PAYMUX_COSIGN="+bin); r.err != nil {
		t.Fatalf("install with both: %v %q", r.err, r.errs)
	}
	if args, _ := os.ReadFile(argsFile); !strings.Contains(string(args), "--bundle ") {
		t.Errorf("the bundle was not preferred: %s", args)
	}
	// A bundle that does not verify is fatal.
	bad, _ := fakeCosignShim(t, 1)
	if r := runInstall(t, f, "PAYMUX_COSIGN="+bad); r.err == nil || r.installed(t) || !strings.Contains(r.errs, "did not verify") {
		t.Fatalf("bad bundle: err=%v installed=%v stderr=%q", r.err, r.installed(t), r.errs)
	}
}
