// Package inbound receives provider webhooks at POST /hooks/{provider_account_id}.
// A webhook is a hint, never the truth (spec §6.4): the handler verifies the
// signature, stores the raw event, deduplicates, enqueues a job and answers
// fast; only the job may change payment state.
package inbound

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/velixio/paymux/internal/ids"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/logctx"
	"github.com/velixio/paymux/internal/store"
)

const (
	// MaxBody is the largest accepted request body.
	MaxBody = 1 << 20
	// forgedPrefix is how much of an event that failed verification is kept.
	forgedPrefix = 128

	JobProcess   = "provider_event.process"
	JobFetchOnly = "provider_event.fetch_only"
)

var (
	// ErrUnsigned is returned by a Verifier for events the provider cannot
	// sign (e.g. PayU refund webhooks). They are stored unverified and only
	// trigger a fetch of the authoritative state.
	ErrUnsigned = errors.New("inbound: webhook is unsigned")
	// ErrUnknownAccount is returned by the Verifiers lookup for an account id
	// that does not exist; the handler answers 404.
	ErrUnknownAccount = errors.New("inbound: unknown provider account")
)

// Verifier authenticates one provider's webhook and returns the provider's
// event id ("" if the provider has none).
type Verifier interface {
	Verify(headers http.Header, body []byte) (eventID string, err error)
}

// VerifierFunc looks up the Verifier for a provider account.
type VerifierFunc func(ctx context.Context, accountID string) (Verifier, error)

// Options configures a Handler.
type Options struct {
	DB        *store.DB
	Queue     *jobs.Queue
	Verifiers VerifierFunc
	Logger    *slog.Logger
	Now       func() time.Time
	// ClientIP names the client of a request (the hub passes its trusted-proxy
	// aware function); the default is the connection's address.
	ClientIP func(*http.Request) string
	// Limits per minute. RequestsPerIP caps every request from one address
	// (default 1200). FailuresPerIP and FailuresPerAccount cap how many events
	// that fail verification are stored (defaults 20 and 100): past them a forged
	// event is answered 401 and not kept. Genuine events are never held back by
	// the failure budgets.
	RequestsPerIP, FailuresPerIP, FailuresPerAccount int
}

// Handler serves POST /hooks/{provider_account_id}.
type Handler struct {
	o                       Options
	reqIP, failIP, failAcct *window
}

// window counts events per key in fixed one-minute windows, in memory.
type window struct {
	mu    sync.Mutex
	limit int
	slot  int64
	n     map[string]int
}

func newWindow(limit int) *window { return &window{limit: limit, n: map[string]int{}} }

// allow counts one event for key and reports whether it is within the limit.
func (w *window) allow(key string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if slot := now.Unix() / 60; slot != w.slot {
		w.slot, w.n = slot, map[string]int{}
	}
	if len(w.n) > 100_000 { // a flood of distinct keys: start over rather than grow
		w.n = map[string]int{}
	}
	w.n[key]++
	return w.n[key] <= w.limit
}

// New builds a Handler.
func New(o Options) *Handler {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.RequestsPerIP <= 0 {
		o.RequestsPerIP = 1200
	}
	if o.FailuresPerIP <= 0 {
		o.FailuresPerIP = 20
	}
	if o.FailuresPerAccount <= 0 {
		o.FailuresPerAccount = 100
	}
	if o.ClientIP == nil {
		o.ClientIP = func(r *http.Request) string {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				return r.RemoteAddr
			}
			return host
		}
	}
	return &Handler{o: o, reqIP: newWindow(o.RequestsPerIP), failIP: newWindow(o.FailuresPerIP), failAcct: newWindow(o.FailuresPerAccount)}
}

// JobPayload is the payload of both provider_event jobs.
type JobPayload struct {
	ProviderEventID   string `json:"provider_event_id"` // provider_events.id
	ProviderAccountID string `json:"provider_account_id"`
}

