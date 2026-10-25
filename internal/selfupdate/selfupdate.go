// Package selfupdate installs a newer release of paymux from GitHub: it finds
// the release, downloads the build for this platform, checks it against the
// release's SHA256SUMS and the cosign signature of that file, tries the new
// binary and replaces the running one atomically. The CLI ("paymux update") and
// the dashboard share it.
package selfupdate

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Defaults of the release source.
const (
	DefaultRepo = "velixio/paymux"
	DefaultAPI  = "https://api.github.com"
)

const (
	maxArchive    = 300 << 20
	maxBinary     = 500 << 20
	maxSmallAsset = 1 << 20
)

// ReleaseOIDCIssuer is the issuer of the certificates GitHub Actions workflows
// get from Sigstore for keyless signing.
const ReleaseOIDCIssuer = "https://token.actions.githubusercontent.com"

// ReleaseIdentityRegexp matches the signer identity of the release workflow of
// repo (owner/name) on a version tag: the identity the keyless signature of
// SHA256SUMS carries. It is anchored at both ends, so another repository, another
// workflow or a branch does not match.
func ReleaseIdentityRegexp(repo string) string {
	return "^" + regexp.QuoteMeta("https://github.com/"+repo+"/.github/workflows/release.yml@refs/tags/") + `v[^/]+$`
}

// CompareVersions orders two release tags ("v1.2.3", "1.2.3-rc.1"): -1, 0 or
// 1. A pre-release sorts before its release.
func CompareVersions(a, b string) int {
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

func Parsable(v string) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	return v != "" && v[0] >= '0' && v[0] <= '9'
}

// Asset is a file of a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release is a GitHub release.
type Release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	// HTMLURL is the release page, which carries the release notes.
	HTMLURL string  `json:"html_url"`
	Assets  []Asset `json:"assets"`
}

func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Client talks to a GitHub-like releases API.
type Client struct {
	API   string // base URL, e.g. https://api.github.com
	Token string // sent to the API's host only
	HTTP  *http.Client
}

func (c *Client) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "paymux-update")
	req.Header.Set("Accept", "application/vnd.github+json, application/octet-stream")
	if pu, err := url.Parse(rawURL); err == nil {
		if au, err := url.Parse(c.API); err == nil && pu.Host == au.Host && c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token) // never to other hosts
		}
	}
	return c.HTTP.Do(req)
}

// FetchRelease returns the latest release of repo, or the one tagged tag.
func (c *Client) FetchRelease(ctx context.Context, repo, tag string) (Release, error) {
	endpoint := c.API + "/repos/" + repo + "/releases/latest"
	if tag != "" {
		endpoint = c.API + "/repos/" + repo + "/releases/tags/" + url.PathEscape(tag)
	}
	res, err := c.get(ctx, endpoint)
	if err != nil {
		return Release{}, fmt.Errorf("asking %s for the release: %w", c.API, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		if tag != "" {
			return Release{}, fmt.Errorf("release %s not found in %s", tag, repo)
		}
		return Release{}, fmt.Errorf("no release found in %s", repo)
	}
	if res.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("the releases API answered %s", res.Status)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("reading the release: %w", err)
	}
	if rel.Tag == "" || rel.Draft {
		return Release{}, errors.New("the release has no usable tag")
	}
	return rel, nil
}

func (c *Client) checkURL(raw string) error {
	pu, err := url.Parse(raw)
	if err != nil {
		return err
	}
	au, _ := url.Parse(c.API)
	if pu.Scheme != "https" && !(au != nil && au.Scheme == "http" && pu.Scheme == "http") {
		return fmt.Errorf("refusing to download from %q: not https", raw)
	}
	return nil
}

