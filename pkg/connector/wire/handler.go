package wire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

const maxBody = 8 << 20

// errBadRequest marks a body the handler could not decode.
type errBadRequest struct{ err error }

func (e errBadRequest) Error() string { return "bad request: " + e.err.Error() }

// NewHandler serves c over the wire protocol. Requests whose signature does
// not verify under secret get 401; now may be nil for the wall clock. The
// handler can be mounted under any path prefix: only the last path segment
// names the method.
//
// Each request's message id is accepted once: a replay of a signed request
// within the tolerance window gets 401. The ids are remembered in memory (see
// DefaultReplayCacheSize and WithReplayCacheSize); several instances of one
// service need a shared store to reject a replay sent to another instance.
func NewHandler(c connector.Connector, secret string, now func() time.Time, opts ...HandlerOption) (http.Handler, error) {
	s, err := NewSigner(secret)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	h := &handler{c: c, signer: s, now: now}
	size := DefaultReplayCacheSize
	for _, o := range opts {
		o(&size)
	}
	h.seen = newReplayCache(size, 2*Tolerance)
	return h, nil
}

// HandlerOption configures NewHandler.
type HandlerOption func(replayCacheSize *int)

// WithReplayCacheSize sets how many message ids the handler remembers.
func WithReplayCacheSize(n int) HandlerOption { return func(size *int) { *size = n } }

type handler struct {
	c      connector.Connector
	signer *Signer
	now    func() time.Time
	seen   *replayCache
}

func (h *handler) reply(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":{"code":"internal","message":"unencodable result"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	h.signer.SignResponse(w.Header(), r.Header.Get(HeaderID), body, h.now())
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var bad errBadRequest
	if errors.As(err, &bad) {
		h.reply(w, r, http.StatusBadRequest, Envelope{Error{Code: CodeBadRequest, Message: err.Error()}})
		return
	}
	status, e := FromError(err)
	h.reply(w, r, status, Envelope{e})
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		h.fail(w, r, errBadRequest{err})
		return
	}
	method := path.Base(r.URL.Path)
	if err := h.signer.VerifyRequest(r.Header, r.Method, method, body, h.now()); err != nil {
		h.reply(w, r, http.StatusUnauthorized, Envelope{Error{Code: CodeInvalidCredentials, Message: "request signature missing, wrong, stale or for another method"}})
		return
	}
	// Only a correctly signed id is remembered, so unsigned junk cannot fill the cache.
	if !h.seen.firstSeen(r.Header.Get(HeaderID), h.now()) {
		h.reply(w, r, http.StatusUnauthorized, Envelope{Error{Code: CodeInvalidCredentials, Message: "message id already used (replayed request)"}})
		return
	}
	if r.Method == http.MethodGet && method == MethodInfo {
		h.reply(w, r, http.StatusOK, h.c.Info())
		return
	}
	if r.Method != http.MethodPost {
		h.reply(w, r, http.StatusMethodNotAllowed, Envelope{Error{Code: CodeBadRequest, Message: "use POST"}})
		return
	}
	out, err := h.dispatch(r.Context(), method, body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.reply(w, r, http.StatusOK, out)
}

// call decodes the body as In, runs f and returns its result.
func call[In, Out any](body []byte, f func(In) (Out, error)) (any, error) {
	var in In
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, errBadRequest{err}
	}
	return f(in)
}

// ack runs f for a call that returns nothing.
func ack[In any](body []byte, f func(In) error) (any, error) {
	return call(body, func(in In) (Empty, error) { return Empty{}, f(in) })
}

// optional returns c as T, or ErrUnsupported when it does not implement it.
func optional[T any](c connector.Connector, method string) (T, error) {
	t, ok := c.(T)
	if !ok {
		return t, fmt.Errorf("%w: %s", connector.ErrUnsupported, method)
	}
	return t, nil
}

