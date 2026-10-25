// Command webhook-receiver-go is a local signed Paymux webhook receiver with a
// durable inbox, in Go with only the standard library. It is the Go version of
// examples/webhook-receiver (Python) and examples/webhook-receiver-js.
//
//	PAYMUX_WEBHOOK_SECRET=whsec_... go run ./examples/webhook-receiver-go
//
// It binds to loopback, stores verified events (one file per event id, so a
// redelivery is stored once) and does not fulfil orders. Verify the signature over
// the raw request bytes, as here, before you parse or trust anything in the body.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// tolerance is how far a delivery's timestamp may be from now.
const tolerance = 5 * time.Minute

// verify checks the Standard Webhooks signature and age of a delivery and returns
// its stable event id. The signed string is "id.timestamp.body" (raw bytes), the
// key is the base64 inside the whsec_ secret, and the signature header holds one or
// more "v1,<base64>" candidates.
func verify(secret string, h http.Header, body []byte, now time.Time) (string, error) {
	id, ts, sigs := h.Get("webhook-id"), h.Get("webhook-timestamp"), h.Get("webhook-signature")
	if id == "" || ts == "" || sigs == "" {
		return "", errors.New("missing signed headers")
	}
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", errors.New("invalid timestamp")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) == 0 {
		return "", errors.New("invalid secret")
	}
	if d := now.Sub(time.Unix(seconds, 0)); d > tolerance || d < -tolerance {
		return "", errors.New("timestamp outside tolerance")
	}
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s.%d.", id, seconds)
	m.Write(body)
	want := m.Sum(nil)
	for _, candidate := range strings.Fields(sigs) {
		version, encoded, _ := strings.Cut(candidate, ",")
		if got, err := base64.StdEncoding.DecodeString(encoded); version == "v1" && err == nil && hmac.Equal(want, got) {
			return id, nil
		}
	}
	return "", errors.New("signature mismatch")
}

// store persists a verified envelope under its event id, once: it reports false
// when the event is already in the inbox.
func store(dir, id string, body []byte) (bool, error) {
	var env struct {
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal(body, &env) != nil || env.Type == "" || env.Data == nil {
		return false, errors.New("invalid event envelope")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, filepath.Base(id)+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.Write(body)
	return err == nil, err
}

func handler(secret, inbox string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil || len(body) == 0 {
			http.Error(w, "bad body", http.StatusRequestEntityTooLarge)
			return
		}
		id, err := verify(secret, r.Header, body, time.Now())
		if err != nil {
			http.Error(w, "invalid event or signature", http.StatusBadRequest)
			return
		}
		if _, err := store(inbox, id, body); err != nil {
			http.Error(w, "invalid event or inbox unavailable", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent) // any 2xx tells Paymux it was received
	})
}

func main() {
	secret := os.Getenv("PAYMUX_WEBHOOK_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "set PAYMUX_WEBHOOK_SECRET to the endpoint's whsec_ secret")
		os.Exit(2)
	}
	inbox := os.Getenv("PAYMUX_WEBHOOK_INBOX")
	if inbox == "" {
		inbox = "webhook-inbox"
	}
	fmt.Println("listening on 127.0.0.1:9000")
	if err := http.ListenAndServe("127.0.0.1:9000", handler(secret, inbox)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
