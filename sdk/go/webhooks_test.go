package paymux

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestWebhookVerifier(t *testing.T) {
	const id = "evt_1"
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("paymux-webhook-test-key!!"))
	now := time.Unix(1614265330, 0)
	body := []byte(`{"type":"payment.succeeded","timestamp":"2021-02-25T12:22:10Z","livemode":false,"data":{"object":{"id":"pay_1","object":"payment"}}}`)
	key, _ := base64.StdEncoding.DecodeString(secret[len("whsec_"):])
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(id + "." + ts + "."))
	_, _ = mac.Write(body)
	headers := http.Header{}
	headers.Set(WebhookHeaderID, id)
	headers.Set(WebhookHeaderTimestamp, ts)
	headers.Set(WebhookHeaderSignature, "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	v, err := NewWebhookVerifier(secret)
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return now }
	event, err := v.Verify(headers, body)
	if err != nil {
		t.Fatal(err)
	}
	if event.ID != id || event.Type != "payment.succeeded" {
		t.Fatalf("event = %+v", event)
	}
	var payment Payment
	if err := event.DecodeObject(&payment); err != nil {
		t.Fatal(err)
	}
	if payment.ID != "pay_1" {
		t.Fatalf("payment = %+v", payment)
	}
	if _, err := v.Verify(headers, append(body, ' ')); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("tampered error = %v", err)
	}
	v.now = func() time.Time { return now.Add(6 * time.Minute) }
	if _, err := v.Verify(headers, body); !errors.Is(err, ErrWebhookTimestamp) {
		t.Fatalf("stale error = %v", err)
	}
}
