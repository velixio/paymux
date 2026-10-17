package cli

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/paymux/paymux"
)

const updateUsage = "usage: paymux update [--check] [--version vX.Y.Z] [--repo owner/name] [--dest path] [--force]\n" +
	"                    [--cosign-key file | --cosign-identity id --cosign-issuer url]"

const (
	defaultUpdateRepo = "velixio/paymux"
	defaultAPIBase    = "https://api.github.com"
	maxArchive        = 300 << 20
	maxBinary         = 500 << 20
	maxSmallAsset     = 1 << 20
)

// currentVersion is the running binary's version (replaced in tests).
var currentVersion = func() string { return paymux.Version }

// compareVersions orders two release tags ("v1.2.3", "1.2.3-rc.1"): -1, 0 or
// 1. A pre-release sorts before its release.
func compareVersions(a, b string) int {
	ac, ap := splitVersion(a)
	bc, bp := splitVersion(b)
	for i := 0; i < max(len(ac), len(bc)); i++ {
		var x, y int
		if i < len(ac) {
			x = ac[i]
		}
		if i < len(bc) {
			y = bc[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case ap == "" && bp == "":
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	}
	as, bs := strings.Split(ap, "."), strings.Split(bp, ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		if i >= len(as) {
			return -1
		}
		if i >= len(bs) {
			return 1
		}
		xn, xerr := strconv.Atoi(as[i])
		yn, yerr := strconv.Atoi(bs[i])
		switch {
		case xerr == nil && yerr == nil && xn != yn:
			if xn < yn {
				return -1
			}
			return 1
		case xerr == nil && yerr != nil:
			return -1
		case xerr != nil && yerr == nil:
			return 1
		case xerr != nil && yerr != nil && as[i] != bs[i]:
			if as[i] < bs[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func splitVersion(v string) (core []int, pre string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "+")
	v, pre, _ = strings.Cut(v, "-")
	for _, p := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(p)
		core = append(core, n)
	}
	return core, pre
}

func parsableVersion(v string) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	return v != "" && v[0] >= '0' && v[0] <= '9'
}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type release struct {
	Tag        string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

func (r release) asset(name string) (releaseAsset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return releaseAsset{}, false
}

// runUpdate handles "paymux update": find the newest release (or the one
// asked for), download the build for this platform, check it against the
// release's SHA256SUMS (and its cosign signature when there is one and cosign
// can check it), try the new binary, and replace the running one atomically.
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || !strings.Contains(*repo, "/") {
		fmt.Fprintln(stderr, updateUsage)
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "paymux update: "+format+"\n", a...)
		return 1
	}
	token, _ := lookup("GITHUB_TOKEN")
	u := &updater{ctx: ctx, api: strings.TrimRight(*apiURL, "/"), token: token, client: &http.Client{Timeout: 10 * time.Minute}}

	cur := currentVersion()
	dev := !parsableVersion(cur)
	rel, err := u.fetchRelease(*repo, *target)
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
		if exe, err = os.Executable(); err != nil {
			return fail("cannot find the running binary: %v (use --dest)", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fail("%v", err)
	}
	dir := filepath.Dir(exe)

	ver := strings.TrimPrefix(rel.Tag, "v")
	archiveName := fmt.Sprintf("paymux_%s_%s_%s.tar.gz", ver, runtime.GOOS, runtime.GOARCH)
	archive, ok := rel.asset(archiveName)
	if !ok {
		var names []string
		for _, a := range rel.Assets {
			names = append(names, a.Name)
		}
		return fail("release %s has no build for %s/%s (looked for %s; it has %s)", rel.Tag, runtime.GOOS, runtime.GOARCH, archiveName, strings.Join(names, ", "))
	}
	sumsAsset, ok := rel.asset("SHA256SUMS")
	if !ok {
		return fail("release %s has no SHA256SUMS, so the download cannot be verified; refusing to install it", rel.Tag)
	}

	fmt.Fprintf(stderr, "paymux update: %s -> %s (%s)\n", cur, rel.Tag, archiveName)
	sums, err := u.download(sumsAsset.URL, maxSmallAsset)
	if err != nil {
		return fail("downloading SHA256SUMS: %v", err)
	}
	want, ok := parseSums(string(sums))[archiveName]
	if !ok {
		return fail("SHA256SUMS has no entry for %s; refusing to install it", archiveName)
	}

	tmpArchive, err := os.CreateTemp(dir, ".paymux-update-*")
	if err != nil {
		return fail("cannot write to %s: %v (run as a user who can, or install elsewhere with --dest)", dir, err)
	}
	defer os.Remove(tmpArchive.Name())
	got, err := u.downloadTo(archive.URL, tmpArchive, maxArchive)
	tmpArchive.Close()
	if err != nil {
		return fail("downloading %s: %v", archiveName, err)
	}
	if got != want {
		return fail("checksum mismatch for %s: SHA256SUMS says %s, the download is %s; nothing was installed", archiveName, want, got)
	}
	fmt.Fprintf(stderr, "paymux update: checksum verified (%s)\n", got[:12])

	// Asking for a signature check makes it mandatory: never fall back to
	// checksums when the operator supplied a key or identity.
	wantSig := *cosignKey != "" || *cosignIdentity != ""
	sig, signed := rel.asset("SHA256SUMS.sig")
	switch {
	case wantSig && !signed:
		return fail("release %s is not signed but --cosign-key/--cosign-identity was given; refusing to install", rel.Tag)
	case signed:
		msg, err := u.verifySignature(sig, rel, sums, *cosignKey, *cosignIdentity, *cosignIssuer)
		if err != nil {
			return fail("%v", err)
		}
		fmt.Fprintf(stderr, "paymux update: %s\n", msg)
	}

	tmpBin, err := os.CreateTemp(dir, ".paymux-update-*")
	if err != nil {
		return fail("cannot write to %s: %v", dir, err)
	}
	defer os.Remove(tmpBin.Name())
	if err := extractBinary(tmpArchive.Name(), "paymux", tmpBin); err != nil {
		tmpBin.Close()
		return fail("%s: %v", archiveName, err)
	}
	if err := tmpBin.Close(); err != nil {
		return fail("%v", err)
	}
	if err := os.Chmod(tmpBin.Name(), 0o755); err != nil {
		return fail("%v", err)
	}
	if err := selfTest(ctx, tmpBin.Name(), rel.Tag); err != nil {
		return fail("the downloaded binary failed its check, nothing was installed: %v", err)
	}

	prev := exe + ".prev"
	_ = os.Remove(prev)
	if err := os.Link(exe, prev); err != nil {
		prev = "" // not fatal: the old binary is just not kept
	}
	if err := os.Rename(tmpBin.Name(), exe); err != nil {
		return fail("replacing %s: %v", exe, err)
	}
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

type updater struct {
	ctx    context.Context
	api    string
	token  string
	client *http.Client
}

func (u *updater) get(rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(u.ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "paymux-update")
	req.Header.Set("Accept", "application/vnd.github+json, application/octet-stream")
	if pu, err := url.Parse(rawURL); err == nil {
		if au, err := url.Parse(u.api); err == nil && pu.Host == au.Host && u.token != "" {
			req.Header.Set("Authorization", "Bearer "+u.token) // never to other hosts
		}
	}
	return u.client.Do(req)
}

func (u *updater) fetchRelease(repo, tag string) (release, error) {
	endpoint := u.api + "/repos/" + repo + "/releases/latest"
	if tag != "" {
		endpoint = u.api + "/repos/" + repo + "/releases/tags/" + url.PathEscape(tag)
	}
	res, err := u.get(endpoint)
	if err != nil {
		return release{}, fmt.Errorf("asking %s for the release: %w", u.api, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		if tag != "" {
			return release{}, fmt.Errorf("release %s not found in %s", tag, repo)
		}
		return release{}, fmt.Errorf("no release found in %s", repo)
	}
	if res.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("the releases API answered %s", res.Status)
	}
	var rel release
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&rel); err != nil {
		return release{}, fmt.Errorf("reading the release: %w", err)
	}
	if rel.Tag == "" || rel.Draft {
		return release{}, errors.New("the release has no usable tag")
	}
	return rel, nil
}

func (u *updater) checkURL(raw string) error {
	pu, err := url.Parse(raw)
	if err != nil {
		return err
	}
	au, _ := url.Parse(u.api)
	if pu.Scheme != "https" && !(au != nil && au.Scheme == "http" && pu.Scheme == "http") {
		return fmt.Errorf("refusing to download from %q: not https", raw)
	}
	return nil
}

func (u *updater) download(raw string, limit int64) ([]byte, error) {
	if err := u.checkURL(raw); err != nil {
		return nil, err
	}
	res, err := u.get(raw)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("larger than the %d byte limit", limit)
	}
	return b, nil
}

// downloadTo streams a download into f and returns its SHA-256.
func (u *updater) downloadTo(raw string, f *os.File, limit int64) (string, error) {
	if err := u.checkURL(raw); err != nil {
		return "", err
	}
	res, err := u.get(raw)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s", res.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, limit+1))
	if err != nil {
		return "", err
	}
	if n > limit {
		return "", fmt.Errorf("larger than the %d byte limit", limit)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parseSums reads sha256sum output: "<hex>  <name>" (or "<hex> *<name>").
func parseSums(s string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		hash, name, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		name = strings.TrimPrefix(strings.TrimSpace(name), "*")
		if ok && len(hash) == 64 && name != "" {
			out[name] = strings.ToLower(hash)
		}
	}
	return out
}

// extractBinary copies the regular file called name out of a .tar.gz.
func extractBinary(archive, name string, dst io.Writer) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("the archive has no %q file", name)
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || path.Base(h.Name) != name {
			continue
		}
		n, err := io.Copy(dst, io.LimitReader(tr, maxBinary+1))
		if err != nil {
			return err
		}
		if n > maxBinary {
			return fmt.Errorf("%q is larger than %d bytes", name, maxBinary)
		}
		return nil
	}
}

// selfTest runs the new binary's "version" command and requires it to report
// the release it was published as.
func selfTest(ctx context.Context, bin, tag string) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "version").Output()
	if err != nil {
		return fmt.Errorf("running it: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || fields[0] != "paymux" {
		return fmt.Errorf("unexpected output %q", strings.TrimSpace(string(out)))
	}
	if strings.TrimPrefix(fields[1], "v") != strings.TrimPrefix(tag, "v") {
		return fmt.Errorf("it reports version %s, but the release is %s", fields[1], tag)
	}
	return nil
}

