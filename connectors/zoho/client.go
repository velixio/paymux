package zoho

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
	"sync"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

const tokenKey = "zoho:access_token"

// client is a thin typed client for Zoho Payments with OAuth refresh-token
// auth. The access token is cached in Deps.KV for its lifetime.
type client struct {
	http *http.Client
	kv   connector.KV
	now  func() time.Time
	cfg  config

	mu sync.Mutex // serializes token refreshes
}

func (c *client) apiBase() string  { return "https://payments.zoho." + c.cfg.dc() }
func (c *client) tokenURL() string { return "https://accounts.zoho." + c.cfg.dc() + "/oauth/v2/token" }
func (c *client) acct() url.Values { return url.Values{"account_id": {c.cfg.AccountID}} }
func (c *client) accessToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force {
		if b, ok, err := c.kv.Get(ctx, tokenKey); err == nil && ok {
			return string(b), nil
		}
	} else {
		_ = c.kv.Delete(ctx, tokenKey)
	}
	form := url.Values{
		"grant_type": {"refresh_token"}, "client_id": {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret}, "refresh_token": {c.cfg.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", transportErr(ctx, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(raw, &tok)
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("%w: oauth HTTP %d", connector.ErrTemporary, resp.StatusCode)
	}
	if resp.StatusCode != 200 || tok.AccessToken == "" {
		// Zoho answers 200 with {"error":"invalid_code"} for a bad refresh token.
		return "", fmt.Errorf("%w: oauth refresh failed (%s)", connector.ErrInvalidCredentials, firstNonEmpty(tok.Error, strconv.Itoa(resp.StatusCode)))
	}
	ttl := time.Duration(tok.ExpiresIn)*time.Second - time.Minute // refresh a minute early
	if tok.ExpiresIn <= 0 {
		ttl = 30 * time.Minute
	}
	if ttl > 0 {
		_ = c.kv.Set(ctx, tokenKey, []byte(tok.AccessToken), ttl)
	}
	return tok.AccessToken, nil
}

func transportErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("%w: %v", connector.ErrTemporary, err)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

type apiError struct {
	Status  int
	Code    int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("zoho: HTTP %d code %d: %s", e.Status, e.Code, e.Message)
}

// do calls an API path (with account_id added). A 401 triggers one token
// refresh and one retry.
func (c *client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	for attempt := 0; ; attempt++ {
		tok, err := c.accessToken(ctx, attempt > 0)
		if err != nil {
			return err
		}
		err = c.once(ctx, tok, method, path, query, in, out)
		var ae *apiError
		if attempt == 0 && errors.As(err, &ae) && ae.Status == 401 {
			continue
		}
		return err
	}
}

func (c *client) once(ctx context.Context, tok, method, path string, query url.Values, in, out any) error {
	q := c.acct()
	for k, v := range query {
		q[k] = v
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase()+path+"?"+q.Encode(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Zoho-oauthtoken "+tok)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return transportErr(ctx, err)
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
			return fmt.Errorf("zoho: decoding %s %s: %w", method, path, err)
		}
		return nil
	}
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &env)
	ae := &apiError{Status: resp.StatusCode, Code: env.Code, Message: env.Message}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return fmt.Errorf("%w: %w", connector.ErrInvalidCredentials, ae)
	case resp.StatusCode == 404:
		return fmt.Errorf("%w: %w", connector.ErrNotFound, ae)
	case resp.StatusCode == 429:
		after := 10 * time.Second
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s >= 0 {
			after = time.Duration(s) * time.Second
		}
		return fmt.Errorf("%w: %w", connector.ErrRateLimited{RetryAfter: after}, ae)
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %w", connector.ErrTemporary, ae)
	}
	return ae
}
