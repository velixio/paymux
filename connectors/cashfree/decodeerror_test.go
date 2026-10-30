package cashfree

import (
	"context"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// A 200 answer that is not what the API documents (a maintenance page, a
// changed shape) is reported to the HTTP client, so the call and its body reach
// the error log.
func TestUndecodableAnswersAreReported(t *testing.T) {
	connectortest.RecordsDecodeFailures(t, New, testConfig, func(ctx context.Context, c connector.Connector) error {
		_, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: "REF0123456789ABCDEFG"})
		return err
	})
}
