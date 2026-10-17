package connector

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Outcomes of one provider HTTP attempt, a small closed set so that they are
// safe as metric label values.
const (
	OutcomeOK           = "ok"            // status below 400
	OutcomeClientError  = "client_error"  // 4xx other than 429
	OutcomeRateLimited  = "rate_limited"  // 429
	OutcomeServerError  = "server_error"  // 5xx
	OutcomeTimeout      = "timeout"       // deadline exceeded
	OutcomeNetworkError = "network_error" // anything else that stopped the request
)

// RequestObservation describes one HTTP attempt to a provider (a retried GET
// is observed once per attempt). It carries no URL, header or body, so it can
// never leak secrets and has a bounded set of values.
type RequestObservation struct {
	Method   string        `json:"method"`
	Status   int           `json:"status,omitempty"` // 0 when no response arrived
	Duration time.Duration `json:"duration"`
	Outcome  string        `json:"outcome"`
}

// Observer receives every RequestObservation of a client built by
// NewHTTPClient (HTTPOptions.Observer). It is connector-agnostic: the code that
// builds the client knows which connector it belongs to.
type Observer func(RequestObservation)

func observe(method string, resp *http.Response, err error, d time.Duration) RequestObservation {
	o := RequestObservation{Method: method, Duration: d}
	switch {
	case err != nil:
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			o.Outcome = OutcomeTimeout
		} else {
			o.Outcome = OutcomeNetworkError
		}
	default:
		o.Status = resp.StatusCode
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			o.Outcome = OutcomeRateLimited
		case resp.StatusCode >= 500:
			o.Outcome = OutcomeServerError
		case resp.StatusCode >= 400:
			o.Outcome = OutcomeClientError
		default:
			o.Outcome = OutcomeOK
		}
	}
	return o
}
