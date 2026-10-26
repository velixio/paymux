package outbound

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAudit2OutboundRefusesLoopbackForms(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.WriteHeader(204) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	c := newHTTPClient(false, nil)
	for _, h := range []string{"127.0.0.1", "2130706433", "0x7f000001", "017700000001", "127.1", "0", "[::ffff:127.0.0.1]", "[::1]", "localhost", "0177.0.0.1"} {
		resp, err := c.Post("http://"+h+":"+port+"/", "application/json", nil)
		if err == nil {
			resp.Body.Close()
			t.Errorf("%s: delivery went through", h)
		}
	}
	if hits != 0 {
		t.Fatalf("server was hit %d times", hits)
	}
}

func TestAudit2OutboundNoRedirect(t *testing.T) {
	c := newHTTPClient(true, nil)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer target.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer a.Close()
	resp, err := c.Post(a.URL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
