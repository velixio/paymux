package connector

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestHTTPClientObservesEveryAttempt(t *testing.T) {
	codes := []int{502, 200, 429, 404, 201}
	var i int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		c := codes[i]
		i++
		mu.Unlock()
		w.WriteHeader(c)
	}))
	defer srv.Close()

	var got []RequestObservation
	o := testOpts(&bytes.Buffer{})
	o.Observer = func(ob RequestObservation) { got = append(got, ob) }
	c := NewHTTPClient(o)
	for _, m := range []string{"GET", "GET", "GET", "POST"} {
		req, _ := http.NewRequest(m, srv.URL+"/secret/path?key=abc", nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	// The first GET is retried after the 502 (two attempts); a 429 and a 404 are not retried.
	want := []struct {
		method, outcome string
		status          int
	}{{"GET", OutcomeServerError, 502}, {"GET", OutcomeOK, 200}, {"GET", OutcomeRateLimited, 429}, {"GET", OutcomeClientError, 404}, {"POST", OutcomeOK, 201}}
	if len(got) != len(want) {
		t.Fatalf("observations = %+v", got)
	}
	for n, w := range want {
		if got[n].Method != w.method || got[n].Outcome != w.outcome || got[n].Status != w.status || got[n].Duration < 0 {
			t.Errorf("%d: %+v want %+v", n, got[n], w)
		}
	}
}

func TestObserveOutcomes(t *testing.T) {
	if o := observe("GET", nil, errTimeout{}, time.Second); o.Outcome != OutcomeTimeout || o.Status != 0 {
		t.Errorf("timeout: %+v", o)
	}
	if o := observe("GET", nil, http.ErrHandlerTimeout, 0); o.Outcome != OutcomeNetworkError {
		t.Errorf("network: %+v", o)
	}
	if o := observe("GET", &http.Response{StatusCode: 301}, nil, 0); o.Outcome != OutcomeOK {
		t.Errorf("redirect: %+v", o)
	}
}

type errTimeout struct{}

func (errTimeout) Error() string { return "deadline" }
func (errTimeout) Is(target error) bool {
	return target.Error() == "context deadline exceeded"
}
