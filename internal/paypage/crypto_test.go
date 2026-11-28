package paypage_test

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// fakeCrypto is a crypto-address connector holding one invoice per checkout.
type fakeCrypto struct {
	deps connector.Deps
}

var fakeCryptoState = struct {
	sync.Mutex
	amount  map[string]int64
	invoice map[string]*connector.CryptoInvoice
}{amount: map[string]int64{}, invoice: map[string]*connector.CryptoInvoice{}}

var fakeCryptoInfo = connector.Info{Version: "0", DisplayName: "Test crypto", Currencies: []string{"USD"}, Methods: []string{"crypto"},
	ConfigSchema: json.RawMessage(`{"type":"object"}`)}

func init() {
	connector.Register("testcrypto", func(_ json.RawMessage, d connector.Deps) (connector.Connector, error) {
		return &fakeCrypto{deps: d}, nil
	}, fakeCryptoInfo)
}

func (f *fakeCrypto) Info() connector.Info { return fakeCryptoInfo }
func (f *fakeCrypto) CheckoutHosts() connector.CheckoutHosts {
	return connector.CryptoCheckoutHosts(f.deps.URLs)
}
func (f *fakeCrypto) CreateCheckout(_ context.Context, req connector.CheckoutRequest) (connector.CheckoutResult, error) {
	page, err := connector.CryptoPageURL(f.deps.URLs, req.ProviderRef)
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	fakeCryptoState.Lock()
	fakeCryptoState.amount[req.ProviderRef] = req.Amount
	fakeCryptoState.Unlock()
	return connector.CheckoutResult{NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: page}}, nil
}
func (f *fakeCrypto) HandleReturn(_ context.Context, req connector.ReturnRequest) (connector.ReturnResult, error) {
	return connector.ReturnResult{ProviderRef: req.ProviderRef}, nil
}
func (f *fakeCrypto) VerifyWebhook(context.Context, connector.WebhookRequest) error {
	return connector.ErrInvalidSignature
}
func (f *fakeCrypto) ParseWebhook(context.Context, connector.WebhookRequest) ([]connector.Notification, error) {
	return nil, connector.ErrInvalidSignature
}
func (f *fakeCrypto) FetchPayment(_ context.Context, ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
	fakeCryptoState.Lock()
	defer fakeCryptoState.Unlock()
	amount, ok := fakeCryptoState.amount[ref.ProviderRef]
	if !ok {
		return connector.PaymentSnapshot{}, connector.ErrNotFound
	}
	s := connector.PaymentSnapshot{ProviderRef: ref.ProviderRef, Status: connector.PaymentRequiresAction, Amount: amount, Currency: "USD", Method: "crypto"}
	if inv := fakeCryptoState.invoice[ref.ProviderRef]; inv != nil && inv.Status == connector.CryptoPaid {
		s.Status, s.ProviderPaymentID = connector.PaymentSucceeded, inv.ID
	}
	return s, nil
}
func (f *fakeCrypto) CryptoCoins() []connector.CryptoCoin {
	return []connector.CryptoCoin{{Currency: "USDT", Network: "TRC20", Label: "USDT · Tron"}, {Currency: "BTC", Network: "BTC", Label: "Bitcoin"}}
}
func (f *fakeCrypto) CryptoState(_ context.Context, ref string) (connector.CryptoState, error) {
	fakeCryptoState.Lock()
	defer fakeCryptoState.Unlock()
	amount, ok := fakeCryptoState.amount[ref]
	if !ok {
		return connector.CryptoState{}, connector.ErrNotFound
	}
	return connector.CryptoState{Amount: amount, Currency: "USD", Invoice: fakeCryptoState.invoice[ref]}, nil
}
func (f *fakeCrypto) ChooseCryptoCoin(ctx context.Context, ref, coin, _ string) (connector.CryptoState, error) {
	if coin != "USDT_TRC20" && coin != "BTC_BTC" {
		return connector.CryptoState{}, connector.ErrCoinRefused
	}
	fakeCryptoState.Lock()
	fakeCryptoState.invoice[ref] = &connector.CryptoInvoice{ID: "inv_1", Status: connector.CryptoPending, Coin: coin, Amount: "499.000000", Address: "TAddr123"}
	fakeCryptoState.Unlock()
	return f.CryptoState(ctx, ref)
}

