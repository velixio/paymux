package razorpay

import (
	"errors"
	"net/http"
	"testing"

	"github.com/paymux/paymux/pkg/connector"
)

// F-C: a provider's "your request is invalid or not allowed" answer (400, 409,
// 422 and the other 4xx that are not one of the specific cases) is
// connector.ErrRejected, so the API can show it as a 4xx with the provider's
// message instead of an internal error. The specific answers keep their errors
// and 5xx stays temporary.
func TestClassifyRejectedAnswers(t *testing.T) {
	body := []byte(`{"error":{"code":"BAD_REQUEST_ERROR","description":"Currency is not supported"}}`)
	for _, tc := range []struct {
		status int
		want   error
	}{
		{400, connector.ErrRejected}, {402, connector.ErrRejected}, {409, connector.ErrRejected}, {422, connector.ErrRejected},
		{401, connector.ErrInvalidCredentials}, {403, connector.ErrInvalidCredentials},
		{404, connector.ErrNotFound},
		{500, connector.ErrTemporary}, {502, connector.ErrTemporary}, {503, connector.ErrTemporary},
	} {
		err := classify(&http.Response{StatusCode: tc.status, Header: http.Header{}}, body)
		if !errors.Is(err, tc.want) {
			t.Errorf("HTTP %d: %v, want %v", tc.status, err, tc.want)
		}
		if tc.want != connector.ErrRejected && errors.Is(err, connector.ErrRejected) {
			t.Errorf("HTTP %d is also ErrRejected: %v", tc.status, err)
		}
	}
	err := classify(&http.Response{StatusCode: 429, Header: http.Header{}}, body)
	var rl connector.ErrRateLimited
	if !errors.As(err, &rl) || errors.Is(err, connector.ErrRejected) {
		t.Errorf("HTTP 429: %v", err)
	}
	// The provider's own message is kept in the error text.
	if err := classify(&http.Response{StatusCode: 400, Header: http.Header{}}, body); !contains(err.Error(), "Currency is not supported") {
		t.Errorf("the provider's message is lost: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
