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
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/store"
)

const (
	// MaxBody is the largest accepted request body.
	MaxBody = 1 << 20

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
}

// Handler serves POST /hooks/{provider_account_id}.
type Handler struct{ o Options }

// New builds a Handler.
func New(o Options) *Handler {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Handler{o: o}
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
	account := r.PathValue("provider_account_id")
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
	switch {
	case verr == nil:
		row.Verified, job = true, JobProcess
		row.ProviderEventID = eventID
		if eventID == "" {
			row.ProviderEventID = bodyHash(body)
		}
	case errors.Is(verr, ErrUnsigned):
		job = JobFetchOnly
		row.ProviderEventID = eventID
		if eventID == "" {
			row.ProviderEventID = bodyHash(body)
		}
	default:
		// Never change state. The id is namespaced so a forged event cannot
		// shadow the genuine one that carries the same provider event id.
		row.ProviderEventID = "unverified:" + bodyHash(body)
		row.Error = "verification failed: " + verr.Error()
		status = http.StatusUnauthorized
		h.o.Logger.Warn("inbound: webhook failed verification", "account", account, "err", verr)
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