func (c *Client) download(ctx context.Context, raw string, limit int64) ([]byte, error) {
	if err := c.checkURL(raw); err != nil {
		return nil, err
	}
	res, err := c.get(ctx, raw)
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
func (c *Client) downloadTo(ctx context.Context, raw string, f *os.File, limit int64) (string, error) {
	if err := c.checkURL(raw); err != nil {
		return "", err
	}
	res, err := c.get(ctx, raw)
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

// ParseSums reads sha256sum output: "<hex>  <name>" (or "<hex> *<name>").
func ParseSums(s string) map[string]string {
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

// ExtractBinary copies the regular file called name out of a .tar.gz.
func ExtractBinary(archive, name string, dst io.Writer) error {
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

// SelfTest runs the new binary's "version" command and requires it to report
// the release it was published as.
func SelfTest(ctx context.Context, bin, tag string) error {
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

// verifySignature checks SHA256SUMS.sig with cosign. The signature protects the
// checksum file, which in turn covers the download. It verifies against key, or
// against the keyless identity and issuer given, or by default against the
// release workflow of repo. Anything that stops it from verifying is an error.
func (c *Client) verifySignature(ctx context.Context, sig Asset, rel Release, sums []byte, repo, key, identity, issuer string) (string, error) {
	const skipHint = "; use --insecure-skip-signature to install on the checksum alone"
	if identity != "" && issuer == "" {
		return "", errors.New("--cosign-identity needs --cosign-issuer; refusing to install")
	}
	cosign, err := exec.LookPath("cosign")
	if err != nil {
		return "", errors.New("the signature of the release cannot be verified because cosign is not installed (https://docs.sigstore.dev/cosign/); refusing to install" + skipHint)
	}
	sigBytes, err := c.download(ctx, sig.URL, maxSmallAsset)
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
	switch {
	case key != "":
		args = append(args, "--key", key)
	case identity != "":
		args = append(args, "--certificate-identity", identity, "--certificate-oidc-issuer", issuer)
		if cert, ok := rel.Asset("SHA256SUMS.pem"); ok {
			if b, err := c.download(ctx, cert.URL, maxSmallAsset); err == nil {
				certFile := filepath.Join(dir, "SHA256SUMS.pem")
				if os.WriteFile(certFile, b, 0o600) == nil {
					args = append(args, "--certificate", certFile)
				}
			}
		}
	default:
		// The built-in identity: the release workflow of this repository, on a tag.
		cert, ok := rel.Asset("SHA256SUMS.pem")
		if !ok {
			return "", errors.New("the release has a signature but no SHA256SUMS.pem certificate to check it against; refusing to install" + skipHint)
		}
		b, err := c.download(ctx, cert.URL, maxSmallAsset)
		if err != nil {
			return "", fmt.Errorf("downloading the signing certificate: %w", err)
		}
		certFile := filepath.Join(dir, "SHA256SUMS.pem")
		if err := os.WriteFile(certFile, b, 0o600); err != nil {
			return "", err
		}
		args = append(args, "--certificate", certFile, "--certificate-identity-regexp", ReleaseIdentityRegexp(repo), "--certificate-oidc-issuer", ReleaseOIDCIssuer)
	}
	args = append(args, sumsFile)
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(cctx, cosign, args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("the SHA256SUMS signature did not verify, nothing was installed: %v: %s (use --insecure-skip-signature only if you accept an unverified release)", err, strings.TrimSpace(string(out)))
	}
	return "signature verified with cosign", nil
}

// InstallOptions are what Install needs besides the release.
type InstallOptions struct {
	Repo    string // owner/name, for the signer identity
	Dest    string // the binary to replace
	Current string // the running version, for the progress lines
	// Signature: by default SHA256SUMS.sig is verified against the release
	// workflow of Repo. CosignKey, or CosignIdentity with CosignIssuer, verify
	// against something else; SkipSignature installs on the checksum alone.
	CosignKey, CosignIdentity, CosignIssuer string
	SkipSignature                           bool
	// Logf receives progress lines (nil: none).
	Logf func(format string, args ...any)
}

// Installed describes a successful Install.
type Installed struct {
	Exe  string // the binary that was replaced
	Prev string // where the old one was kept ("" if it could not be)
}

// Install downloads rel's build for this platform, verifies it and replaces
// o.Dest with it. Nothing is replaced unless every check passed.
func (c *Client) Install(ctx context.Context, rel Release, o InstallOptions) (Installed, error) {
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	exe, err := filepath.Abs(o.Dest)
	if err != nil {
		return Installed{}, err
	}
	dir := filepath.Dir(exe)

	ver := strings.TrimPrefix(rel.Tag, "v")
	archiveName := fmt.Sprintf("paymux_%s_%s_%s.tar.gz", ver, runtime.GOOS, runtime.GOARCH)
	archive, ok := rel.Asset(archiveName)
	if !ok {
		var names []string
		for _, a := range rel.Assets {
			names = append(names, a.Name)
		}
		return Installed{}, fmt.Errorf("release %s has no build for %s/%s (looked for %s; it has %s)", rel.Tag, runtime.GOOS, runtime.GOARCH, archiveName, strings.Join(names, ", "))
	}
	sumsAsset, ok := rel.Asset("SHA256SUMS")
	if !ok {
		return Installed{}, fmt.Errorf("release %s has no SHA256SUMS, so the download cannot be verified; refusing to install it", rel.Tag)
	}

	logf("%s -> %s (%s)", o.Current, rel.Tag, archiveName)
	sums, err := c.download(ctx, sumsAsset.URL, maxSmallAsset)
	if err != nil {
		return Installed{}, fmt.Errorf("downloading SHA256SUMS: %v", err)
	}
	want, ok := ParseSums(string(sums))[archiveName]
	if !ok {
		return Installed{}, fmt.Errorf("SHA256SUMS has no entry for %s; refusing to install it", archiveName)
	}

	tmpArchive, err := os.CreateTemp(dir, ".paymux-update-*")
	if err != nil {
		return Installed{}, fmt.Errorf("cannot write to %s: %v (run as a user who can, or install elsewhere with --dest)", dir, err)
	}
	defer os.Remove(tmpArchive.Name())
	got, err := c.downloadTo(ctx, archive.URL, tmpArchive, maxArchive)
	tmpArchive.Close()
	if err != nil {
		return Installed{}, fmt.Errorf("downloading %s: %v", archiveName, err)
	}
	if got != want {
		return Installed{}, fmt.Errorf("checksum mismatch for %s: SHA256SUMS says %s, the download is %s; nothing was installed", archiveName, want, got)
	}
	logf("checksum verified (%s)", got[:12])

	// The signature over SHA256SUMS is checked unless the caller said not to:
	// against the key or identity given, otherwise against the release workflow
	// of Repo. There is no fallback to the checksum alone.
	sig, signed := rel.Asset("SHA256SUMS.sig")
	switch {
	case o.SkipSignature:
		logf("WARNING: the signature of this release was NOT verified (--insecure-skip-signature); only the checksum protects this download")
	case !signed:
		return Installed{}, fmt.Errorf("release %s is not signed (it has no SHA256SUMS.sig); refusing to install. Use --insecure-skip-signature to install it on the checksum alone", rel.Tag)
	default:
		msg, err := c.verifySignature(ctx, sig, rel, sums, o.Repo, o.CosignKey, o.CosignIdentity, o.CosignIssuer)
		if err != nil {
			return Installed{}, err
		}
		logf("%s", msg)
	}

	tmpBin, err := os.CreateTemp(dir, ".paymux-update-*")
	if err != nil {
		return Installed{}, fmt.Errorf("cannot write to %s: %v", dir, err)
	}
	defer os.Remove(tmpBin.Name())
	if err := ExtractBinary(tmpArchive.Name(), "paymux", tmpBin); err != nil {
		tmpBin.Close()
		return Installed{}, fmt.Errorf("%s: %v", archiveName, err)
	}
	if err := tmpBin.Close(); err != nil {
		return Installed{}, err
	}
	if err := os.Chmod(tmpBin.Name(), 0o755); err != nil {
		return Installed{}, err
	}
	if err := SelfTest(ctx, tmpBin.Name(), rel.Tag); err != nil {
		return Installed{}, fmt.Errorf("the downloaded binary failed its check, nothing was installed: %v", err)
	}

	prev := exe + ".prev"
	_ = os.Remove(prev)
	if err := os.Link(exe, prev); err != nil {
		prev = "" // not fatal: the old binary is just not kept
	}
	if err := os.Rename(tmpBin.Name(), exe); err != nil {
		return Installed{}, fmt.Errorf("replacing %s: %v", exe, err)
	}
	return Installed{Exe: exe, Prev: prev}, nil
}

// ExecutablePath is the path of the running binary, symlinks resolved.
func ExecutablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}

// CheckWritable reports why the binary at exe cannot be replaced: Install
// writes next to it and renames over it, so its directory must take a new file.
func CheckWritable(exe string) error {
	dir := filepath.Dir(exe)
	f, err := os.CreateTemp(dir, ".paymux-writable-*")
	if err != nil {
		return fmt.Errorf("the directory of the binary (%s) is not writable: %v", dir, err)
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// CosignInstalled reports whether the cosign command is on the PATH.
func CosignInstalled() bool {
	_, err := exec.LookPath("cosign")
	return err == nil
}
