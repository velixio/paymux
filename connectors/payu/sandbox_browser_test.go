//go:build sandbox

package payu

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// A checkout that is completed by hand in a browser (or by Playwright): the
// test writes the auto-submitting form PayU's hosted checkout expects, waits for
// PayU to post the customer back to a local surl/furl listener, then checks the
// reverse hash (HandleReturn) and fetches the payment (verify_payment).
//
//	PAYU_BROWSER_DIR=/some/dir go test -tags sandbox ./connectors/payu -run SandboxBrowser -v -timeout 10m
//
// The form is written to $PAYU_BROWSER_DIR/checkout.html (it holds the merchant
// key, which PayU's form needs anyway, and the request hash; never the salt);
// open it in a browser, pay with a PayU test instrument, and the test continues.
// The amount is PAYU_BROWSER_AMOUNT in paise (default 10050).
func TestSandboxBrowserCheckout(t *testing.T) {
	dir := os.Getenv("PAYU_BROWSER_DIR")
	key, salt := os.Getenv("PAYU_TEST_KEY"), os.Getenv("PAYU_TEST_SALT")
	if dir == "" || key == "" || salt == "" {
		t.Skip("PAYU_BROWSER_DIR / PAYU_TEST_KEY / PAYU_TEST_SALT not set")
	}
	amount := int64(10050)
	if v, err := strconv.ParseInt(os.Getenv("PAYU_BROWSER_AMOUNT"), 10, 64); err == nil && v > 0 {
		amount = v
	}
	ln, err := net.Listen("tcp", "127.0.0.1:18765")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan url.Values, 1)
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			f, _ := url.ParseQuery(string(b))
			for k, v := range r.URL.Query() {
				f[k] = v
			}
			select {
			case got <- f:
			default:
			}
			fmt.Fprint(w, "<html><body>returned to Paymux</body></html>")
		}))
	}()
	defer ln.Close()

	cfg, _ := json.Marshal(config{Key: key, Salt: salt, TestMode: true})
	deps := connectortest.NewDeps(nil, time.Now())
	deps.HTTP = connector.NewHTTPClient(connector.HTTPOptions{Secrets: []string{salt}})
	base := "http://127.0.0.1:18765"
	if b := os.Getenv("PAYU_BROWSER_BASE"); b != "" {
		base = b // a public URL for surl/furl; the return is then captured by the browser tooling
	}
	deps.URLs = connectortest.URLs{Base: base}
	c, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	ref := "sbx" + strconv.FormatInt(time.Now().UnixNano()/1000, 36)
	var res connector.CheckoutResult
	if m := os.Getenv("PAYU_BROWSER_MANDATE"); m != "" { // a standing-instruction registration: card, upi or emandate
		res, err = c.(connector.MandateManager).CreateMandateCheckout(context.Background(), connector.MandateRequest{MandateID: "mdt_sbx", ProviderRef: ref, Amount: amount, MaxAmount: 150000,
			Currency: "INR", Method: m, Customer: connector.Customer{Name: "Test User", Email: "test@example.com", Phone: "9876543210"}})
	} else {
		res, err = c.CreateCheckout(context.Background(), connector.CheckoutRequest{PaymentID: "pay_sbx", AttemptID: "att_sbx", AppID: "app_sbx", ProviderRef: ref,
			Amount: amount, Currency: "INR", Description: "Paymux sandbox check", Customer: connector.Customer{Name: "Test User", Email: "test@example.com", Phone: "9876543210"}})
	}
	if err != nil || res.NextAction.Kind != connector.ActionFormPost {
		t.Fatalf("%+v %v", res, err)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `<html><body onload="document.forms[0].submit()"><form method="post" action=%q>`, res.NextAction.URL)
	names := make([]string, 0, len(res.NextAction.Fields))
	for k := range res.NextAction.Fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(&sb, `<input type="hidden" name=%q value="%s">`, k, html.EscapeString(res.NextAction.Fields[k]))
	}
	sb.WriteString(`</form></body></html>`)
	if err := os.WriteFile(filepath.Join(dir, "checkout.html"), []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ref.txt"), []byte(ref), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("open %s/checkout.html (txnid %s)", dir, ref)
	if os.Getenv("PAYU_BROWSER_BASE") != "" {
		return // the return is verified later by TestSandboxVerifyCapturedReturn
	}

	var f url.Values
	select {
	case f = <-got:
	case <-time.After(8 * time.Minute):
		t.Fatal("no return from PayU")
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("PayU returned fields: %v", keys)
	t.Logf("status=%q unmappedstatus=%q mode=%q amount=%q net_amount_debit=%q additionalCharges=%q error=%q error_Message=%q",
		f.Get("status"), f.Get("unmappedstatus"), f.Get("mode"), f.Get("amount"), f.Get("net_amount_debit"), f.Get("additionalCharges"), f.Get("error"), f.Get("error_Message"))
	_ = os.WriteFile(filepath.Join(dir, "return.txt"), []byte(f.Encode()), 0o600)

	rr, err := c.HandleReturn(context.Background(), connector.ReturnRequest{ProviderRef: ref, Method: "POST", Body: []byte(f.Encode())})
	if err != nil || rr.ProviderRef != ref || rr.ProviderPaymentID == "" {
		t.Fatalf("HandleReturn (the reverse hash must verify): %+v %v", rr, err)
	}
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: ref})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("FetchPayment: %+v", snap)
	if snap.ProviderPaymentID != rr.ProviderPaymentID || snap.Amount != amount || snap.ProviderRef != ref {
		t.Errorf("verify_payment disagrees with the return: %+v", snap)
	}
}

// TestSandboxVerifyCapturedReturn checks a PayU return (the url-encoded body of
// the surl/furl POST) captured elsewhere, in $PAYU_RETURN_FILE: the reverse hash
// must verify, and a tampered copy must not.
func TestSandboxVerifyCapturedReturn(t *testing.T) {
	file, key, salt := os.Getenv("PAYU_RETURN_FILE"), os.Getenv("PAYU_TEST_KEY"), os.Getenv("PAYU_TEST_SALT")
	if file == "" || key == "" || salt == "" {
		t.Skip("PAYU_RETURN_FILE / PAYU_TEST_KEY / PAYU_TEST_SALT not set")
	}
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := url.ParseQuery(string(body))
	cfg, _ := json.Marshal(config{Key: key, Salt: salt, TestMode: true})
	c, err := New(cfg, connectortest.NewDeps(nil, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("fields: %v", keys)
	t.Logf("status=%q unmappedstatus=%q mode=%q amount=%q additionalCharges=%q error=%q error_Message=%q", f.Get("status"), f.Get("unmappedstatus"), f.Get("mode"), f.Get("amount"), f.Get("additionalCharges"), f.Get("error"), f.Get("error_Message"))
	rr, err := c.HandleReturn(context.Background(), connector.ReturnRequest{ProviderRef: f.Get("txnid"), Method: "POST", Body: body})
	if err != nil || rr.ProviderPaymentID == "" {
		t.Fatalf("the reverse hash of a real PayU return must verify: %+v %v", rr, err)
	}
	f.Set("amount", "1.00")
	if _, err := c.HandleReturn(context.Background(), connector.ReturnRequest{ProviderRef: f.Get("txnid"), Method: "POST", Body: []byte(f.Encode())}); err == nil {
		t.Error("a tampered return verified")
	}
}
