package whop

import (
	"context"
	"net/url"

	"github.com/velixio/paymux/pkg/connector"
)

// TestConnection implements connector.ConnectionTester: Whop can only look a
// payment up by its own id or by a checkout configuration, so the generic probe
// (a payment of a reference that cannot exist) fails before any request is made.
// This lists one payment, a read-only call the connector's key already needs.
func (w *whop) TestConnection(ctx context.Context) error {
	var list struct {
		Data []paymentEntity `json:"data"`
	}
	return w.do(ctx, "GET", "/payments", url.Values{"first": {"1"}}, nil, &list, nil)
}

var _ connector.ConnectionTester = (*whop)(nil)
