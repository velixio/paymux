package connectors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/pkg/connector"
)

// ErrNextActionRefused is wrapped by CheckNextAction when a connector sent the
// customer somewhere it never declared.
var ErrNextActionRefused = errors.New("the connector's next action leaves its declared checkout hosts")

// CheckNextAction is the gate every checkout passes before the customer sees
// it: the next action may only use the hosts the account's connector declared
// (connector.HostsOf: its CheckoutHoster, else Info.CheckoutHosts; a connector
// with neither may not leave the site). A refusal is logged with the URL and
// raises an ops alert naming the account, because a service that suddenly
// answers elsewhere is either broken or compromised. alerts and log may be nil.
func CheckNextAction(ctx context.Context, log *slog.Logger, alerts Alerter, accountID string, c connector.Connector, a connector.NextAction) error {
	err := a.Allowed(connector.HostsOf(c))
	if err == nil {
		return nil
	}
	name := c.Info().Name
	if log == nil {
		log = slog.Default()
	}
	log.Error("checkout: next action refused", "provider_account", accountID, "connector", name,
		"kind", string(a.Kind), "url", a.URL, "script_url", a.ScriptURL, "err", err.Error())
	if alerts != nil {
		_ = alerts.Raise(ctx, notify.Alert{Severity: notify.SeverityCritical, Kind: "provider.checkout_host_refused",
			Message: fmt.Sprintf("Provider account %s (%s) answered a checkout with a URL outside its declared hosts and was refused: %s", accountID, name, err.Error()),
			Refs:    map[string]string{"provider_account": accountID}})
	}
	return fmt.Errorf("%w: %s", ErrNextActionRefused, err.Error())
}