// keepHeader is the allow-list of headers worth keeping with the raw event:
// content metadata, Standard Webhooks headers and provider-specific X-*
// headers (signatures, event ids). Credentials and proxy noise are dropped.
func keepHeader(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "content-type", "user-agent", "webhook-id", "webhook-timestamp", "webhook-signature":
		return true
	}
	if strings.HasPrefix(n, "x-forwarded-") || n == "x-real-ip" {
		return false
	}
	return strings.HasPrefix(n, "x-")
}

func allowedHeaders(h http.Header) json.RawMessage {
	keep := http.Header{}
	for k, v := range h {
		if keepHeader(k) {
			keep[k] = v
		}
	}
	b, _ := json.Marshal(keep)
	return b
}

func bodyHash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := h.o.ClientIP(r)
	if !h.reqIP.allow(ip, h.o.Now()) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	account := r.PathValue("provider_account_id")
	logctx.Note(ctx, "provider_account_id", account)
	v, err := h.o.Verifiers(ctx, account)
	switch {
	case errors.Is(err, ErrUnknownAccount):
		http.Error(w, "unknown account", http.StatusNotFound)
		return
	case err != nil:
		h.o.Logger.Error("inbound: verifier lookup", "account", account, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	eventID, verr := v.Verify(r.Header, body)
	row := store.ProviderEventRow{ID: ids.New("pev"), ProviderAccountID: account,
		Headers: allowedHeaders(r.Header), Body: body, ReceivedAt: h.o.Now()}
	var job string
	status := http.StatusOK
	defer func() { logctx.Note(ctx, "provider_event_id", row.ID, "webhook_event_id", row.ProviderEventID) }()
	switch {
	case verr == nil:
		row.Verified, job = true, JobProcess
		row.ProviderEventID = eventID
		if eventID == "" {
			row.ProviderEventID = bodyHash(body)
		}
	case errors.Is(verr, ErrUnsigned):
		job = JobFetchOnly
		// Its own key space: the id comes from a header nobody signed, so it must
		// not take the place of a signed event with the same id, and the body is
		// part of the key so a replayed id with another body is another event.
		row.ProviderEventID = "unsigned:" + bodyHash(body)
		if eventID != "" {
			row.ProviderEventID = "unsigned:" + eventID + ":" + bodyHash(body)
		}
	default:
		// Never change state. The id is namespaced so a forged event cannot
		// shadow the genuine one that carries the same provider event id.
		//
		// A forged event is evidence, not data: keep its hash, size and a short
		// prefix, not the (up to 1 MiB) body, and only while the budgets allow.
		sum := bodyHash(body)
		row.ProviderEventID = "unverified:" + sum
		row.Body = body[:min(len(body), forgedPrefix)]
		row.Error = fmt.Sprintf("verification failed: %s; size=%d sha256=%s", verr.Error(), len(body), sum)
		status = http.StatusUnauthorized
		h.o.Logger.Warn("inbound: webhook failed verification", "account", account, "err", verr)
		now := h.o.Now()
		keepIP, keepAcct := h.failIP.allow(ip, now), h.failAcct.allow(account, now)
		if !keepIP || !keepAcct {
			http.Error(w, "invalid signature", status) // answered, not kept
			return
		}
	}

	err = h.o.DB.Write(ctx, func(tx store.Tx) error {
		inserted, err := h.o.DB.InsertProviderEvent(ctx, tx, row)
		if err != nil || !inserted || job == "" {
			return err // duplicates enqueue nothing: the first delivery did
		}
		return h.o.Queue.Enqueue(ctx, tx, jobs.Job{
			Kind: job, Payload: JobPayload{ProviderEventID: row.ID, ProviderAccountID: account},
			DedupeKey: job + ":" + row.ID,
		})
	})
	if err != nil {
		h.o.Logger.Error("inbound: store event", "account", account, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if status != http.StatusOK {
		http.Error(w, "invalid signature", status)
		return
	}
	w.WriteHeader(http.StatusOK)
}
