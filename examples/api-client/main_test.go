package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateUsesTheApplicationContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/payments" || r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Idempotency-Key") != "order-1" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request: %s %s %v", r.Method, r.URL.Path, r.Header)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"pay_example","checkout_url":"https://pay.example.com/pay/example?t=secret"}`)
	}))
	defer server.Close()
	id, err := create(context.Background(), server.Client(), server.URL+"/", "test-key", "order-1")
	if err != nil || id != "pay_example" {
		t.Fatalf("%q %v", id, err)
	}
}
func TestCreateRejectsUncertainOrInvalidResults(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{401, `{}`}, {201, `bad json`}, {201, `{"id":"pay_example"}`}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
		if _, err := create(context.Background(), server.Client(), server.URL, "key", "order-1"); err == nil {
			t.Errorf("accepted %d %s", tc.status, tc.body)
		}
		server.Close()
	}
	for _, args := range [][3]string{{"", "key", "id"}, {"http://localhost", "", "id"}, {"http://localhost", "key", ""}, {"://bad", "key", "id"}} {
		if _, err := create(context.Background(), http.DefaultClient, args[0], args[1], args[2]); err == nil {
			t.Errorf("accepted invalid configuration")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := create(ctx, http.DefaultClient, "http://localhost", "key", "id"); err == nil {
		t.Fatal("ignored canceled request")
	}
}
