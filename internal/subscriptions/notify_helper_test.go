package subscriptions_test

import "github.com/paymux/paymux/pkg/connector"

// notify delivers a provider webhook notification whose subscription state the
// provider's API agrees with (the hub checks what a webhook says against it).
func (e *env) notify(account string, n connector.Notification) error {
	if n.Subscription != nil {
		e.r.mu.Lock()
		if e.r.subTruth == nil {
			e.r.subTruth = map[string]connector.SubscriptionSnapshot{}
		}
		e.r.subTruth[n.Subscription.ProviderSubscriptionID] = *n.Subscription
		e.r.mu.Unlock()
	}
	return e.subs.HandleNotification(ctx, account, n)
}
