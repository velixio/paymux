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

// ---- supply chain rules (audit H-16) ----

func repoRoot() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(self))
}

func readRepoFile(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{repoRoot()}, rel...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func workflowFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot(), ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	return files
}

// stepBlock returns the lines of the step that starts at lines[i] (a "- uses:"
// line): up to the next line that is indented no deeper than the dash.
func stepBlock(lines []string, i int) []string {
	indent := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
	block := []string{lines[i]}
	for _, l := range lines[i+1:] {
		if strings.TrimSpace(l) != "" && len(l)-len(strings.TrimLeft(l, " ")) <= indent {
			break
		}
		block = append(block, l)
	}
	return block
}

// A checkout leaves the job's token in .git/config for every later step to
// read, and for any code those steps run. No job here pushes with it, so no
// checkout keeps it.
func TestCheckoutsDoNotPersistCredentials(t *testing.T) {
	checkouts := 0
	for _, f := range workflowFiles(t) {
		b, _ := os.ReadFile(f)
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if !strings.Contains(l, "uses: actions/checkout@") {
				continue
			}
			checkouts++
			if !strings.Contains(strings.Join(stepBlock(lines, i), "\n"), "persist-credentials: false") {
				t.Errorf("%s:%d: actions/checkout without persist-credentials: false", filepath.Base(f), i+1)
			}
		}
	}
	if checkouts < 8 {
		t.Errorf("only %d checkouts found: the scan is broken", checkouts)
	}
}

var digestRE = regexp.MustCompile(`@sha256:[0-9a-f]{64}\b`)

// A tag can be moved to other content; a digest cannot. Every base image of the
// Dockerfile and every service image of a workflow is pinned by digest.
func TestContainerImagesArePinnedByDigest(t *testing.T) {
	stages := map[string]bool{"scratch": true}
	images := 0
	for _, l := range strings.Split(readRepoFile(t, "deploy", "docker", "Dockerfile"), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || !strings.EqualFold(f[0], "FROM") {
			continue
		}
		image := f[1]
		if strings.HasPrefix(image, "--") { // FROM --platform=... image
			image = f[2]
		}
		if !stages[image] { // a stage of this file needs no digest
			images++
			if !digestRE.MatchString(image) {
				t.Errorf("Dockerfile: %s is not pinned by digest", image)
			}
		}
		if len(f) >= 4 && strings.EqualFold(f[len(f)-2], "AS") {
			stages[f[len(f)-1]] = true
		}
	}
	if images < 3 {
		t.Errorf("only %d Dockerfile images found", images)
	}
	imageLine := regexp.MustCompile(`^\s*image:\s*(\S+)`)
	for _, f := range workflowFiles(t) {
		b, _ := os.ReadFile(f)
		for i, l := range strings.Split(string(b), "\n") {
			if m := imageLine.FindStringSubmatch(l); m != nil && !digestRE.MatchString(m[1]) {
				t.Errorf("%s:%d: service image %s is not pinned by digest", filepath.Base(f), i+1, m[1])
			}
		}
	}
}

// Publishing and signing need a person's approval (the reviewers of the
// "release" environment, set in the repository's settings), and the archives
// carry a build provenance attestation.
func TestReleaseJobRunsInTheProtectedEnvironment(t *testing.T) {
	text := readRepoFile(t, ".github", "workflows", "release.yml")
	job := text[strings.Index(text, "\n  release:"):]
	if !regexp.MustCompile(`(?m)^    environment: release\s*$`).MatchString(job) {
		t.Error("the release job does not declare environment: release")
	}
	if !strings.Contains(job, "actions/attest-build-provenance@") || !strings.Contains(job, "attestations: write") {
		t.Error("the release job does not attest the build provenance of the archives")
	}
	if !strings.Contains(readRepoFile(t, "scripts", "release.sh"), "bun install --cwd frontend --frozen-lockfile --ignore-scripts") {
		t.Error("release.sh runs dependency install scripts")
	}
}

func TestNoStaleReviewMarkersInWorkflows(t *testing.T) {
	for _, f := range workflowFiles(t) {
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), "TODO(review)") {
			t.Errorf("%s still has a TODO(review) marker", filepath.Base(f))
		}
	}
}

