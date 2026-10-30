package razorpay

import (
	"errors"
	"net/http"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

// Razorpay answers unknown ids with 400 BAD_REQUEST_ERROR "The id provided
// does not exist" (observed in the sandbox, 2026-10-17), not 404.
func TestClassifyUnknownIDIsNotFound(t *testing.T) {
	raw := []byte(`{"error":{"code":"BAD_REQUEST_ERROR","description":"The id provided does not exist"}}`)
	err := classify(&http.Response{StatusCode: 400, Header: http.Header{}}, raw)
	if !errors.Is(err, connector.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("apiError not kept in chain: %v", err)
	}
	other := classify(&http.Response{StatusCode: 400, Header: http.Header{}},
		[]byte(`{"error":{"code":"BAD_REQUEST_ERROR","description":"The amount must be atleast INR 1.00"}}`))
	if errors.Is(other, connector.ErrNotFound) {
		t.Fatal("ordinary validation error mapped to ErrNotFound")
	}
}