// verifySignature checks SHA256SUMS.sig with cosign when it can. The
// signature protects the checksum file, which in turn covers the download.
// With cosign missing, or nothing to verify against, it says so and the
// checksum alone decides.
func (u *updater) verifySignature(sig releaseAsset, rel release, sums []byte, key, identity, issuer string) (string, error) {
	required := key != "" || identity != ""
	if identity != "" && issuer == "" {
		return "", errors.New("--cosign-identity needs --cosign-issuer; refusing to install")
	}
	cosign, err := exec.LookPath("cosign")
	if err != nil {
		if required {
			return "", errors.New("signature verification was requested but cosign is not installed; refusing to install")
		}
		return "the release is signed, but cosign is not installed: signature not verified (checksums only)", nil
	}
	if key == "" && identity == "" {
		return "the release is signed, but no --cosign-key or --cosign-identity was given: signature not verified (checksums only)", nil
	}
	sigBytes, err := u.download(sig.URL, maxSmallAsset)
	if err != nil {
		return "", fmt.Errorf("downloading the signature: %w", err)
	}
	dir, err := os.MkdirTemp("", "paymux-cosign-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	sumsFile, sigFile := filepath.Join(dir, "SHA256SUMS"), filepath.Join(dir, "SHA256SUMS.sig")
	if err := os.WriteFile(sumsFile, sums, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(sigFile, sigBytes, 0o600); err != nil {
		return "", err
	}
	args := []string{"verify-blob", "--signature", sigFile}
	if key != "" {
		args = append(args, "--key", key)
	} else {
		args = append(args, "--certificate-identity", identity, "--certificate-oidc-issuer", issuer)
		if cert, ok := rel.asset("SHA256SUMS.pem"); ok {
			if b, err := u.download(cert.URL, maxSmallAsset); err == nil {
				certFile := filepath.Join(dir, "SHA256SUMS.pem")
				if os.WriteFile(certFile, b, 0o600) == nil {
					args = append(args, "--certificate", certFile)
				}
			}
		}
	}
	args = append(args, sumsFile)
	cctx, cancel := context.WithTimeout(u.ctx, time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(cctx, cosign, args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("the SHA256SUMS signature did not verify, nothing was installed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return "signature verified with cosign", nil
}
