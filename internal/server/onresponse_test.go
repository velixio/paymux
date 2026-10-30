package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/logctx"
)

type seen struct {
	ctx           context.Context
	method, route string
	code          int
	d             time.Duration
}

func post(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func hookServer(t *testing.T) (*Server, *[]seen) {
	t.Helper()
	var got []seen
	s := New(Options{OnResponse: func(ctx context.Context, method, route string, status int, d time.Duration) {
		got = append(got, seen{ctx: ctx, method: method, route: route, code: status, d: d})
	}})
	return s, &got
}

func TestOnResponseGetsRouteStatusAndRequestScope(t *testing.T) {
	s, got := hookServer(t)
	s.HandleFunc("POST /v1/things", func(w http.ResponseWriter, r *http.Request) {
		logctx.Note(r.Context(), "org_id", "org_1", "app_id", "app_1") // learnt by the handler
		w.WriteHeader(http.StatusUnauthorized)
	})
	get(s.Handler(), "/nothing-here", "X-Request-Id", "req-abc")
	rec := post(s.Handler(), "/v1/things", "X-Request-Id", "req-def")
	if rec.Code != 401 || len(*got) != 2 {
		t.Fatalf("%d %+v", rec.Code, *got)
	}
	g := (*got)[1]
	ids := logctx.IDs(g.ctx)
	if g.method != "POST" || g.route != "POST /v1/things" || g.code != 401 || g.d < 0 || ids["request_id"] != "req-def" || ids["org_id"] != "org_1" || ids["app_id"] != "app_1" {
		t.Errorf("%+v %v", g, ids)
	}
	if u := (*got)[0]; u.route != "unmatched" || u.code != 404 || logctx.IDs(u.ctx)["request_id"] != "req-abc" {
		t.Errorf("unmatched: %+v", u)
	}
}

func TestOnResponseSeesAPanicAsA500(t *testing.T) {
	s, got := hookServer(t)
	s.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("x") })
	rec := get(s.Handler(), "/boom")
	if rec.Code != 500 || len(*got) != 1 || (*got)[0].code != 500 || (*got)[0].route != "GET /boom" {
		t.Errorf("%d %+v", rec.Code, *got)
	}
}

func TestNoHookIsFine(t *testing.T) {
	s := New(Options{})
	if get(s.Handler(), "/healthz").Code != 200 {
		t.Fatal("healthz")
	}
}
