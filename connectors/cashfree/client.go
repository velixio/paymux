package cashfree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

const (
	prodBase    = "https://api.cashfree.com/pg"
	sandboxBase = "https://sandbox.cashfree.com/pg"
	// apiVersion is sent as x-api-version: Cashfree's request and response
	// shapes depend on it.
	apiVersion = "2023-08-01"
)

// client is a thin typed client over the Cashfree PG REST API.
type client struct {
	http       *http.Client
	base       string
	id, secret string
}

// apiError is Cashfree's error object.
type apiError struct {
	Status  int
	Type    string
	Code    string
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("cashfree: HTTP %d %s %s: %s", e.Status, e.Type, e.Code, e.Message)
}

// do sends a JSON request. idem, when set, is the x-idempotency-key.
func (c *client) do(ctx context.Context, method, path string, in, out any, idem string) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("x-client-id", c.id)
	req.Header.Set("x-client-secret", c.secret)
	req.Header.Set("x-api-version", apiVersion)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idem != "" {
		req.Header.Set("x-idempotency-key", idem)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", connector.ErrTemporary, scrubErr(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%w: reading response: %v", connector.ErrTemporary, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			connector.ReportDecodeError(resp, err)
			return fmt.Errorf("cashfree: decoding %s %s: %w", method, path, err)
		}
		return nil
	}
	return classify(resp, raw)
}

func scrubErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// classify maps an error response to a typed SDK error, keeping the *apiError
// in the chain.
func classify(resp *http.Response, raw []byte) error {
	var env struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &env)
	ae := &apiError{Status: resp.StatusCode, Type: env.Type, Code: env.Code, Message: env.Message}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %w", connector.ErrInvalidCredentials, ae)
	case resp.StatusCode == http.StatusNotFound, strings.HasSuffix(ae.Code, "_not_found"):
		return fmt.Errorf("%w: %w", connector.ErrNotFound, ae)
	case resp.StatusCode == http.StatusTooManyRequests:
		after := 5 * time.Second
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s >= 0 {
			after = time.Duration(s) * time.Second
		}
		return fmt.Errorf("%w: %w", connector.ErrRateLimited{RetryAfter: after}, ae)
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %w", connector.ErrTemporary, ae)
	case resp.StatusCode >= 400:
		// 400, 409, 422 and the like: the provider says the request is invalid or
		// not allowed. The caller's to fix, with the provider's message.
		return fmt.Errorf("%w: %w", connector.ErrRejected, ae)
	}
	return ae
}
