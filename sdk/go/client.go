package paymux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "http://localhost:8080"
	maxJSONBody    = 8 << 20
	maxErrorBody   = 1 << 20
)

// Client is safe for concurrent use.
type Client struct {
	apiKey    string
	baseURL   *url.URL
	http      *http.Client
	userAgent string

	Payments      *PaymentsService
	Refunds       *RefundsService
	PaymentLinks  *PaymentLinksService
	Subscriptions *SubscriptionsService
	Invoices      *InvoicesService
	CreditNotes   *CreditNotesService
	Payouts       *PayoutsService
}

// Option configures a Client.
type Option func(*clientConfig) error

type clientConfig struct {
	baseURL   string
	http      *http.Client
	userAgent string
}

// WithBaseURL sets the Paymux hub URL. Plain HTTP is accepted only for a
// loopback address, preventing accidental API-key disclosure over the network.
func WithBaseURL(raw string) Option {
	return func(c *clientConfig) error { c.baseURL = raw; return nil }
}

// WithHTTPClient supplies the transport. The caller retains ownership of it.
func WithHTTPClient(client *http.Client) Option {
	return func(c *clientConfig) error {
		if client == nil {
			return errors.New("paymux: HTTP client is nil")
		}
		c.http = client
		return nil
	}
}

// WithUserAgent appends an application identifier to the SDK user agent.
func WithUserAgent(value string) Option {
	return func(c *clientConfig) error {
		if strings.ContainsAny(value, "\r\n") {
			return errors.New("paymux: invalid user agent")
		}
		c.userAgent = strings.TrimSpace(value)
		return nil
	}
}

// New creates a merchant API client. It defaults to http://localhost:8080 for
// local self-hosted development; production callers should use WithBaseURL.
func New(apiKey string, options ...Option) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("paymux: API key is required")
	}
	if strings.ContainsAny(apiKey, "\r\n") {
		return nil, errors.New("paymux: invalid API key")
	}
	cfg := clientConfig{baseURL: defaultBaseURL, http: &http.Client{Timeout: 30 * time.Second}}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("paymux: nil client option")
		}
		if err := option(&cfg); err != nil {
			return nil, err
		}
	}
	base, err := parseBaseURL(cfg.baseURL)
	if err != nil {
		return nil, err
	}
	ua := "paymux-go/0.1"
	if cfg.userAgent != "" {
		ua += " " + cfg.userAgent
	}
	c := &Client{apiKey: apiKey, baseURL: base, http: cfg.http, userAgent: ua}
	c.Payments = &PaymentsService{client: c}
	c.Refunds = &RefundsService{client: c}
	c.PaymentLinks = &PaymentLinksService{client: c}
	c.Subscriptions = &SubscriptionsService{client: c}
	c.Invoices = &InvoicesService{client: c}
	c.CreditNotes = &CreditNotesService{client: c}
	c.Payouts = &PayoutsService{client: c}
	return c, nil
}

func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("paymux: base URL must be absolute")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("paymux: base URL must not contain credentials, query, or fragment")
	}
	if u.Scheme != "https" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if u.Scheme != "http" || (host != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return nil, errors.New("paymux: base URL must use HTTPS (HTTP is allowed only on loopback)")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/"
	return u, nil
}

// RequestOption configures one mutation request.
type RequestOption func(*requestConfig) error
type requestConfig struct{ idempotencyKey string }

// WithIdempotencyKey sets a stable key representing one logical operation.
// Reuse the same key when retrying that operation; never reuse it for another.
func WithIdempotencyKey(key string) RequestOption {
	return func(c *requestConfig) error {
		if key == "" || strings.TrimSpace(key) != key || len(key) > 255 || strings.ContainsAny(key, "\r\n") {
			return errors.New("paymux: idempotency key must be 1-255 non-whitespace-padded characters")
		}
		c.idempotencyKey = key
		return nil
	}
}

func mutationOptions(options []RequestOption) (requestConfig, error) {
	var cfg requestConfig
	for _, option := range options {
		if option == nil {
			return cfg, errors.New("paymux: nil request option")
		}
		if err := option(&cfg); err != nil {
			return cfg, err
		}
	}
	if cfg.idempotencyKey == "" {
		return cfg, errors.New("paymux: mutation requires WithIdempotencyKey")
	}
	return cfg, nil
}

// Response contains transport metadata useful for diagnostics. It is attached
// to APIError; successful high-level calls intentionally return domain values.
type Response struct {
	StatusCode       int
	RequestID        string
	IdempotentReplay bool
	RetryAfter       time.Duration
}

// APIError is returned for every non-2xx API response.
type APIError struct {
	Response
	Type    string
	Message string
}

func (e *APIError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("paymux: HTTP %d %s: %s (request %s)", e.StatusCode, e.Type, e.Message, e.RequestID)
	}
	return fmt.Sprintf("paymux: HTTP %d %s: %s", e.StatusCode, e.Type, e.Message)
}

func (e *APIError) Temporary() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}
func (e *APIError) IsNotFound() bool { return e.StatusCode == http.StatusNotFound }
func (e *APIError) IsConflict() bool { return e.StatusCode == http.StatusConflict }

type errorEnvelope struct {
	Error struct {
		Type      string `json:"type"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body, output any, mutation *requestConfig) error {
	if ctx == nil {
		return errors.New("paymux: nil context")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("paymux: encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	rel := &url.URL{Path: strings.TrimPrefix(path, "/"), RawQuery: query.Encode()}
	u := c.baseURL.ResolveReference(rel)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return fmt.Errorf("paymux: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if mutation != nil {
		req.Header.Set("Idempotency-Key", mutation.idempotencyKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("paymux: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp)
	}
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSONBody))
		return err
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBody+1))
	if err := dec.Decode(output); err != nil {
		return fmt.Errorf("paymux: decode response: %w", err)
	}
	return nil
}

func decodeAPIError(resp *http.Response) error {
	b, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	meta := Response{StatusCode: resp.StatusCode, RequestID: resp.Header.Get("X-Request-Id"), IdempotentReplay: strings.EqualFold(resp.Header.Get("Idempotent-Replay"), "true"), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	e := &APIError{Response: meta, Type: http.StatusText(resp.StatusCode), Message: strings.TrimSpace(string(b))}
	var envelope errorEnvelope
	if json.Unmarshal(b, &envelope) == nil && envelope.Error.Message != "" {
		e.Type, e.Message = envelope.Error.Type, envelope.Error.Message
		if envelope.Error.RequestID != "" {
			e.RequestID = envelope.Error.RequestID
		}
	}
	if readErr != nil {
		e.Message = readErr.Error()
	}
	return e
}

func parseRetryAfter(raw string) time.Duration {
	seconds, err := strconv.Atoi(raw)
	if err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		d := time.Until(when)
		if d > 0 {
			return d
		}
	}
	return 0
}

func objectPath(collection, id string, suffix ...string) (string, error) {
	if id == "" || strings.Contains(id, "/") {
		return "", errors.New("paymux: invalid object ID")
	}
	parts := append([]string{"v1", collection, url.PathEscape(id)}, suffix...)
	return strings.Join(parts, "/"), nil
}

func addPageQuery(q url.Values, limit int, cursor string) {
	if limit != 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("starting_after", cursor)
	}
}
