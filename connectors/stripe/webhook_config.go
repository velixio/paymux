package stripe

import (
	"context"
	"errors"
	"net/url"

	"github.com/velixio/paymux/pkg/connector"
)

// stripeWebhookEvents is the complete set consumed by ParseWebhook. Keep this
// list explicit: "*" would send unrelated customer data to Paymux.
var stripeWebhookEvents = []string{
	"checkout.session.completed",
	"checkout.session.async_payment_succeeded",
	"checkout.session.async_payment_failed",
	"checkout.session.expired",
	"refund.created",
	"refund.updated",
	"refund.failed",
	"charge.refund.updated",
	"charge.dispute.created",
	"charge.dispute.updated",
	"charge.dispute.closed",
	"charge.dispute.funds_withdrawn",
	"charge.dispute.funds_reinstated",
	"invoice.paid",
	"invoice.payment_failed",
	"customer.subscription.updated",
	"customer.subscription.deleted",
}

type webhookEndpoint struct {
	ID            string   `json:"id"`
	URL           string   `json:"url"`
	Status        string   `json:"status"`
	EnabledEvents []string `json:"enabled_events"`
	Secret        string   `json:"secret"`
}

type webhookEndpointList struct {
	Data    []webhookEndpoint `json:"data"`
	HasMore bool              `json:"has_more"`
}

func webhookForm(endpoint string) url.Values {
	f := url.Values{"url": {endpoint}, "description": {"Paymux provider webhook"}}
	for _, event := range stripeWebhookEvents {
		f.Add("enabled_events[]", event)
	}
	return f
}

// ConfigureWebhook creates the Stripe endpoint once, then updates the matching
// endpoint on later clicks. Stripe reveals a signing secret only on creation.
func (s *stripe) ConfigureWebhook(ctx context.Context, cfg connector.ProviderWebhookConfig) (connector.ProviderWebhookResult, error) {
	if cfg.URL == "" {
		return connector.ProviderWebhookResult{}, errors.New("stripe: webhook URL is required")
	}
	query := url.Values{"limit": {"100"}}
	for {
		var list webhookEndpointList
		if err := s.api.do(ctx, "GET", "/v1/webhook_endpoints", query, nil, &list, ""); err != nil {
			return connector.ProviderWebhookResult{}, err
		}
		for _, endpoint := range list.Data {
			if endpoint.URL != cfg.URL {
				continue
			}
			if s.cfg.WebhookSecret == "" {
				return connector.ProviderWebhookResult{}, errors.New("stripe: this webhook URL already exists, but Stripe will not reveal its signing secret again; enter the existing secret manually or remove that endpoint in Stripe before configuring it here")
			}
			form := webhookForm(cfg.URL)
			var updated webhookEndpoint
			if err := s.api.do(ctx, "POST", "/v1/webhook_endpoints/"+url.PathEscape(endpoint.ID), nil, form, &updated, ""); err != nil {
				return connector.ProviderWebhookResult{}, err
			}
			return connector.ProviderWebhookResult{}, nil
		}
		if !list.HasMore {
			break
		}
		if len(list.Data) == 0 || list.Data[len(list.Data)-1].ID == "" {
			return connector.ProviderWebhookResult{}, errors.New("stripe: webhook endpoint list has_more without a cursor")
		}
		query.Set("starting_after", list.Data[len(list.Data)-1].ID)
	}
	var created webhookEndpoint
	if err := s.api.do(ctx, "POST", "/v1/webhook_endpoints", nil, webhookForm(cfg.URL), &created, "paymux-webhook:"+cfg.URL); err != nil {
		return connector.ProviderWebhookResult{}, err
	}
	if created.ID == "" || created.Secret == "" {
		return connector.ProviderWebhookResult{}, errors.New("stripe: created webhook response lacks id or signing secret")
	}
	if created.URL != "" && created.URL != cfg.URL {
		return connector.ProviderWebhookResult{}, errors.New("stripe: created webhook response has the wrong URL")
	}
	return connector.ProviderWebhookResult{Secret: created.Secret, Created: true}, nil
}

// Compile-time checks also make accidental capability drift obvious here.
var _ connector.WebhookConfigurator = (*stripe)(nil)
