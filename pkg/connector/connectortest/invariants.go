package connectortest

import (
	"fmt"

	"github.com/paymux/paymux/pkg/connector"
)

// CheckNotifications reports the first way a ParseWebhook result breaks the
// SDK's contract (see connector.Notification.Validate), for fuzz targets and
// table tests.
func CheckNotifications(ns []connector.Notification) error {
	for i, n := range ns {
		if err := n.Validate(); err != nil {
			return fmt.Errorf("result %d: %w", i, err)
		}
	}
	return nil
}
