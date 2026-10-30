package hub

import (
	"context"
	"errors"

	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/subscriptions"
	"github.com/velixio/paymux/pkg/connector"
)

// knownSubscription tells reconciliation whether Paymux has a provider's
// subscription (of one provider account).
func knownSubscription(db *store.DB) func(ctx context.Context, accountID, providerSubscriptionID string) bool {
	return func(ctx context.Context, accountID, providerSubscriptionID string) bool {
		known := false
		_ = db.Read(ctx, func(q store.Querier) error {
			_, err := store.GetSubscriptionByProviderID(ctx, q, accountID, providerSubscriptionID)
			known = err == nil
			return nil
		})
		return known
	}
}

// linkRenewal hands a provider-managed renewal payment found by
// reconciliation to the subscriptions service, exactly as a
// subscription.charged webhook would: the service records it as a renewal
// payment of the subscription (deduplicated per provider payment id). The
// subscription's current provider state is fetched first so its status is
// applied too; without a SubscriptionManager only the payment is recorded.
func linkRenewal(conns *connectors.Service, subs *subscriptions.Service) func(ctx context.Context, accountID string, pay connector.PaymentSnapshot) error {
	return func(ctx context.Context, accountID string, pay connector.PaymentSnapshot) error {
		if pay.SubscriptionID == "" {
			return errors.New("payment names no subscription")
		}
		snap := connector.SubscriptionSnapshot{ProviderSubscriptionID: pay.SubscriptionID}
		if live, err := conns.Get(ctx, accountID); err == nil {
			if mgr, ok := live.Connector.(connector.SubscriptionManager); ok {
				if fetched, err := mgr.FetchSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: pay.SubscriptionID}); err == nil {
					snap = fetched
				}
			}
		}
		return subs.HandleNotification(ctx, accountID, connector.Notification{Kind: connector.NotifySubscription,
			EventID: "recon:" + pay.ProviderPaymentID, EventType: "reconciliation.renewal", Subscription: &snap, Payment: &pay})
	}
}
