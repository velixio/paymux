package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/redact"
	"github.com/velixio/paymux/pkg/connector"
)

func answer(t *testing.T, a *API, err error) (int, map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.serverError(rec, httptest.NewRequest("POST", "/v1/subscriptions/sub_1/update", nil), err)
	raw, _ := io.ReadAll(rec.Body)
	var body struct {
		Error map[string]any `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	return rec.Code, body.Error, string(raw)
}

// F-C: an error that is not one of the API's own, but is a connector's "the
// provider refused this request", reaches the caller as a 4xx with the provider's
// message (redacted and bounded), not as "internal error" (500).
func TestProviderRejectionsAreA4xxWithARedactedBoundedMessage(t *testing.T) {
	const secret = "key_secret_9f8e7d6c5b4a"
	red := redact.New()
	red.SetKnown("account:pa_1", secret)
	a := &API{log: slog.New(slog.NewTextHandler(io.Discard, nil)), redact: red.Text}

	// The provider's message passes through.
	err := fmt.Errorf("razorpay: HTTP 400 BAD_REQUEST_ERROR: Only offers can be updated for subscriptions when payment mode is domestic card.: %w", connector.ErrRejected)
	status, e, raw := answer(t, a, err)
	if status != http.StatusUnprocessableEntity || e["type"] != "provider_rejected" || !strings.Contains(fmt.Sprint(e["message"]), "Only offers can be updated") {
		t.Fatalf("rejected: %d %s", status, raw)
	}

	// A message that echoes a configured secret does not leak it.
	leak := fmt.Errorf("provider: invalid request for key %s: %w", secret, connector.ErrRejected)
	status, _, raw = answer(t, a, leak)
	if status != 422 || strings.Contains(raw, secret) {
		t.Errorf("the secret reached the response: %d %s", status, raw)
	}
	// Nor does a credential-shaped value the redactor was never told about.
	pattern := fmt.Errorf("provider: bad key sk_live_51Habcdefghijklmnopqrstuvwx: %w", connector.ErrRejected)
	if _, _, raw = answer(t, a, pattern); strings.Contains(raw, "sk_live_51Habcdefghijklmnopqrstuvwx") {
		t.Errorf("a provider key pattern reached the response: %s", raw)
	}

	// The message is bounded.
	long := fmt.Errorf("provider said %s: %w", strings.Repeat("x", 5000), connector.ErrRejected)
	_, e, raw = answer(t, a, long)
	if m := fmt.Sprint(e["message"]); len(m) > 400 || len(raw) > 700 {
		t.Errorf("the message is not bounded: %d bytes (body %d)", len(m), len(raw))
	}

	// Unsupported is a 422 too.
	status, e, _ = answer(t, a, fmt.Errorf("razorpay cannot change the plan: %w", connector.ErrUnsupported))
	if status != 422 || e["type"] != "unsupported" {
		t.Errorf("unsupported: %d %v", status, e)
	}
	// Anything else stays an internal error that says nothing.
	status, e, raw = answer(t, a, errors.New("boom: "+secret))
	if status != 500 || e["type"] != "internal" || strings.Contains(raw, "boom") || strings.Contains(raw, secret) {
		t.Errorf("internal: %d %s", status, raw)
	}
}
