package connector

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestAttemptHookRunsForEveryAttemptOnItsOwnCopy(t *testing.T) {
	var n atomic.Int32
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("X-Attempt"))
		if n.Add(1) < 3 {
			w.WriteHeader(502)
		}
	}))
	defer srv.Close()
	c := NewHTTPClient(testOpts(&bytes.Buffer{}))
	var calls int
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req = req.WithContext(WithAttemptHook(req.Context(), func(r *http.Request) {
		calls++
		r.Header.Set("X-Attempt", string(rune('a'+calls-1)))
	}))
	resp, err := c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	if calls != 3 || len(seen) != 3 || seen[0] != "a" || seen[1] != "b" || seen[2] != "c" {
		t.Errorf("hook calls=%d seen=%v", calls, seen)
	}
	if req.Header.Get("X-Attempt") != "" {
		t.Error("the hook changed the caller's request")
	}
}
