package paymux

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	WebhookHeaderID        = "webhook-id"
	WebhookHeaderTimestamp = "webhook-timestamp"
	WebhookHeaderSignature = "webhook-signature"
)

var (
	ErrWebhookHeaders   = errors.New("paymux: missing webhook headers")
	ErrWebhookTimestamp = errors.New("paymux: webhook timestamp invalid or outside tolerance")
	ErrWebhookSignature = errors.New("paymux: webhook signature does not match")
)

// Event is an authenticated app-webhook envelope. Event ID is supplied by the
// signed webhook-id header and is stable across retries and replays.
type Event struct {
	ID        string    `json:"-"`
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	LiveMode  bool      `json:"livemode"`
	Data      struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// DecodeObject decodes the event's resource snapshot after verification.
func (e *Event) DecodeObject(target any) error {
	if target == nil {
		return errors.New("paymux: webhook target is nil")
	}
	if err := json.Unmarshal(e.Data.Object, target); err != nil {
		return fmt.Errorf("paymux: decode webhook object: %w", err)
	}
	return nil
}

type WebhookVerifier struct {
	key       []byte
	tolerance time.Duration
	now       func() time.Time
}
type WebhookOption func(*WebhookVerifier) error

// WithWebhookTolerance sets permitted clock skew. The default is five minutes.
func WithWebhookTolerance(tolerance time.Duration) WebhookOption {
	return func(v *WebhookVerifier) error {
		if tolerance <= 0 || tolerance > time.Hour {
			return errors.New("paymux: webhook tolerance must be between zero and one hour")
		}
		v.tolerance = tolerance
		return nil
	}
}

// NewWebhookVerifier prepares a Standard Webhooks verifier for an app endpoint secret.
func NewWebhookVerifier(secret string, options ...WebhookOption) (*WebhookVerifier, error) {
	raw := strings.TrimPrefix(secret, "whsec_")
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) == 0 {
		return nil, errors.New("paymux: webhook secret is not valid base64")
	}
	v := &WebhookVerifier{key: key, tolerance: 5 * time.Minute, now: time.Now}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("paymux: nil webhook option")
		}
		if err := option(v); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// Verify authenticates the exact raw request bytes before decoding the event.
func (v *WebhookVerifier) Verify(headers http.Header, body []byte) (*Event, error) {
	id, timestamp, signatures := headers.Get(WebhookHeaderID), headers.Get(WebhookHeaderTimestamp), headers.Get(WebhookHeaderSignature)
	if id == "" || timestamp == "" || signatures == "" {
		return nil, ErrWebhookHeaders
	}
	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return nil, ErrWebhookTimestamp
	}
	delta := v.now().Sub(time.Unix(unix, 0))
	if delta > v.tolerance || delta < -v.tolerance {
		return nil, ErrWebhookTimestamp
	}
	mac := hmac.New(sha256.New, v.key)
	_, _ = mac.Write([]byte(id + "." + timestamp + "."))
	_, _ = mac.Write(body)
	want := mac.Sum(nil)
	valid := false
	for _, signature := range strings.Fields(signatures) {
		version, encoded, ok := strings.Cut(signature, ",")
		if !ok || version != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(encoded)
		if err == nil && hmac.Equal(got, want) {
			valid = true
			break
		}
	}
	if !valid {
		return nil, ErrWebhookSignature
	}
	var event Event
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, fmt.Errorf("paymux: decode webhook: %w", err)
	}
	if event.Type == "" || len(event.Data.Object) == 0 {
		return nil, errors.New("paymux: webhook event is missing type or data.object")
	}
	event.ID = id
	return &event, nil
}
