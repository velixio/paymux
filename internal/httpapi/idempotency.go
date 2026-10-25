package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
)

const (
	idempotencyTTL   = 24 * time.Hour
	idempotencyStale = time.Minute // an in-progress reservation older than this is a crashed request
	maxKeyLen        = 255
)

// recorder buffers a handler's response so it can be stored before sending.
type recorder struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(code int)        { r.status = code }
func (r *recorder) Write(b []byte) (int, error) { return r.buf.Write(b) }

// idempotent runs h under the request's Idempotency-Key. The first request
// reserves the key, runs, and stores its response (sealed); a repeat with the
// same body replays it, a repeat with another body is a 409, and a repeat
// while the first is still running is a 409 to retry later. Responses with
// status 5xx are not stored, so the client may retry.
func (a *API) idempotent(w http.ResponseWriter, r *http.Request, pr core.Principal, body []byte, h handler) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) > maxKeyLen {
		a.fail(w, r, http.StatusBadRequest, "invalid_request", "Idempotency-Key must be at most 255 characters")
		return
	}
	sum := sha256.Sum256(append([]byte(r.Method+"\n"+r.URL.Path+"\n"), body...))
	hash := hex.EncodeToString(sum[:])
	aad := secrets.AADIdempotency(pr.AppID, pr.Mode, key)

	owner := ids.New("idk")
	var existing *store.IdempotencyRecord
	err := a.db.Write(r.Context(), func(tx store.Tx) (err error) {
		existing, err = store.ReserveIdempotencyKey(r.Context(), tx, pr.AppID, pr.Mode, key, owner, hash, time.Now(), idempotencyTTL, idempotencyStale)
		return
	})
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if existing != nil {
		a.replay(w, r, existing, hash, aad)
		return
	}

	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	keep := false
	defer func() {
		if keep {
			return
		}
		// The handler panicked or failed with a server error: free the key.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		_ = a.db.Write(ctx, func(tx store.Tx) error {
			return store.ReleaseIdempotencyKey(ctx, tx, pr.AppID, pr.Mode, key, owner)
		})
	}()
	h(rec, r, pr, body)

	if rec.status < 500 {
		// The handler's effect (a payout, a refund) may have committed, so the
		// key is never freed from here on: a client that hung up must not be
		// able to run the request again. Storing the response therefore does
		// not use the request's context, and if it still fails the reservation
		// stays in progress (a retry gets 409) rather than being released.
		keep = true
		sealed, err := a.box.Seal(rec.buf.Bytes(), aad)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			err = a.db.Write(ctx, func(tx store.Tx) error {
				return store.CompleteIdempotencyKey(ctx, tx, pr.AppID, pr.Mode, key, owner, rec.status, sealed)
			})
			cancel()
		}
		if err != nil {
			a.log.Error("store idempotent response", "err", err) // the response is still returned
		}
	}
	for k, v := range rec.header {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.status)
	_, _ = w.Write(rec.buf.Bytes())
}

func (a *API) replay(w http.ResponseWriter, r *http.Request, rec *store.IdempotencyRecord, hash string, aad []byte) {
	switch {
	case rec.RequestHash != hash:
		a.fail(w, r, http.StatusConflict, "idempotency_key_reuse",
			"this Idempotency-Key was already used with a different request")
	case rec.Status == 0:
		w.Header().Set("Retry-After", "1")
		a.fail(w, r, http.StatusConflict, "idempotency_in_progress",
			"a request with this Idempotency-Key is still being processed; retry shortly")
	default:
		body, err := a.box.Open(rec.ResponseEnc, aad)
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotent-Replay", "true")
		w.WriteHeader(rec.Status)
		_, _ = w.Write(body)
	}
}
