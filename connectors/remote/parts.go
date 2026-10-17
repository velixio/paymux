package remote

import (
	"context"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/wire"
)

// One part per optional interface. compose (combos_gen.go) embeds exactly the
// parts the remote declared.

type refundPart struct{ r *remote }

func (p refundPart) Refund(ctx context.Context, req connector.RefundRequest) (connector.RefundSnapshot, error) {
	return rpc[connector.RefundSnapshot](ctx, p.r, wire.MethodRefund, req)
}
func (p refundPart) FetchRefund(ctx context.Context, ref connector.RefundRef) (connector.RefundSnapshot, error) {
	return rpc[connector.RefundSnapshot](ctx, p.r, wire.MethodFetchRefund, ref)
}

type disputeReadPart struct{ r *remote }

func (p disputeReadPart) ListDisputes(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.DisputeSnapshot], error) {
	return rpc[connector.Page[connector.DisputeSnapshot]](ctx, p.r, wire.MethodListDisputes, wire.ListRequest{Range: tr, Cursor: cur})
}
func (p disputeReadPart) FetchDispute(ctx context.Context, ref connector.DisputeRef) (connector.DisputeSnapshot, error) {
	return rpc[connector.DisputeSnapshot](ctx, p.r, wire.MethodFetchDispute, ref)
}

type disputeRespondPart struct{ r *remote }

func (p disputeRespondPart) AcceptDispute(ctx context.Context, ref connector.DisputeRef) error {
	_, err := rpc[wire.Empty](ctx, p.r, wire.MethodAcceptDispute, ref)
	return err
}
func (p disputeRespondPart) SubmitEvidence(ctx context.Context, ref connector.DisputeRef, ev connector.Evidence) error {
	_, err := rpc[wire.Empty](ctx, p.r, wire.MethodSubmitEvidence, wire.EvidenceRequest{Dispute: ref, Evidence: ev})
	return err
}

type listPart struct{ r *remote }

func (p listPart) ListPayments(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.PaymentSnapshot], error) {
	return rpc[connector.Page[connector.PaymentSnapshot]](ctx, p.r, wire.MethodListPayments, wire.ListRequest{Range: tr, Cursor: cur})
}
func (p listPart) ListRefunds(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.RefundSnapshot], error) {
	return rpc[connector.Page[connector.RefundSnapshot]](ctx, p.r, wire.MethodListRefunds, wire.ListRequest{Range: tr, Cursor: cur})
}

type settlePart struct{ r *remote }

func (p settlePart) ListSettlements(ctx context.Context, tr connector.TimeRange) ([]connector.SettlementSnapshot, error) {
	out, err := rpc[wire.Settlements](ctx, p.r, wire.MethodListSettlements, wire.SettlementsRequest{Range: tr})
	return out.Items, err
}

type cancelPart struct{ r *remote }

func (p cancelPart) CancelCheckout(ctx context.Context, ref connector.PaymentRef) error {
	_, err := rpc[wire.Empty](ctx, p.r, wire.MethodCancelCheckout, ref)
	return err
}

type subscriptionPart struct{ r *remote }

func (p subscriptionPart) CreateSubscriptionCheckout(ctx context.Context, req connector.SubscriptionRequest) (connector.CheckoutResult, error) {
	return rpc[connector.CheckoutResult](ctx, p.r, wire.MethodCreateSubscriptionCheckout, req)
}
func (p subscriptionPart) FetchSubscription(ctx context.Context, ref connector.SubscriptionRef) (connector.SubscriptionSnapshot, error) {
	return rpc[connector.SubscriptionSnapshot](ctx, p.r, wire.MethodFetchSubscription, ref)
}
func (p subscriptionPart) CancelSubscription(ctx context.Context, ref connector.SubscriptionRef, atPeriodEnd bool) error {
	_, err := rpc[wire.Empty](ctx, p.r, wire.MethodCancelSubscription, wire.CancelSubscriptionRequest{Subscription: ref, AtPeriodEnd: atPeriodEnd})
	return err
}
func (p subscriptionPart) UpdateSubscriptionAmount(ctx context.Context, ref connector.SubscriptionRef, amount int64) error {
	_, err := rpc[wire.Empty](ctx, p.r, wire.MethodUpdateSubscriptionAmount, wire.UpdateAmountRequest{Subscription: ref, Amount: amount})
	return err
}

type mandatePart struct{ r *remote }

func (p mandatePart) CreateMandateCheckout(ctx context.Context, req connector.MandateRequest) (connector.CheckoutResult, error) {
	return rpc[connector.CheckoutResult](ctx, p.r, wire.MethodCreateMandateCheckout, req)
}
func (p mandatePart) FetchMandate(ctx context.Context, ref connector.MandateRef) (connector.MandateSnapshot, error) {
	return rpc[connector.MandateSnapshot](ctx, p.r, wire.MethodFetchMandate, ref)
}
func (p mandatePart) NotifyPreDebit(ctx context.Context, req connector.PreDebitRequest) error {
	_, err := rpc[wire.Empty](ctx, p.r, wire.MethodNotifyPreDebit, req)
	return err
}
func (p mandatePart) ChargeMandate(ctx context.Context, req connector.ChargeRequest) (connector.PaymentSnapshot, error) {
	return rpc[connector.PaymentSnapshot](ctx, p.r, wire.MethodChargeMandate, req)
}
func (p mandatePart) RevokeMandate(ctx context.Context, ref connector.MandateRef) error {
	_, err := rpc[wire.Empty](ctx, p.r, wire.MethodRevokeMandate, ref)
	return err
}

type payoutPart struct{ r *remote }

func (p payoutPart) CreatePayout(ctx context.Context, req connector.PayoutRequest) (connector.PayoutSnapshot, error) {
	return rpc[connector.PayoutSnapshot](ctx, p.r, wire.MethodCreatePayout, req)
}
func (p payoutPart) FetchPayout(ctx context.Context, ref connector.PayoutRef) (connector.PayoutSnapshot, error) {
	return rpc[connector.PayoutSnapshot](ctx, p.r, wire.MethodFetchPayout, ref)
}
