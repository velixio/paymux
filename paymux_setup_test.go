package paymux

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var setupTokenRE = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{8}-[0-9a-f]{8}-[0-9a-f]{8}`)

func TestFirstRunSetupThroughTheRunningHub(t *testing.T) {
	dir := t.TempDir()
	r := startApp(t, testConfig(t, "sqlite://"+dir+"/paymux.db", dir))
	waitReady(t, r)
	addr := r.app.Addr()

	// The start-up log carries the one-time token and where to use it.
	logs := r.logs.String()
	token := setupTokenRE.FindString(logs)
	if token == "" || !strings.Contains(logs, "first-run setup") || !strings.Contains(logs, "/setup") {
		t.Fatalf("the log should announce the setup token:\n%s", logs)
	}

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (int, string) {
		res, err := c.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	code, page := get("/setup")
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page)
	if code != 200 || csrf == nil {
		t.Fatalf("/setup: %d %s", code, page)
	}
	if c2, _ := get("/dashboard/login"); c2 != 200 {
		t.Fatalf("login page: %d", c2)
	}

	form := url.Values{"token": {token}, "email": {"owner@example.com"}, "password": {"correct horse battery staple"},
		"password2": {"correct horse battery staple"}, "org_name": {"Acme"}, "app_name": {"Shop"}, "csrf": {csrf[1]}}
	res, err := c.PostForm("http://"+addr+"/setup", form)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	keys := regexp.MustCompile(`pmx_(test|live)_sk_[A-Za-z0-9]+`).FindAllString(string(b), -1)
	if res.StatusCode != 200 || len(keys) != 2 {
		t.Fatalf("setup: %d keys=%v\n%s", res.StatusCode, keys, b)
	}

	// One-time: closed now, and nothing secret went to the log.
	if code, _ := get("/setup"); code != 404 {
		t.Errorf("/setup after setup: %d, want 404", code)
	}
	logs = r.logs.String()
	for _, secret := range append(keys, "correct horse battery staple") {
		if strings.Contains(logs, secret) {
			t.Errorf("the log contains a secret: %s", secret)
		}
	}
	shutdown(t, r)
}
