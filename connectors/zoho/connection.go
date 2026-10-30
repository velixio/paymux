package zoho

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/velixio/paymux/pkg/connector"
)

// TestConnection implements connector.ConnectionTester. Zoho can only look a
// payment up by its own ids, so the generic "ask about a payment that cannot
// exist" probe fails before any request is made. This refreshes the access
// token and makes one read-only call that the scopes the connector already needs
// allow: the first page of one payment (ZohoPay.payments.READ).
//
// The mistake it is built to name: Zoho has two servers (payments.zoho.* and
// paymentssandbox.zoho.*), each with its own scope prefix (ZohoPay.* and
// ZohoPaySandbox.*), and a token for one is answered "Not An Authorized User"
// (401) by the other, which looks exactly like a missing scope. When the first
// call is refused, the other server is asked with the same token; if that one
// accepts it, the account is set to the wrong server and the error says so.
func (z *zoho) TestConnection(ctx context.Context) error {
	tok, err := z.api.accessToken(ctx, true)
	if err != nil {
		if errors.Is(err, connector.ErrInvalidCredentials) && z.cfg.RefreshToken != "" {
			host := z.cfg.accountsHost()
			return fmt.Errorf("%w: Zoho refused the refresh token at %s (%s). The usual causes: the data centre setting is not the one this Zoho account lives in (this connection asks %s), the client id and secret are not the ones the token was granted to, or the grant was revoked; connect the account again",
				connector.ErrInvalidCredentials, host, strings.TrimPrefix(err.Error(), connector.ErrInvalidCredentials.Error()+": "), host)
		}
		return err
	}
	q := url.Values{"per_page": {"1"}}
	err = z.api.onceAt(ctx, z.api.apiBase(), tok, "GET", "/api/v1/payments", q, nil, nil)
	if err == nil {
		return nil
	}
	if !errors.Is(err, connector.ErrInvalidCredentials) {
		return err // an outage or a rate limit tells nothing about the credentials
	}
	here, there := "live", "sandbox"
	if z.cfg.Sandbox {
		here, there = there, here
	}
	if alt := z.api.otherBase(); alt != "" {
		if z.api.onceAt(ctx, alt, tok, "GET", "/api/v1/payments", q, nil, nil) == nil {
			return fmt.Errorf("%w: wrong server: this token belongs to Zoho's %s server, but the account is set to %s. Turn the Sandbox setting %s and connect again with %s scopes",
				connector.ErrInvalidCredentials, there, here, map[bool]string{true: "off", false: "on"}[z.cfg.Sandbox], scopePrefix(!z.cfg.Sandbox))
		}
	}
	return fmt.Errorf("%w: the %s server refused the token (Not An Authorized User): the account id may be wrong, or the token lacks %s.payments.READ; connect the account again",
		connector.ErrInvalidCredentials, here, strings.TrimSuffix(scopePrefix(z.cfg.Sandbox), "."))
}

// scopePrefix is the prefix of Zoho's scope names on a server.
func scopePrefix(sandbox bool) string {
	if sandbox {
		return "ZohoPaySandbox."
	}
	return "ZohoPay."
}

var _ connector.ConnectionTester = (*zoho)(nil)
