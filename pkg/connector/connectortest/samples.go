package connectortest

import "github.com/paymux/paymux/pkg/connector"

// SampleValues returns one fully populated value of every SDK type that
// crosses the connector boundary. Numbers inside NextAction.Options are
// float64 because that is what encoding/json produces for map[string]any.
func SampleValues() []any {
	pay := &connector.PaymentSnapshot{
		ProviderRef: "pay_1", ProviderOrderID: "ord_1", ProviderPaymentID: "pp_1",
		Status: connector.PaymentSucceeded, RawStatus: "captured", Amount: 49900, Currency: "INR",
		Method: "upi", Fee: 1000, Tax: 180, AmountRefunded: 100, ErrorCode: "E1", ErrorMessage: "msg", CreatedAt: 1760000000000,
	}
	ref := &connector.RefundSnapshot{
		ProviderRef: "ref_1", ProviderRefundID: "rf_1", ProviderPaymentID: "pp_1",
		Status: connector.RefundPending, RawStatus: "created", Amount: 100, Currency: "INR", CreatedAt: 1760000000001,
	}
	dsp := &connector.DisputeSnapshot{
		ProviderDisputeID: "d_1", ProviderPaymentID: "pp_1", Status: connector.DisputeNeedsResponse,
		RawStatus: "open", Phase: "chargeback", Reason: "fraud", Amount: 49900, Currency: "INR", DueBy: 1760100000000, CreatedAt: 1760000000002,
	}
	mnd := &connector.MandateSnapshot{
		ProviderMandateID: "m_1", Status: "active", RawStatus: "confirmed", MaxAmount: 1500000,
		Currency: "INR", Method: "upi", ValidUntil: 1900000000000,
	}
	sub := &connector.SubscriptionSnapshot{
		ProviderSubscriptionID: "sub_1", Status: "active", RawStatus: "active", Amount: 49900,
		Currency: "INR", CurrentPeriodStart: 1, CurrentPeriodEnd: 2,
	}
	cust := connector.Customer{Name: "A B", Email: "a@example.com", Phone: "+910000000000"}
	return []any{
		connector.Info{
			Name: "x", Version: "1", DisplayName: "X",
			Capabilities: []connector.Capability{connector.CapRefund}, ConfigSchema: []byte(`{"type":"object"}`),
			Currencies: []string{"INR"}, Methods: []string{"card"}, MerchantOfRecord: true,
			SubscriptionModes: []connector.BillingMode{connector.ProviderManaged}, PreDebitPerAttempt: true,
			MandateLimits: []connector.MandateLimit{{Method: "upi", Currency: "INR", MaxAmount: 1500000}},
			RateLimit:     connector.RateLimit{RequestsPerSecond: 5, Burst: 10},
		},
		connector.CheckoutRequest{
			PaymentID: "p", AttemptID: "a", AppID: "app", ProviderRef: "r", Amount: 100, Currency: "INR",
			Description: "d", Customer: cust, Metadata: map[string]string{"k": "v"},
		},
		connector.CheckoutResult{
			ProviderOrderID: "o", ExpiresAt: 5,
			NextAction: connector.NextAction{Kind: connector.ActionSDK, ScriptURL: "https://x.example.com/s.js",
				Options: map[string]any{"key": "k", "amount": float64(100), "prefill": map[string]any{"email": "a@example.com"}}},
		},
		connector.NextAction{Kind: connector.ActionRedirect, URL: "https://x.example.com/r"},
		connector.NextAction{Kind: connector.ActionFormPost, URL: "https://x.example.com/f", Fields: map[string]string{"hash": "h"}},
		connector.ReturnRequest{
			ProviderRef: "r", Method: "POST", Query: map[string][]string{"a": {"1"}}, Form: map[string][]string{"b": {"2", "3"}},
			Headers: map[string][]string{"H": {"v"}}, Body: []byte("body"),
		},
		connector.ReturnResult{ProviderRef: "r", ProviderOrderID: "o", ProviderPaymentID: "p"},
		connector.WebhookRequest{Method: "POST", Headers: map[string][]string{"X": {"1"}}, Query: map[string][]string{"q": {"1"}}, Body: []byte(`{"a":1}`), ReceivedAt: 9},
		connector.Notification{Kind: connector.NotifyPayment, EventID: "e", EventType: "t", OccurredAt: 1, Payment: pay, Refund: ref, Dispute: dsp, Mandate: mnd, Subscription: sub},
		connector.PaymentRef{ProviderRef: "r", ProviderOrderID: "o", ProviderPaymentID: "p"},
		*pay,
		connector.RefundRequest{ProviderRef: "r", ProviderPaymentID: "p", Amount: 1, Currency: "INR", Reason: "dup"},
		connector.RefundRef{ProviderRef: "r", ProviderRefundID: "rf", ProviderPaymentID: "p"},
		*ref,
		connector.DisputeRef{ProviderDisputeID: "d"},
		*dsp,
		connector.Evidence{Text: "t", Files: map[string][]byte{"receipt": []byte("pdf")}},
		connector.SettlementSnapshot{ProviderSettlementID: "s", Amount: 1, Fees: 2, Tax: 3, Currency: "INR", Status: "processed", UTR: "u", SettledAt: 4},
		connector.SubscriptionRequest{
			SubscriptionID: "s", ProviderRef: "r", Amount: 1, Currency: "INR", Interval: "month", IntervalCount: 1,
			TotalCount: 12, StartAt: 5, Description: "d", Customer: cust, Metadata: map[string]string{"k": "v"},
		},
		connector.SubscriptionRef{ProviderSubscriptionID: "s"},
		*sub,
		connector.MandateRequest{MandateID: "m", ProviderRef: "r", MaxAmount: 1, Currency: "INR", Method: "upi", ValidUntil: 3, Customer: cust},
		connector.MandateRef{ProviderMandateID: "m"},
		*mnd,
		connector.PreDebitRequest{Mandate: connector.MandateRef{ProviderMandateID: "m"}, ProviderRef: "r", Amount: 1, Currency: "INR", DebitDate: 7},
		connector.ChargeRequest{Mandate: connector.MandateRef{ProviderMandateID: "m"}, ProviderRef: "r", Amount: 1, Currency: "INR"},
		connector.TimeRange{From: 1, To: 2},
		connector.Cursor("c"),
		connector.Page[connector.PaymentSnapshot]{Items: []connector.PaymentSnapshot{*pay}, Next: "n"},
		connector.Page[connector.RefundSnapshot]{Items: []connector.RefundSnapshot{*ref}, Next: "n"},
		connector.Page[connector.DisputeSnapshot]{Items: []connector.DisputeSnapshot{*dsp}},
	}
}
