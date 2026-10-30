package whop

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/velixio/paymux/pkg/connector"
)

// whopWebhookEvents is the complete set of events consumed by ParseWebhook.
// Keep this explicit so Paymux does not receive unrelated company data.
var whopWebhookEvents = []string{
	"payment.created",
	"payment.succeeded",
	"payment.failed",
	"payment.pending",
	"payment.requires_action",
	"payment.authorized",
	"payment.canceled",
	"refund.created",
	"refund.updated",
	"membership.activated",
	"membership.deactivated",
	"membership.trial_ending_soon",
	"membership.updated",
	"membership.cancel_at_period_end_changed",
}

type webhookEndpoint struct {
	ID            string   `json:"id"`
	URL           string   `json:"url"`
	ResourceID    string   `json:"resource_id"`
	WebhookSecret string   `json:"webhook_secret"`
	Enabled       bool     `json:"enabled"`
	Events        []string `json:"events"`
}

type webhookEndpointList struct {
	Data     []webhookEndpoint `json:"data"`
	PageInfo struct {
		EndCursor   string `json:"end_cursor"`
		HasNextPage bool   `json:"has_next_page"`
	} `json:"page_info"`
}

func (w *whop) webhookBody(endpoint string) map[string]any {
	return map[string]any{
		"url":                   endpoint,
		"resource_id":           w.cfg.CompanyID,
		"enabled":               true,
		"child_resource_events": false,
		"events":                whopWebhookEvents,
	}
}

// ConfigureWebhook creates the company webhook once and updates the matching
// endpoint on later clicks. Whop only reveals its signing secret on creation
// to API-key callers, so an existing endpoint still needs its locally stored
// secret.
func (w *whop) ConfigureWebhook(ctx context.Context, cfg connector.ProviderWebhookConfig) (connector.ProviderWebhookResult, error) {
	if cfg.URL == "" {
		return connector.ProviderWebhookResult{}, errors.New("whop: webhook URL is required")
	}
	query := url.Values{"account_id": {w.cfg.CompanyID}, "first": {"100"}}
	for {
		var list webhookEndpointList
		if err := w.do(ctx, http.MethodGet, "/webhooks", query, nil, &list, nil); err != nil {
			return connector.ProviderWebhookResult{}, err
		}
		for _, endpoint := range list.Data {
			if endpoint.URL != cfg.URL {
				continue
			}
			if w.cfg.WebhookSecret == "" {
				return connector.ProviderWebhookResult{}, errors.New("whop: this webhook URL already exists, but Whop will not reveal its signing secret again; enter the existing secret manually or remove that endpoint in Whop before configuring it here")
			}
			body := w.webhookBody(cfg.URL)
			delete(body, "resource_id") // resource attachment cannot be changed by PATCH
			var updated webhookEndpoint
			if err := w.do(ctx, http.MethodPatch, "/webhooks/"+url.PathEscape(endpoint.ID), nil, body, &updated, nil); err != nil {
				return connector.ProviderWebhookResult{}, err
			}
			return connector.ProviderWebhookResult{}, nil
		}
		if !list.PageInfo.HasNextPage {
			break
		}
		if list.PageInfo.EndCursor == "" {
			return connector.ProviderWebhookResult{}, errors.New("whop: webhook list has another page without a cursor")
		}
		query.Set("after", list.PageInfo.EndCursor)
	}

	var created webhookEndpoint
	if err := w.do(ctx, http.MethodPost, "/webhooks", nil, w.webhookBody(cfg.URL), &created, nil); err != nil {
		return connector.ProviderWebhookResult{}, err
	}
	if created.ID == "" || created.WebhookSecret == "" {
		return connector.ProviderWebhookResult{}, errors.New("whop: created webhook response lacks id or signing secret")
	}
	if created.URL != "" && created.URL != cfg.URL {
		return connector.ProviderWebhookResult{}, errors.New("whop: created webhook response has the wrong URL")
	}
	return connector.ProviderWebhookResult{Secret: created.WebhookSecret, Created: true}, nil
}

var _ connector.WebhookConfigurator = (*whop)(nil)
