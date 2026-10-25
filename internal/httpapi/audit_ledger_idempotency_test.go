package httpapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// Audit 2026-10 (ledger lane). The handler has committed its side effect (a
// payout, a refund) when the client goes away. CompleteIdempotencyKey runs on
// r.Context(), which is now cancelled, so it fails; stored stays false and the
// deferred release (on a context without cancel) deletes the reservation. The
// client never saw an answer and retries with the same Idempotency-Key: the
// handler runs a second time.
func TestAuditLedgerIdempotencyClientGoneAfterCommitRunsTwice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		box, err := secrets.NewBox(bytes.Repeat([]byte{7}, 32))
		if err != nil {
			t.Fatal(err)
		}
		a := &API{db: db, box: box, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		pr := core.Principal{AppID: "app_x", Mode: "test", OrgID: store.DefaultOrgID}
		var runs atomic.Int32
		body := []byte(`{"amount":100}`)

		call := func(disconnect bool) int {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest("POST", "/v1/payouts", bytes.NewReader(body)).WithContext(ctx)
			req.Header.Set("Idempotency-Key", "k-1")
			rec := httptest.NewRecorder()
			a.idempotent(rec, req, pr, body, func(w http.ResponseWriter, r *http.Request, _ core.Principal, _ []byte) {
				runs.Add(1) // the side effect has committed
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"po_1"}`))
				if disconnect {
					cancel() // the client hangs up before the response is stored
				}
			})
			return rec.Code
		}
		call(true)
		call(false)
		if n := runs.Load(); n != 1 {
			t.Errorf("handler ran %d times under one Idempotency-Key", n)
		}
	})
}
