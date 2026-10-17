package simulator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

const confSecret = "simsec_conformance"

var confNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func signedHook(secret string, at time.Time, w wireBody) connector.WebhookRequest {
	body, _ := json.Marshal(w)
	ts := at.Unix()
	return connector.WebhookRequest{Method: "POST", Body: body, ReceivedAt: at.UnixMilli(), Headers: map[string][]string{
		sigHeader: {sign(secret, ts, body)}, timestampHeader: {strconv.FormatInt(ts, 10)}, "Content-Type": {"application/json"}}}
}

// seeded builds the connector and, when the test server behind deps.HTTP names
// a payment status, preloads that payment under the fixture's reference: the
// conformance kit builds a fresh connector (and key/value store) for every
// status it checks, so the status has to come from the server.
func seeded(cfg json.RawMessage, deps connector.Deps) (connector.Connector, error) {
	c, err := New(cfg, deps)
	if err != nil || deps.HTTP == nil {
		return c, err
	}
	res, err := deps.HTTP.Get("http://seed.invalid/status")
	if err != nil {
		return c, nil
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if status := string(b); status != "" {
		sc := c.(*conn)
		ctx := context.Background()
		mu.Lock()
		defer mu.Unlock()
		_, err = sc.newPay(ctx, payRec{Kind: kindPayment, Ref: "ref_conf", OrderID: "ord_conf", PaymentID: "pay_conf", Amount: 49900, Currency: "INR", Status: status})
	}
	return c, err
}

func statusServer(status string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(status)) })
}

func TestConformance(t *testing.T) {
	cfg := json.RawMessage(`{"webhook_secret":"` + confSecret + `"}`)
	cases := []struct {
		raw  string
		want connector.PaymentStatus
	}{{stCreated, connector.PaymentCreated}, {stPending, connector.PaymentProcessing}, {stAuthorized, connector.PaymentAuthorized},
		{stCaptured, connector.PaymentSucceeded}, {stFailed, connector.PaymentFailed}, {stCanceled, connector.PaymentCanceled}}
	var statuses []connectortest.StatusCase
	for _, c := range cases {
		statuses = append(statuses, connectortest.StatusCase{Raw: c.raw, Want: c.want, WantAmount: 49900, Handler: statusServer(c.raw)})
	}
	hook := wireBody{ID: "evt_sim_1", CreatedAt: confNow.UnixMilli(), event: event{Type: "payment.captured", Payment: &connector.PaymentSnapshot{
		ProviderRef: "ref_conf", ProviderOrderID: "ord_conf", ProviderPaymentID: "pay_conf", Status: connector.PaymentSucceeded, RawStatus: "captured", Amount: 49900, Currency: "INR"}}}
	stale := signedHook(confSecret, confNow.Add(-time.Hour), hook)
	connectortest.Run(t, seeded, connectortest.Fixtures{
		Config:   cfg,
		Now:      confNow,
		Ref:      connector.PaymentRef{ProviderRef: "ref_conf"},
		Statuses: statuses,
		Webhooks: connectortest.WebhookFixtures{
			Valid:            signedHook(confSecret, confNow, hook),
			WrongSecret:      signedHook("not_the_secret", confNow, hook),
			SignatureHeaders: []string{sigHeader, timestampHeader},
			Stale:            &stale,
		},
		Checkout: connector.CheckoutRequest{PaymentID: "pay_01", AttemptID: "att_01", AppID: "app_01", ProviderRef: "ref_checkout", Amount: 49900, Currency: "INR"},
		CheckNextAction: func(t *testing.T, na connector.NextAction) {
			if na.Kind != connector.ActionRedirect || na.URL != "https://pay.example.com/sim/ref_checkout?pa=acc" {
				t.Errorf("next action %+v", na)
			}
		},
	})
}
