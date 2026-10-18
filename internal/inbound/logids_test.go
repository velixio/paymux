package inbound_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/logctx"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// A webhook tells the request scope which account and event it is, and the id
// of the provider_events row that holds it, so the log row of a rejected
// webhook links to the stored event instead of repeating it.
func TestWebhookNotesAccountAndEventIDs(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.acc["pa_1"] = fakeVerifier(func(http.Header, []byte) (string, error) { return "evt_prov_7", nil })
		e.acc["pa_bad"] = fakeVerifier(func(http.Header, []byte) (string, error) { return "", errors.New("signature mismatch") })
		for account, wantStatus := range map[string]int{"pa_1": 200, "pa_bad": 401} {
			scope := logctx.Begin(ctx)
			req := httptest.NewRequest("POST", "/hooks/"+account, bytes.NewReader([]byte(`{"x":1}`))).WithContext(scope)
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			if rec.Code != wantStatus {
				t.Fatalf("%s: %d", account, rec.Code)
			}
			got := logctx.IDs(scope)
			if got["provider_account_id"] != account || !strings.HasPrefix(got["provider_event_id"], "pev_") {
				t.Errorf("%s: %v", account, got)
			}
			if account == "pa_1" && got["webhook_event_id"] != "evt_prov_7" {
				t.Errorf("provider event id: %v", got)
			}
		}
	})
}
