package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The image's /data holds the databases (sealed secrets, customer data): it is
// created 0700 and owned by the non-root user, so no other user of a shared
// host or volume can read it (audit R-A17).
func TestDockerfileCreatesDataDirPrivately(t *testing.T) {
	_, self, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(self)), "deploy", "docker", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !regexp.MustCompile(`chmod\s+0?700\s+/out/data`).MatchString(text) {
		t.Error("the Dockerfile does not chmod 0700 the /data directory it creates")
	}
	if !strings.Contains(text, "--chown=65532:65532 /out/data /data") {
		t.Error("/data is not owned by 65532")
	}
}
