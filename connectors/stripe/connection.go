package stripe

import (
	"context"
	"net/url"

	"github.com/paymux/paymux/pkg/connector"
)

// TestConnection implements connector.ConnectionTester: Stripe never searches
// for a payment by reference, so the generic probe (a payment of a reference
// that cannot exist) fails before any request is made. This lists one checkout
// session, a read-only call the key needs anyway (a restricted key with the
// Checkout Sessions read permission is enough).
func (s *stripe) TestConnection(ctx context.Context) error {
	var l list[session]
	return s.api.do(ctx, "GET", "/v1/checkout/sessions", url.Values{"limit": {"1"}}, nil, &l, "")
}

var _ connector.ConnectionTester = (*stripe)(nil)