func setFakeCryptoStatus(ref, status string) {
	fakeCryptoState.Lock()
	defer fakeCryptoState.Unlock()
	fakeCryptoState.invoice[ref].Status = status
}

var cryptoLink = regexp.MustCompile(`/crypto/([0-9A-Za-z]+)\?pa=(pa_[0-9a-z]+)`)

// cryptoEnv adds a crypto account, routes every payment to it and opens the
// crypto page of a new payment.
func cryptoEnv(t *testing.T, e *env) (path, ref, pa, payID string) {
	t.Helper()
	if _, err := e.conns.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{Connector: "testcrypto", Name: "crypto", Mode: "test",
		Config: json.RawMessage(`{}`), AppID: "app_1"}); err != nil {
		t.Fatal(err)
	}
	doc := `{"rules":[],"catch_all":{"name":"all","when":{},"action":{"kind":"route","targets":[{"connector":"testcrypto"}]}}}`
	if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "routing", json.RawMessage(doc)); err != nil {
		t.Fatal(err)
	}
	c := e.create(core.CreatePaymentInput{Amount: 49900})
	r := e.do("GET", c.path(), "", nil)
	m := cryptoLink.FindStringSubmatch(r.header.Get("Location"))
	if r.status != 303 || m == nil {
		t.Fatalf("pay page did not send the buyer to the crypto page: %d %q", r.status, r.header.Get("Location"))
	}
	u, _ := url.Parse(r.header.Get("Location"))
	return u.RequestURI(), m[1], m[2], c.id
}

func TestCryptoPagePicksACoinShowsTheAddressAndReturnsWhenPaid(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		path, ref, pa, payID := cryptoEnv(t, e)
		page := e.do("GET", path, "", nil)
		for _, want := range []string{"499.00", "Choose a coin", `value="USDT_TRC20"`, `value="BTC_BTC"`} {
			if page.status != 200 || !strings.Contains(page.body, want) {
				t.Fatalf("picker lacks %q: %d\n%s", want, page.status, page.body)
			}
		}
		if bad := e.do("POST", path, "", url.Values{"coin": {"DOGE_DOGE"}}); bad.status != 200 || !strings.Contains(bad.body, "be used right now") {
			t.Fatalf("refused coin: %d %s", bad.status, bad.body)
		}
		if post := e.do("POST", path, "", url.Values{"coin": {"USDT_TRC20"}}); post.status != 303 {
			t.Fatalf("choose: %d %s", post.status, post.body)
		}
		page = e.do("GET", path, "", nil)
		for _, want := range []string{"499.000000", "TAddr123", `<svg class="qr"`, `http-equiv="refresh"`} {
			if !strings.Contains(page.body, want) {
				t.Fatalf("invoice page lacks %q:\n%s", want, page.body)
			}
		}
		setFakeCryptoStatus(ref, connector.CryptoPaid)
		done := e.do("GET", path, "", nil)
		ret := "/return/" + pa + "/" + ref
		if done.status != 303 || done.header.Get("Location") != ret {
			t.Fatalf("paid page: %d %q", done.status, done.header.Get("Location"))
		}
		e.do("GET", ret, "", nil)
		if got := e.payment(payID); got.Status != "succeeded" || got.AmountReceived != 49900 {
			t.Errorf("payment after the return: %+v", got)
		}
	})
}

func TestCryptoPageIsOnlyForCryptoConnectors(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		pa := simEnv(t, e)
		if r := e.do("GET", "/crypto/anything?pa="+pa, "", nil); r.status != 404 {
			t.Fatalf("simulator account: %d", r.status)
		}
	})
}
