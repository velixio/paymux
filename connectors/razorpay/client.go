package razorpay

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

const apiBase = "https://api.razorpay.com"

// client is a thin typed HTTP client over the Razorpay REST API.
type client struct {
	http      *http.Client
	keyID     string
	keySecret string
}

// apiError is Razorpay's error envelope.
type apiError struct {
	Status      int
	Code        string
	Description string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("razorpay: HTTP %d %s: %s", e.Status, e.Code, e.Description)
}

// do sends the request with basic auth and decodes a JSON response into out.
// extra headers may be nil.
func (c *client) do(ctx context.Context, method, path string, query url.Values, in, out any, extra map[string]string) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	return c.send(ctx, method, path, query, body, ctype, out, extra)
}

// send sends a request with an already encoded body (empty ctype: none) and
// decodes a JSON response into out.
func (c *client) send(ctx context.Context, method, path string, query url.Values, body io.Reader, ctype string, out any, extra map[string]string) error {
	u := apiBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.keyID, c.keySecret)
	req.Header.Set("Accept", "application/json")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
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
			return fmt.Errorf("razorpay: decoding %s %s: %w", method, path, err)
		}
		return nil
	}
	return classify(resp, raw)
}

// scrubErr drops the URL from a transport error so nothing sensitive is kept.
func scrubErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// classify maps an HTTP error response to a typed SDK error. The raw
// *apiError stays in the chain so callers can inspect the description.
func classify(resp *http.Response, raw []byte) error {
	var env struct {
		Error struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	ae := &apiError{Status: resp.StatusCode, Code: env.Error.Code, Description: env.Error.Description}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %w", connector.ErrInvalidCredentials, ae)
	case resp.StatusCode == http.StatusNotFound,
		// Razorpay reports unknown ids as 400 BAD_REQUEST_ERROR (seen in the sandbox).
		resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(ae.Description), "does not exist"):
		return fmt.Errorf("%w: %w", connector.ErrNotFound, ae)
	case resp.StatusCode == http.StatusTooManyRequests:
		after := 5 * time.Second
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s >= 0 {
			after = time.Duration(s) * time.Second
		}
		return fmt.Errorf("%w: %w", connector.ErrRateLimited{RetryAfter: after}, ae)
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %w", connector.ErrTemporary, ae)
	}
	return ae
}