func (h *handler) dispatch(ctx context.Context, method string, body []byte) (any, error) {
	c := h.c
	switch method {
	case MethodCreateCheckout:
		return call(body, func(in connector.CheckoutRequest) (connector.CheckoutResult, error) { return c.CreateCheckout(ctx, in) })
	case MethodHandleReturn:
		return call(body, func(in connector.ReturnRequest) (connector.ReturnResult, error) { return c.HandleReturn(ctx, in) })
	case MethodVerifyWebhook:
		return ack(body, func(in connector.WebhookRequest) error { return c.VerifyWebhook(ctx, in) })
	case MethodParseWebhook:
		return call(body, func(in connector.WebhookRequest) (Notifications, error) {
			ns, err := c.ParseWebhook(ctx, in)
			return Notifications{Items: ns}, err
		})
	case MethodFetchPayment:
		return call(body, func(in connector.PaymentRef) (connector.PaymentSnapshot, error) { return c.FetchPayment(ctx, in) })
	}
	switch method {
	case MethodRefund, MethodFetchRefund:
		x, err := optional[connector.Refunder](c, method)
		if err != nil {
			return nil, err
		}
		if method == MethodRefund {
			return call(body, func(in connector.RefundRequest) (connector.RefundSnapshot, error) { return x.Refund(ctx, in) })
		}
		return call(body, func(in connector.RefundRef) (connector.RefundSnapshot, error) { return x.FetchRefund(ctx, in) })
	case MethodListDisputes, MethodFetchDispute:
		x, err := optional[connector.DisputeReader](c, method)
		if err != nil {
			return nil, err
		}
		if method == MethodListDisputes {
			return call(body, func(in ListRequest) (connector.Page[connector.DisputeSnapshot], error) {
				return x.ListDisputes(ctx, in.Range, in.Cursor)
			})
		}
		return call(body, func(in connector.DisputeRef) (connector.DisputeSnapshot, error) { return x.FetchDispute(ctx, in) })
	case MethodAcceptDispute, MethodSubmitEvidence:
		x, err := optional[connector.DisputeResponder](c, method)
		if err != nil {
			return nil, err
		}
		if method == MethodAcceptDispute {
			return ack(body, func(in connector.DisputeRef) error { return x.AcceptDispute(ctx, in) })
		}
		return ack(body, func(in EvidenceRequest) error { return x.SubmitEvidence(ctx, in.Dispute, in.Evidence) })
	case MethodListPayments, MethodListRefunds:
		x, err := optional[connector.Lister](c, method)
		if err != nil {
			return nil, err
		}
		if method == MethodListPayments {
			return call(body, func(in ListRequest) (connector.Page[connector.PaymentSnapshot], error) {
				return x.ListPayments(ctx, in.Range, in.Cursor)
			})
		}
		return call(body, func(in ListRequest) (connector.Page[connector.RefundSnapshot], error) {
			return x.ListRefunds(ctx, in.Range, in.Cursor)
		})
	case MethodListSettlements:
		x, err := optional[connector.Settler](c, method)
		if err != nil {
			return nil, err
		}
		return call(body, func(in SettlementsRequest) (Settlements, error) {
			items, err := x.ListSettlements(ctx, in.Range)
			return Settlements{Items: items}, err
		})
	case MethodCancelCheckout:
		x, err := optional[connector.Canceler](c, method)
		if err != nil {
			return nil, err
		}
		return ack(body, func(in connector.PaymentRef) error { return x.CancelCheckout(ctx, in) })
	case MethodCreateSubscriptionCheckout, MethodFetchSubscription, MethodCancelSubscription, MethodUpdateSubscriptionAmount:
		x, err := optional[connector.SubscriptionManager](c, method)
		if err != nil {
			return nil, err
		}
		switch method {
		case MethodCreateSubscriptionCheckout:
			return call(body, func(in connector.SubscriptionRequest) (connector.CheckoutResult, error) {
				return x.CreateSubscriptionCheckout(ctx, in)
			})
		case MethodFetchSubscription:
			return call(body, func(in connector.SubscriptionRef) (connector.SubscriptionSnapshot, error) {
				return x.FetchSubscription(ctx, in)
			})
		case MethodCancelSubscription:
			return ack(body, func(in CancelSubscriptionRequest) error {
				return x.CancelSubscription(ctx, in.Subscription, in.AtPeriodEnd)
			})
		}
		return ack(body, func(in UpdateAmountRequest) error { return x.UpdateSubscriptionAmount(ctx, in.Subscription, in.Amount) })
	case MethodCreatePayout, MethodFetchPayout:
		x, err := optional[connector.Payouter](c, method)
		if err != nil {
			return nil, err
		}
		if method == MethodCreatePayout {
			return call(body, func(in connector.PayoutRequest) (connector.PayoutSnapshot, error) { return x.CreatePayout(ctx, in) })
		}
		return call(body, func(in connector.PayoutRef) (connector.PayoutSnapshot, error) { return x.FetchPayout(ctx, in) })
	case MethodCreateMandateCheckout, MethodFetchMandate, MethodNotifyPreDebit, MethodChargeMandate, MethodRevokeMandate:
		x, err := optional[connector.MandateManager](c, method)
		if err != nil {
			return nil, err
		}
		switch method {
		case MethodCreateMandateCheckout:
			return call(body, func(in connector.MandateRequest) (connector.CheckoutResult, error) {
				return x.CreateMandateCheckout(ctx, in)
			})
		case MethodFetchMandate:
			return call(body, func(in connector.MandateRef) (connector.MandateSnapshot, error) { return x.FetchMandate(ctx, in) })
		case MethodNotifyPreDebit:
			return ack(body, func(in connector.PreDebitRequest) error { return x.NotifyPreDebit(ctx, in) })
		case MethodChargeMandate:
			return call(body, func(in connector.ChargeRequest) (connector.PaymentSnapshot, error) { return x.ChargeMandate(ctx, in) })
		}
		return ack(body, func(in connector.MandateRef) error { return x.RevokeMandate(ctx, in) })
	}
	return nil, errBadRequest{fmt.Errorf("unknown method %q", method)}
}
