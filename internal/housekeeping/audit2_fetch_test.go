package housekeeping

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAudit2DefaultFetchRefusesPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("tok")) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	f := DefaultFetch(false)
	for _, h := range []string{"127.0.0.1", "2130706433", "0x7f000001", "[::ffff:127.0.0.1]", "localhost", "[::1]", "0"} {
		if code, body, err := f(context.Background(), "http://"+h+":"+port+"/x"); err == nil {
			t.Errorf("%s: fetched (%d %q)", h, code, body)
		}
	}
}

func TestAudit2DefaultFetchNoProxyNoRedirect(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer target.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer a.Close()
	code, _, err := DefaultFetch(true)(context.Background(), a.URL)
	if err != nil || code != 302 {
		t.Fatalf("code=%d err=%v (proxy used or redirect followed?)", code, err)
	}
}
