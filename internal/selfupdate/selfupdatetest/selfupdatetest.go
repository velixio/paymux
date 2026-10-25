// Package selfupdatetest has the fakes the update tests share: a GitHub-like
// releases server with real archives, stand-in binaries that answer "version",
// and a fake cosign on the PATH.
package selfupdatetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

// Script is a stand-in binary: a shell script that answers "version".
func Script(version string) []byte {
	return []byte("#!/bin/sh\necho \"paymux " + version + "\"\n")
}

// WriteBinary writes the stand-in running binary of the given version.
func WriteBinary(t *testing.T, version string) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "paymux")
	if err := os.WriteFile(dest, Script(version), 0o755); err != nil {
		t.Fatal(err)
	}
	return dest
}

// TarGz makes a .tar.gz with a README and a file called name.
func TarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
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

// Releases is a GitHub-like releases API with assets, for velixio/paymux.
type Releases struct {
	t      *testing.T
	Srv    *httptest.Server
	Tag    string
	Assets map[string][]byte // name -> content
	mu     sync.Mutex
	hits   []string
}

// NewReleases serves one release, tag, whose build answers "version" with
// binVersion. With signed it carries a SHA256SUMS.sig and certificate.
func NewReleases(t *testing.T, tag, binVersion string, signed bool) *Releases {
	t.Helper()
	f := &Releases{t: t, Tag: tag, Assets: map[string][]byte{}}
	archive := TarGz(t, "paymux", Script(binVersion))
	f.Assets[f.AssetName()] = archive
	f.Assets["SHA256SUMS"] = []byte(sum(archive) + "  " + f.AssetName() + "\n")
	if signed {
		f.Assets["SHA256SUMS.sig"] = []byte("c2lnbmF0dXJl")
		f.Assets["SHA256SUMS.pem"] = []byte("-----BEGIN CERTIFICATE-----\n")
	}
	f.Srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Srv.Close)
	return f
}

// AssetName is the archive of this platform.
func (f *Releases) AssetName() string {
	return fmt.Sprintf("paymux_%s_%s_%s.tar.gz", strings.TrimPrefix(f.Tag, "v"), runtime.GOOS, runtime.GOARCH)
}

// NotesURL is the release page the fake reports.
func (f *Releases) NotesURL() string {
	return "https://github.com/velixio/paymux/releases/tag/" + f.Tag
}

func (f *Releases) release() map[string]any {
	var assets []map[string]any
	for name, body := range f.Assets {
		assets = append(assets, map[string]any{"name": name, "size": len(body), "browser_download_url": f.Srv.URL + "/dl/" + name})
	}
	return map[string]any{"tag_name": f.Tag, "draft": false, "prerelease": false, "html_url": f.NotesURL(), "assets": assets}
}

func (f *Releases) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits = append(f.hits, r.URL.Path)
	f.mu.Unlock()
	switch {
	case r.URL.Path == "/repos/velixio/paymux/releases/latest":
		_ = json.NewEncoder(w).Encode(f.release())
	case strings.HasPrefix(r.URL.Path, "/repos/velixio/paymux/releases/tags/"):
		if strings.TrimPrefix(r.URL.Path, "/repos/velixio/paymux/releases/tags/") != f.Tag {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(f.release())
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		body, ok := f.Assets[strings.TrimPrefix(r.URL.Path, "/dl/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	default:
		http.NotFound(w, r)
	}
}

// Hits is how many requests the server got under prefix.
func (f *Releases) Hits(prefix string) (n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.hits {
		if strings.HasPrefix(h, prefix) {
			n++
		}
	}
	return n
}

// FakeCosign puts a cosign that exits with code on the PATH and returns the
// file it writes its arguments to (absent if it was never run).
func FakeCosign(t *testing.T, exit int) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" > %q\nexit %d\n", argsFile, exit)
	if err := os.WriteFile(filepath.Join(dir, "cosign"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

// NoCosign makes cosign unavailable.
func NoCosign(t *testing.T) { t.Setenv("PATH", t.TempDir()) }