// The nightly fuzz job never runs for a pull request or a push.
func TestFuzzWorkflowIsScheduleOnly(t *testing.T) {
	text := readRepoFile(t, ".github", "workflows", "fuzz.yml")
	on := text[strings.Index(text, "\non:"):strings.Index(text, "\npermissions:")]
	if !strings.Contains(on, "schedule:") || !strings.Contains(on, "cron:") {
		t.Error("fuzz.yml has no schedule")
	}
	for _, trigger := range []string{"pull_request", "push:", "workflow_call"} {
		if strings.Contains(on, trigger) {
			t.Errorf("fuzz.yml is triggered by %s", trigger)
		}
	}
	if !strings.Contains(text, "-fuzztime 10m") {
		t.Error("fuzz.yml does not give each target 10 minutes")
	}
}

// Dependabot covers each ecosystem, weekly, with one grouped pull request per
// ecosystem, so updates cannot flood the review queue.
func TestDependabotIsWeeklyAndGrouped(t *testing.T) {
	text := readRepoFile(t, ".github", "dependabot.yml")
	blocks := strings.Split(text, "  - package-ecosystem: ")[1:]
	got := map[string]bool{}
	for _, b := range blocks {
		name := strings.Fields(b)[0]
		got[name] = true
		if !strings.Contains(b, "interval: weekly") || !strings.Contains(b, "groups:") || !strings.Contains(b, `patterns: ["*"]`) {
			t.Errorf("dependabot %s: not weekly and grouped", name)
		}
	}
	for _, want := range []string{"gomod", "npm", "github-actions", "docker"} {
		if !got[want] {
			t.Errorf("dependabot does not cover %s", want)
		}
	}
}

// gitleaks scans the history in the CI gate, and every reviewed false positive
// is listed with the reason it is fake.
func TestGitleaksIsInTheGateWithReasonedAllowlist(t *testing.T) {
	ci := readRepoFile(t, ".github", "workflows", "ci.yml")
	if !strings.Contains(ci, "uses: gitleaks/gitleaks-action@") || !strings.Contains(ci, "fetch-depth: 0") {
		t.Error("ci.yml does not run gitleaks over the full history")
	}
	if !regexp.MustCompile(`needs: \[[^\]]*\bsecrets\b[^\]]*\]`).MatchString(ci) {
		t.Error("the ci aggregator does not wait for the secrets job")
	}
	prev := ""
	entries := 0
	for _, l := range strings.Split(readRepoFile(t, ".gitleaksignore"), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			prev = l
			continue
		}
		entries++
		if !strings.HasPrefix(prev, "# ") || len(prev) < 12 {
			t.Errorf(".gitleaksignore: %s has no reason comment above it", l)
		}
	}
	if entries == 0 {
		t.Error(".gitleaksignore has no entries: the file is not being read")
	}
}

// A reusable workflow's jobs cannot have more permissions than the job that
// calls it: GitHub refuses to start the run (startup_failure). release.yml
// calls ci.yml, so its "ci" job grants every permission a ci.yml job asks for.
// The first v0.1.0 tag failed this way when the gitleaks job asked for
// pull-requests: read.
func TestReleaseGrantsWhatTheCIGateAsksFor(t *testing.T) {
	perm := regexp.MustCompile(`^\s+([a-z-]+): (read|write)\s*(#.*)?$`)
	collect := func(lines []string) map[string]string {
		out := map[string]string{}
		in, indent := false, 0
		for _, l := range lines {
			trimmed := strings.TrimLeft(l, " ")
			depth := len(l) - len(trimmed)
			switch {
			case strings.HasPrefix(trimmed, "permissions:"):
				in, indent = true, depth
				continue
			case in && trimmed != "" && depth <= indent:
				in = false
			}
			if m := perm.FindStringSubmatch(l); in && m != nil && out[m[1]] != "write" {
				out[m[1]] = m[2]
			}
		}
		return out
	}
	asked := collect(strings.Split(readRepoFile(t, ".github", "workflows", "ci.yml"), "\n"))
	release := strings.Split(readRepoFile(t, ".github", "workflows", "release.yml"), "\n")
	var job []string
	for i, l := range release {
		if l == "  ci:" {
			for _, m := range release[i+1:] {
				if strings.HasPrefix(m, "  ") && !strings.HasPrefix(m, "   ") && strings.TrimSpace(m) != "" {
					break
				}
				job = append(job, m)
			}
		}
	}
	if job == nil {
		t.Fatal(`release.yml has no "ci" job`)
	}
	granted := collect(job)
	for scope, level := range asked {
		if g := granted[scope]; g == "" || (level == "write" && g != "write") {
			t.Errorf("ci.yml asks for %s: %s, which release.yml's ci job does not grant (granted %q)", scope, level, g)
		}
	}
}
