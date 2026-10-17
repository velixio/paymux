package stripe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

const (
	apiBase   = "https://api.stripe.com"
	filesBase = "https://files.stripe.com"
	// apiVersion pins the wire format: Stripe changes shapes (subscription
	// periods, invoice fields) between versions, and an account's default can
	// be older or newer than the one this connector was written against.
	apiVersion = "2024-06-20"
)

// client is a thin typed client over the Stripe REST API: form-encoded
// requests, JSON responses, the secret key as a bearer token.
type client struct {
	http *http.Client
	key  string
}

// apiError is Stripe's error object.
type apiError struct {
	Status  int
	Type    string
	Code    string
	Message string
	Param   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("stripe: HTTP %d %s %s: %s", e.Status, e.Type, e.Code, e.Message)
}

// do sends a request. form is the encoded body of a POST (nil: no body), query
// the query of a GET, idem the Idempotency-Key (POSTs only).
func (c *client) do(ctx context.Context, method, path string, query, form url.Values, out any, idem string) error {
	u := apiBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idem != "" && method == http.MethodPost {
		req.Header.Set("Idempotency-Key", idem)
	}
	return c.send(req, out)
}

func (c *client) send(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Stripe-Version", apiVersion)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return req.Context().Err()
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
			return fmt.Errorf("stripe: decoding %s %s: %w", req.Method, req.URL.Path, err)
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
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	ae := &apiError{Status: resp.StatusCode, Type: env.Error.Type, Code: env.Error.Code, Message: env.Error.Message, Param: env.Error.Param}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %w", connector.ErrInvalidCredentials, ae)
	case resp.StatusCode == http.StatusNotFound, ae.Code == "resource_missing":
		return fmt.Errorf("%w: %w", connector.ErrNotFound, ae)
	case resp.StatusCode == http.StatusTooManyRequests || ae.Code == "rate_limit":
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

// upload sends a file to the Files API (multipart) and returns its id.
func (c *client) upload(ctx context.Context, name string, content []byte, purpose string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("purpose", purpose)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, name))
	h.Set("Content-Type", "application/octet-stream")
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	_, _ = part.Write(content)
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, filesBase+"/v1/files", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var out struct {
		ID string `json:"id"`
	}
	if err := c.send(req, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("stripe: file upload returned no id")
	}
	return out.ID, nil
}

// expandable is a field Stripe returns as an id, or as the whole object when
// the request asked to expand it.
type expandable[T any] struct {
	ID  string
	Obj *T
}

func (e *expandable[T]) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		return json.Unmarshal(b, &e.ID)
	}
	var idOnly struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &idOnly); err != nil {
		return err
	}
	var t T
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	e.ID, e.Obj = idOnly.ID, &t
	return nil
}

// list is a Stripe list response.
type list[T any] struct {
	Data    []T  `json:"data"`
	HasMore bool `json:"has_more"`
}
