package outbound_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/outbound/stdwebhooks"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestEndpointLifecycle(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		ep, secret, err := e.svc.CreateEndpoint(ctx, outbound.CreateEndpointInput{
			OrgID: "org_1", AppID: "app_1", URL: "https://example.com/hook", EventTypes: []string{"payment.succeeded"}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(ep.ID, "whe_") || !ep.Enabled || !strings.HasPrefix(secret, "whsec_") {
			t.Fatalf("%+v %q", ep, secret)
		}
		if _, err := stdwebhooks.NewSigner(secret); err != nil {
			t.Fatal(err)
		}

		// the secret is stored encrypted, bound to the endpoint id
		row, err := db.GetEndpoint(ctx, ep.ID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(row.SigningSecretEnc), secret) {
			t.Fatal("secret stored in clear")
		}
		pt, err := e.box.Open(row.SigningSecretEnc, []byte("webhook_endpoints.signing_secret:"+ep.ID))
		if err != nil || string(pt) != secret {
			t.Fatalf("open: %q %v", pt, err)
		}
		if _, err := e.box.Open(row.SigningSecretEnc, []byte("webhook_endpoints.signing_secret:other")); err == nil {
			t.Fatal("AAD not bound to the endpoint id")
		}

		list, err := e.svc.ListEndpoints(ctx, "app_1")
		if err != nil || len(list) != 1 || list[0].ID != ep.ID || !reflect.DeepEqual(list[0].EventTypes, []string{"payment.succeeded"}) {
			t.Fatalf("list %+v %v", list, err)
		}

		url, types := "https://example.com/v2", []string{}
		if err := e.svc.UpdateEndpoint(ctx, ep.ID, outbound.EndpointUpdate{URL: &url, EventTypes: &types}); err != nil {
			t.Fatal(err)
		}
		list, _ = e.svc.ListEndpoints(ctx, "app_1")
		if list[0].URL != url || len(list[0].EventTypes) != 0 {
			t.Fatalf("after update %+v", list[0])
		}
		if err := e.svc.DisableEndpoint(ctx, ep.ID); err != nil {
			t.Fatal(err)
		}
		list, _ = e.svc.ListEndpoints(ctx, "app_1")
		if list[0].Enabled {
			t.Fatal("still enabled")
		}
		if err := e.svc.DisableEndpoint(ctx, "whe_missing"); err == nil {
			t.Fatal("disable of missing endpoint succeeded")
		}
	})
}

func TestCreateEndpointValidation(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		for _, bad := range []string{"", "not a url", "ftp://example.com/x", "https://", "/relative", "javascript:alert(1)"} {
			if _, _, err := e.svc.CreateEndpoint(ctx, outbound.CreateEndpointInput{OrgID: "org_1", AppID: "app_1", URL: bad}); err == nil {
				t.Errorf("url %q accepted", bad)
			}
		}
		if _, _, err := e.svc.CreateEndpoint(ctx, outbound.CreateEndpointInput{OrgID: "org_1", URL: "https://example.com"}); err == nil {
			t.Error("missing app accepted")
		}
	})
}

// An endpoint belongs to an app that exists, in the org the caller names; with
// no org named, the app's own org is used.
func TestCreateEndpointChecksTheApp(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		if _, _, err := e.svc.CreateEndpoint(ctx, outbound.CreateEndpointInput{OrgID: "org_1", AppID: "app_nosuch", URL: "https://example.com/h"}); err == nil {
			t.Error("an endpoint for an app that does not exist was created")
		}
		if _, _, err := e.svc.CreateEndpoint(ctx, outbound.CreateEndpointInput{OrgID: "org_other", AppID: "app_1", URL: "https://example.com/h"}); err == nil {
			t.Error("an endpoint for another org's app was created")
		}
		if list, _ := e.svc.ListEndpoints(ctx, ""); len(list) != 0 {
			t.Errorf("refused endpoints were stored: %+v", list)
		}
		ep, _, err := e.svc.CreateEndpoint(ctx, outbound.CreateEndpointInput{AppID: "app_1", URL: "https://example.com/h"})
		if err != nil {
			t.Fatal(err)
		}
		row, _ := db.GetEndpoint(ctx, ep.ID)
		if row.OrgID != "org_1" {
			t.Fatalf("org %q, want the app's org_1", row.OrgID)
		}
	})
}
