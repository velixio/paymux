package payouts

import (
	"context"
	"errors"

	"github.com/paymux/paymux/pkg/connector"
)

// ErrUnknownPayout means no payout of this provider account matches the
// notification.
var ErrUnknownPayout = errors.New("payouts: no payout of this provider account matches the notification")

// SyncNotification handles a payout webhook.
func (s *Service) SyncNotification(ctx context.Context, providerAccountID string, n connector.Notification) error {
	return errors.New("payouts: not implemented")
}
