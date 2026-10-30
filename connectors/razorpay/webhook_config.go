package razorpay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/velixio/paymux/pkg/connector"
)

// razorpayWebhookEvents is the complete set consumed by ParseWebhook.
var razorpayWebhookEvents = []string{
	"order.paid",
	"payment.authorized",
	"payment.captured",
	"payment.failed",
	"refund.created",
	"refund.processed",
	"refund.failed",
	"payment.dispute.created",
	"payment.dispute.under_review",
	"payment.dispute.action_required",
	"payment.dispute.won",
	"payment.dispute.lost",
	"payment.dispute.closed",
	"subscription.activated",
	"subscription.charged",
	"subscription.pending",
	"subscription.halted",
	"subscription.cancelled",
	"subscription.completed",
}

type webhookEndpoint struct {
	ID           string   `json:"id"`
	URL          string   `json:"url"`
	SecretExists bool     `json:"secret_exists"`
	Active       bool     `json:"active"`
	Events       []string `json:"events"`
}

type webhookEndpointList struct {
	Count int               `json:"count"`
	Items []webhookEndpoint `json:"items"`
}

func webhookSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (r *razorpay) webhookPath() (string, error) {
	if r.cfg.AccountID == "" {
		return "", errors.New("razorpay: account_id (acc_...) is required for automatic webhook configuration; add it to this provider account's settings")
	}
	return "/v2/accounts/" + url.PathEscape(r.cfg.AccountID) + "/webhooks", nil
}

// ConfigureWebhook uses Razorpay's account webhook API. Razorpay expects the
// caller to choose the signing secret, so Paymux generates it before creation
// and the hub stores it in the encrypted provider config after success.
func (r *razorpay) ConfigureWebhook(ctx context.Context, cfg connector.ProviderWebhookConfig) (connector.ProviderWebhookResult, error) {
	if cfg.URL == "" {
		return connector.ProviderWebhookResult{}, errors.New("razorpay: webhook URL is required")
	}
	path, err := r.webhookPath()
	if err != nil {
		return connector.ProviderWebhookResult{}, err
	}
	for skip := 0; ; skip += 100 {
		query := url.Values{"count": {"100"}, "skip": {strconv.Itoa(skip)}}
		var list webhookEndpointList
		if err := r.api.do(ctx, http.MethodGet, path, query, nil, &list, nil); err != nil {
			return connector.ProviderWebhookResult{}, err
		}
		for _, endpoint := range list.Items {
			if endpoint.URL != cfg.URL {
				continue
			}
			if r.cfg.WebhookSecret == "" {
				return connector.ProviderWebhookResult{}, errors.New("razorpay: this webhook URL already exists but its signing secret is not stored; enter the existing secret manually or delete that endpoint in Razorpay before configuring it here")
			}
			body := map[string]any{"url": cfg.URL, "events": razorpayWebhookEvents}
			var updated webhookEndpoint
			if err := r.api.do(ctx, http.MethodPatch, path+"/"+url.PathEscape(endpoint.ID), nil, body, &updated, nil); err != nil {
				return connector.ProviderWebhookResult{}, err
			}
			return connector.ProviderWebhookResult{}, nil
		}
		if len(list.Items) < 100 {
			break
		}
	}
	secret, err := webhookSecret()
	if err != nil {
		return connector.ProviderWebhookResult{}, errors.New("razorpay: generating webhook secret: " + err.Error())
	}
	body := map[string]any{"url": cfg.URL, "secret": secret, "events": razorpayWebhookEvents}
	var created webhookEndpoint
	if err := r.api.do(ctx, http.MethodPost, path, nil, body, &created, nil); err != nil {
		return connector.ProviderWebhookResult{}, err
	}
	if created.ID == "" || !created.SecretExists {
		return connector.ProviderWebhookResult{}, errors.New("razorpay: created webhook response lacks id or secret confirmation")
	}
	if created.URL != "" && created.URL != cfg.URL {
		return connector.ProviderWebhookResult{}, errors.New("razorpay: created webhook response has the wrong URL")
	}
	return connector.ProviderWebhookResult{Secret: secret, Created: true}, nil
}

var _ connector.WebhookConfigurator = (*razorpay)(nil)
