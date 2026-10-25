package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

var (
	key    = []byte("test-key-for-example-only")
	secret = "whsec_" + base64.StdEncoding.EncodeToString(key)
	body   = []byte(`{"type":"payment.succeeded","data":{"object":{"id":"pay_example"}}}`)
	t0     = time.Unix(1000, 0)
)

func sign(id string, ts int64, b []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + strconv.FormatInt(ts, 10) + "."))
	m.Write(b)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func headers() http.Header {
	h := http.Header{}
	h.Set("webhook-id", "evt_example")
	h.Set("webhook-timestamp", "1000")
	h.Set("webhook-signature", "v2,ignored v1,bad! v1,"+sign("evt_example", 1000, body))
	return h
}

func TestVerify(t *testing.T) {
	if id, err := verify(secret, headers(), body, t0); err != nil || id != "evt_example" {
		t.Fatalf("a signed delivery: %q %v", id, err)
	}
	bad := http.Header{}
	bad.Set("webhook-id", "evt_example")
	bad.Set("webhook-timestamp", "bad")
	bad.Set("webhook-signature", "v1,x")
	for name, c := range map[string]struct {
		secret string
		h      http.Header
		body   []byte
		now    time.Time
	}{
		"no headers":      {secret, http.Header{}, body, t0},
		"tampered body":   {secret, headers(), []byte("tampered"), t0},
		"too old":         {secret, headers(), body, t0.Add(301 * time.Second)},
		"from the future": {secret, headers(), body, t0.Add(-301 * time.Second)},
		"bad secret":      {"whsec_!", headers(), body, t0},
		"empty secret":    {"whsec_", headers(), body, t0},
		"bad timestamp":   {secret, bad, body, t0},
		"other key":       {"whsec_" + base64.StdEncoding.EncodeToString([]byte("another-key")), headers(), body, t0},
	} {
		if _, err := verify(c.secret, c.h, c.body, c.now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestInboxStoresAnEventOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inbox")
	for i, want := range []bool{true, false} {
		if got, err := store(dir, "evt_example", body); err != nil || got != want {
			t.Fatalf("store %d: %v %v, want %v", i, got, err, want)
		}
	}
	if saved, _ := os.ReadFile(filepath.Join(dir, "evt_example.json")); !bytes.Equal(saved, body) {
		t.Errorf("saved %q", saved)
	}
	for _, b := range []string{"not json", "[]", `{"type":1,"data":{}}`, `{"type":"payment.succeeded"}`} {
		if _, err := store(dir, "bad", []byte(b)); err == nil {
			t.Errorf("accepted %q", b)
		}
	}
}

func TestHandlerOverHTTP(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Unix()
	h := handler(secret, dir)
	post := func(id string, ts int64, b []byte, sig string) int {
		req := httptest.NewRequest("POST", "/", bytes.NewReader(b))
		req.Header.Set("webhook-id", id)
		req.Header.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
		req.Header.Set("webhook-signature", "v1,"+sig)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post("evt_1", now, body, sign("evt_1", now, body)); code != 204 {
		t.Errorf("signed delivery: %d", code)
	}
	if code := post("evt_1", now, body, sign("evt_1", now, body)); code != 204 { // a redelivery is fine and stored once
		t.Errorf("redelivery: %d", code)
	}
	if code := post("evt_2", now, body, "AAAA"); code != 400 {
		t.Errorf("bad signature: %d", code)
	}
	if code := post("evt_3", now-3600, body, sign("evt_3", now-3600, body)); code != 400 {
		t.Errorf("stale delivery: %d", code)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(files) != 1 {
		t.Errorf("inbox holds %d events, want 1", len(files))
	}
}
