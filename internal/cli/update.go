package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/selfupdate"
	"github.com/paymux/paymux/pkg/hub"
)

const updateUsage = "usage: paymux update [--check] [--version vX.Y.Z] [--repo owner/name] [--dest path] [--force]\n" +
	"                    [--cosign-key file | --cosign-identity id --cosign-issuer url | --insecure-skip-signature]"

const (
	defaultUpdateRepo = selfupdate.DefaultRepo
	defaultAPIBase    = selfupdate.DefaultAPI
)

// releaseIdentityRegexp, compareVersions: the shared implementations.
func releaseIdentityRegexp(repo string) string { return selfupdate.ReleaseIdentityRegexp(repo) }
func compareVersions(a, b string) int          { return selfupdate.CompareVersions(a, b) }

// currentVersion is the running binary's version (replaced in tests).
var currentVersion = func() string { return hub.Version }

// runUpdate handles "paymux update": find the newest release (or the one
// asked for), download the build for this platform, check it against the
// release's SHA256SUMS and the cosign signature of that file, try the new
// binary, and replace the running one atomically. The signature is checked by
// default, against the identity of the release workflow of --repo; only
// --insecure-skip-signature installs a release that was not verified that way.
func runUpdate(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoDefault := defaultUpdateRepo
	if v, ok := lookup("PAYMUX_UPDATE_REPO"); ok && v != "" {
		repoDefault = v
	}
	check := fs.Bool("check", false, "only report whether a newer release exists")
	repo := fs.String("repo", repoDefault, "GitHub repository of the releases (owner/name)")
	apiURL := fs.String("api-url", defaultAPIBase, "base URL of the releases API")
	target := fs.String("version", "", "install this release tag instead of the latest")
	dest := fs.String("dest", "", "binary to replace (default: the running one)")
	force := fs.Bool("force", false, "update a development build, or reinstall the same or an older release")
	cosignKey := fs.String("cosign-key", "", "cosign public key to verify SHA256SUMS.sig with")
	cosignIdentity := fs.String("cosign-identity", "", "expected signer identity for keyless cosign verification")
	cosignIssuer := fs.String("cosign-issuer", "", "expected OIDC issuer for keyless cosign verification")
	skipSig := fs.Bool("insecure-skip-signature", false, "install without verifying the cosign signature (the SHA-256 check still applies)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || !strings.Contains(*repo, "/") || *skipSig && (*cosignKey != "" || *cosignIdentity != "") {
		fmt.Fprintln(stderr, updateUsage)
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "paymux update: "+format+"\n", a...)
		return 1
	}
	token, _ := lookup("GITHUB_TOKEN")
	u := &selfupdate.Client{API: strings.TrimRight(*apiURL, "/"), Token: token, HTTP: &http.Client{Timeout: 10 * time.Minute}}

	cur := currentVersion()
	dev := !selfupdate.Parsable(cur)
	rel, err := u.FetchRelease(ctx, *repo, *target)
	if err != nil {
		return fail("%v", err)
	}
	if *check {
		switch {
		case dev:
			fmt.Fprintf(stdout, "this is a development build (%s); the latest release is %s\n", cur, rel.Tag)
		case compareVersions(cur, rel.Tag) < 0:
			fmt.Fprintf(stdout, "update available: %s -> %s (run \"paymux update\")\n", cur, rel.Tag)
		default:
			fmt.Fprintf(stdout, "paymux %s is up to date (latest release %s)\n", cur, rel.Tag)
		}
		return 0
	}
	if dev && !*force {
		return fail("this is a development build (version %q), so it cannot tell whether it is current; use --force to install %s anyway", cur, rel.Tag)
	}
	if !dev && !*force && compareVersions(cur, rel.Tag) >= 0 {
		fmt.Fprintf(stdout, "paymux %s is up to date (latest release %s)\n", cur, rel.Tag)
		return 0
	}

	exe := *dest
	if exe == "" {
		if exe, err = selfupdate.ExecutablePath(); err != nil {
			return fail("cannot find the running binary: %v (use --dest)", err)
		}
	}
	res, err := u.Install(ctx, rel, selfupdate.InstallOptions{Repo: *repo, Dest: exe, Current: cur,
		CosignKey: *cosignKey, CosignIdentity: *cosignIdentity, CosignIssuer: *cosignIssuer, SkipSignature: *skipSig,
		Logf: func(format string, a ...any) { fmt.Fprintf(stderr, "paymux update: "+format+"\n", a...) }})
	if err != nil {
		return fail("%v", err)
	}
	exe, prev := res.Exe, res.Prev
	fmt.Fprintf(stdout, "updated paymux %s -> %s (%s)\n", cur, rel.Tag, exe)
	if prev != "" {
		fmt.Fprintf(stdout, "the previous binary is kept at %s; to go back, move it over %s\n", prev, exe)
	}
	fmt.Fprintln(stdout, "restart to run the new version:")
	fmt.Fprintln(stdout, "  systemd:         systemctl restart paymux")
	fmt.Fprintln(stdout, "  by hand:         stop \"paymux serve\" and start it again")
	if _, err := os.Stat("/.dockerenv"); err == nil {
		fmt.Fprintln(stdout, "  note: this looks like a container; the next image pull replaces the binary again, so prefer updating the image")
	}
	fmt.Fprintln(stdout, "pending database migrations are applied on the next start; see docs/upgrades.md (take a backup first)")
	return 0
}
