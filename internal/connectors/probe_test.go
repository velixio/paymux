package connectors

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

// probeConn is a connector whose FetchPayment fails if it is asked: a connector
// that implements ConnectionTester must be tested through it, never through a
// payment lookup (Zoho and Stripe cannot look a payment up by reference at all).
type probeConn struct {
	connector.Connector
	fetches  int
	fetchErr error
}

func (p *probeConn) FetchPayment(context.Context, connector.PaymentRef) (connector.PaymentSnapshot, error) {
	p.fetches++
	return connector.PaymentSnapshot{}, p.fetchErr
}

type testerConn struct {
	probeConn
	err error
}

func (t *testerConn) TestConnection(context.Context) error { return t.err }

func TestProbeUsesTheConnectionTesterWhenThereIsOne(t *testing.T) {
	ctx := context.Background()
	c := &testerConn{}
	if res := probe(ctx, c); !res.OK || !strings.Contains(res.Message, "accepted") {
		t.Errorf("ok: %+v", res)
	}
	c.err = fmt.Errorf("%w: zoho: the account is not connected yet", connector.ErrInvalidCredentials)
	res := probe(ctx, c)
	if res.OK || !strings.Contains(res.Message, "credentials") || !strings.Contains(res.Message, "not connected yet") {
		t.Errorf("rejected, with the connector's own explanation: %+v", res)
	}
	c.err = errors.New("connection refused")
	if res := probe(ctx, c); res.OK || !strings.Contains(res.Message, "Could not tell") || !strings.Contains(res.Message, "connection refused") {
		t.Errorf("inconclusive: %+v", res)
	}
	if c.fetches != 0 {
		t.Errorf("FetchPayment was asked %d times", c.fetches)
	}
}

func TestProbeFallsBackToTheFetchPaymentProbe(t *testing.T) {
	ctx := context.Background()
	c := &probeConn{fetchErr: connector.ErrNotFound}
	if res := probe(ctx, c); !res.OK || c.fetches != 1 {
		t.Errorf("not found proves the credentials: %+v fetches=%d", res, c.fetches)
	}
	c.fetchErr = connector.ErrInvalidCredentials
	if res := probe(ctx, c); res.OK || !strings.Contains(res.Message, "credentials") {
		t.Errorf("%+v", res)
	}
	c.fetchErr = errors.New("PaymentRef has no id")
	if res := probe(ctx, c); res.OK || !strings.Contains(res.Message, "Could not tell") {
		t.Errorf("%+v", res)
	}
}
