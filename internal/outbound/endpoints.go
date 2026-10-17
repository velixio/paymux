package outbound

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/outbound/stdwebhooks"
	"github.com/paymux/paymux/internal/store"
)

// Endpoint is a webhook endpoint as shown to operators (no secret).
type Endpoint struct {
	ID, OrgID, AppID, URL string
	EventTypes            []string // empty means all event types
	Enabled               bool
}

// EndpointUpdate carries the fields to change; nil fields are left alone.
type EndpointUpdate = store.EndpointUpdate

// CreateEndpointInput describes a new endpoint. An empty OrgID means the
// only org.
type CreateEndpointInput struct {
	OrgID, AppID, URL string
	EventTypes        []string
}

func secretAAD(endpointID string) []byte {
	return []byte("webhook_endpoints.signing_secret:" + endpointID)
}

func toEndpoint(r store.EndpointRow) Endpoint {
	return Endpoint{ID: r.ID, OrgID: r.OrgID, AppID: r.AppID, URL: r.URL, EventTypes: r.EventTypes, Enabled: r.Enabled}
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("endpoint url %q must be an absolute http(s) URL", raw)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("endpoint url %q must use https:// or http://", raw)
	}
	return nil
}

// CreateEndpoint creates an enabled endpoint with a fresh signing secret. The
// returned secret ("whsec_...") is shown once; only its ciphertext is kept.
// TODO(spec): check that AppID exists once the apps table lands (another lane).
func (s *Service) CreateEndpoint(ctx context.Context, in CreateEndpointInput) (Endpoint, string, error) {
	if strings.TrimSpace(in.AppID) == "" {
		return Endpoint{}, "", errors.New("endpoint needs an app id")
	}
	if err := validateURL(in.URL); err != nil {
		return Endpoint{}, "", err
	}
	if in.OrgID == "" {
		org, err := s.db.FirstOrgID(ctx)
		if err != nil {
			return Endpoint{}, "", fmt.Errorf("no org found for the endpoint (pass an org id or run setup first): %w", err)
		}
		in.OrgID = org
	}
	secret, err := stdwebhooks.GenerateSecret()
	if err != nil {
		return Endpoint{}, "", err
	}
	id := ids.New("whe")
	enc, err := s.box.Seal([]byte(secret), secretAAD(id))
	if err != nil {
		return Endpoint{}, "", err
	}
	now := s.now()
	row := store.EndpointRow{ID: id, OrgID: in.OrgID, AppID: in.AppID, URL: in.URL, SigningSecretEnc: enc,
		EventTypes: in.EventTypes, Enabled: true, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Write(ctx, func(tx store.Tx) error { return s.db.InsertEndpoint(ctx, tx, row) }); err != nil {
		return Endpoint{}, "", err
	}
	return toEndpoint(row), secret, nil
}

// ListEndpoints lists an app's endpoints (all apps when appID is empty).
func (s *Service) ListEndpoints(ctx context.Context, appID string) ([]Endpoint, error) {
	rows, err := s.db.ListEndpoints(ctx, appID)
	if err != nil {
		return nil, err
	}
	out := make([]Endpoint, len(rows))
	for i, r := range rows {
		out[i] = toEndpoint(r)
	}
	return out, nil
}

// UpdateEndpoint changes an endpoint's URL, event types or enabled flag.
func (s *Service) UpdateEndpoint(ctx context.Context, id string, u EndpointUpdate) error {
	if u.URL != nil {
		if err := validateURL(*u.URL); err != nil {
			return err
		}
	}
	return s.db.UpdateEndpoint(ctx, id, u, s.now())
}

// DisableEndpoint stops new deliveries to the endpoint. Pending deliveries
// fail without sending.
func (s *Service) DisableEndpoint(ctx context.Context, id string) error {
	off := false
	return s.UpdateEndpoint(ctx, id, EndpointUpdate{Enabled: &off})
}
