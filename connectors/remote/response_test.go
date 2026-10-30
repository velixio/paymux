package remote

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/wire"
)

// A response is bound to the request it answers: a genuine, correctly signed
// response recorded for one call is refused when handed back for another.
func TestWireRejectsSwappedResponse(t *testing.T) {
	f := &fullFake{caps: allCaps}
	h, _ := wire.NewHandler(f, testSecret, func() time.Time { return now0 })
	var recorded *httptest.ResponseRecorder
	var replay bool
	mitm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		if replay && recorded != nil {
			rec = recorded // answer with the earlier response
		} else {
			h.ServeHTTP(rec, r)
			if strings.HasSuffix(r.URL.Path, "/fetch_payment") {
				recorded = rec
			}
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	defer mitm.Close()
	c := dial(t, mitm, testSecret)
	ctx := context.Background()

	if ps, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: "FIRST"}); err != nil || ps.ProviderRef != "FIRST" {
		t.Fatalf("genuine call: %+v %v", ps, err)
	}
	replay = true
	ps, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: "SECOND"})
	if !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Fatalf("a response recorded for another call was accepted: %+v %v", ps, err)
	}
}
