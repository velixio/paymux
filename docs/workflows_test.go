package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// A tag such as actions/checkout@v4 can be moved by whoever controls the
// action's repository, and then runs in our CI with its secrets. Every action a
// workflow uses from outside this repository is pinned to a full commit SHA,
// with the tag it stands for in a comment.
func TestWorkflowActionsArePinnedToCommits(t *testing.T) {
	// Another test in this package changes directory, so find the repository from this file.
	_, self, _, _ := runtime.Caller(0)
	files, err := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(self)), ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	use := regexp.MustCompile(`^\s*-?\s*uses:\s*(\S+)(.*)$`)
	pinned := regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}$`)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(b), "\n") {
			m := use.FindStringSubmatch(line)
			if m == nil || strings.HasPrefix(m[1], "./") {
				continue // a workflow of this repository
			}
			if !pinned.MatchString(m[1]) {
				t.Errorf("%s:%d: %s is not pinned to a commit SHA", filepath.Base(f), i+1, m[1])
			}
			if !strings.Contains(m[2], "# v") {
				t.Errorf("%s:%d: %s has no comment naming the tag it stands for", filepath.Base(f), i+1, m[1])
			}
		}
	}
}
