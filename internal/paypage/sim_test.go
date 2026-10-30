package paypage_test

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/velixio/paymux/connectors/simulator"
	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
)

// simEnv adds a simulator account and routes every payment to it.
func simEnv(t *testing.T, e *env) (accountID string) {
	t.Helper()
	pa, err := e.conns.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{Connector: simulator.Name, Name: "sim", Mode: "test",
		Config: json.RawMessage(`{}`), AppID: "app_1"})
	if err != nil {
		t.Fatal(err)
	}
	doc := `{"rules":[],"catch_all":{"name":"all","when":{},"action":{"kind":"route","targets":[{"connector":"simulator"}]}}}`
	if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "routing", json.RawMessage(doc)); err != nil {
		t.Fatal(err)
	}
	return pa.ID
}

var simLink = regexp.MustCompile(`/sim/([0-9A-Za-z]+)\?pa=(pa_[0-9a-z]+)`)

// openSim creates a payment, opens its pay page and follows the redirect to the
// hosted simulator page; it returns the page path and the reference.
func openSim(t *testing.T, e *env) (path, ref, pa string, payID string) {
	t.Helper()
	c := e.create(core.CreatePaymentInput{Amount: 49900})
	r := e.do("GET", c.path(), "", nil)
	loc := r.header.Get("Location")
	m := simLink.FindStringSubmatch(loc)
	if r.status != 303 || m == nil {
		t.Fatalf("pay page did not send the customer to the simulator: %d %q", r.status, loc)
	}
	u, _ := url.Parse(loc)
	return u.RequestURI(), m[1], m[2], c.id
}

func TestSimulatorPageOffersTheChoicesAndReturnsThroughReturn(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		accountID := simEnv(t, e)
		path, ref, pa, payID := openSim(t, e)
		if pa != accountID {
			t.Fatalf("account %s vs %s", pa, accountID)
		}
		page := e.do("GET", path, "", nil)
		if page.status != 200 || !strings.Contains(page.header.Get("Cache-Control"), "no-store") {
			t.Fatalf("page: %d %v", page.status, page.header)
		}
		for _, want := range []string{"Succeed", "Fail", "Pending", "Authorize", "Late success", "USD", "499.00", `action="/sim/` + ref, `name="action"`, "test mode"} {
			if !strings.Contains(page.body, want) {
				t.Errorf("page lacks %q:\n%s", want, page.body)
			}
		}

		// Succeed: back through the normal return path, which fetches the truth.
		post := e.do("POST", path, "", url.Values{"action": {"succeed"}})
		wantReturn := "/return/" + pa + "/" + ref
		if post.status != 303 || post.header.Get("Location") != wantReturn {
			t.Fatalf("decision: %d %q, want a redirect to %s", post.status, post.header.Get("Location"), wantReturn)
		}
		ret := e.do("GET", wantReturn, "", nil)
		if ret.status != 303 && ret.status != 200 {
			t.Fatalf("return: %d %s", ret.status, ret.body)
		}
		if got := e.payment(payID); got.Status != "succeeded" || got.AmountReceived != 49900 {
			t.Errorf("payment after the return: %+v", got)
		}
		// The page now just says it is done.
		done := e.do("GET", path, "", nil)
		if done.status != 200 || strings.Contains(done.body, `value="fail"`) || !strings.Contains(done.body, "captured") {
			t.Errorf("finished page: %d %s", done.status, done.body)
		}
		// A second decision is refused.
		if again := e.do("POST", path, "", url.Values{"action": {"fail"}}); again.status != 409 {
			t.Errorf("deciding twice: %d", again.status)
		}
	})
}

func TestSimulatorFailureAndAuthorizeFlows(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		simEnv(t, e)
		path, ref, pa, payID := openSim(t, e)
		post := e.do("POST", path, "", url.Values{"action": {"fail"}})
		if post.status != 303 {
			t.Fatalf("fail: %d", post.status)
		}
		e.do("GET", "/return/"+pa+"/"+ref, "", nil)
		if got := e.payment(payID); got.Status == "succeeded" {
			t.Errorf("a failed attempt succeeded the payment: %+v", got)
		}
		atts := e.attempts(payID)
		if len(atts) != 1 || atts[0].Status != "failed" || atts[0].FailureCode != "simulated_failure" {
			t.Errorf("attempts %+v", atts)
		}

		// Authorize-only keeps the customer on the page, now offering capture.
		path, ref, pa, _ = openSim(t, e)
		post = e.do("POST", path, "", url.Values{"action": {"authorize"}})
		if post.status != 303 || post.header.Get("Location") != path {
			t.Fatalf("authorize should return to the page: %d %q", post.status, post.header.Get("Location"))
		}
		page := e.do("GET", path, "", nil)
		if !strings.Contains(page.body, `value="capture"`) || strings.Contains(page.body, `value="succeed"`) {
			t.Errorf("authorised page:\n%s", page.body)
		}
		if post = e.do("POST", path, "", url.Values{"action": {"capture"}}); post.header.Get("Location") != "/return/"+pa+"/"+ref {
			t.Errorf("capture: %d %q", post.status, post.header.Get("Location"))
		}

		// Pending then success: the customer is sent back at once, the success comes later.
		path, ref, pa, _ = openSim(t, e)
		post = e.do("POST", path, "", url.Values{"action": {"pending_succeed"}, "delay": {"3"}})
		if post.status != 303 || post.header.Get("Location") != "/return/"+pa+"/"+ref {
			t.Errorf("pending: %d %q", post.status, post.header.Get("Location"))
		}
		if wait := e.do("GET", path, "", nil); !strings.Contains(wait.body, "pending") {
			t.Errorf("waiting page:\n%s", wait.body)
		}
		// Bad input.
		path, _, _, _ = openSim(t, e)
		for _, form := range []url.Values{{"action": {"explode"}}, {}, {"action": {"pending_succeed"}, "delay": {"-4"}}, {"action": {"pending_succeed"}, "delay": {"999999"}}} {
			if bad := e.do("POST", path, "", form); bad.status != 400 {
				t.Errorf("%v: %d", form, bad.status)
			}
		}
	})
}

func TestSimulatorPageIsNotForOtherConnectorsOrUnknownRefs(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		simEnv(t, e)
		for _, path := range []string{"/sim/nosuchref?pa=" + e.pa, "/sim/nosuchref?pa=pa_unknown", "/sim/nosuchref", "/sim/x?pa=" + e.pa} {
			if r := e.do("GET", path, "", nil); r.status != 404 {
				t.Errorf("GET %s: %d", path, r.status)
			}
			if r := e.do("POST", path, "", url.Values{"action": {"succeed"}}); r.status != 404 {
				t.Errorf("POST %s: %d", path, r.status)
			}
		}
		// A reference of another connector is not the simulator's to decide.
		if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "routing", json.RawMessage(
			`{"rules":[],"catch_all":{"name":"all","when":{},"action":{"kind":"route","targets":[{"connector":"`+e.fake.Name+`"}]}}}`)); err != nil {
			t.Fatal(err)
		}
		c := e.create(core.CreatePaymentInput{})
		e.do("GET", c.path(), "", nil)
		atts := e.attempts(c.id)
		if len(atts) != 1 {
			t.Fatalf("attempts %+v", atts)
		}
		path := "/sim/" + atts[0].ProviderRef + "?pa=" + e.pa
		if r := e.do("GET", path, "", nil); r.status != 404 {
			t.Errorf("GET another connector's ref: %d", r.status)
		}
		if r := e.do("POST", path, "", url.Values{"action": {"succeed"}}); r.status != 404 {
			t.Errorf("POST another connector's ref: %d", r.status)
		}
	})
}

func (e *env) attempts(paymentID string) []store.Attempt {
	e.t.Helper()
	var out []store.Attempt
	err := e.db.Read(ctx, func(q store.Querier) (err error) { out, err = store.ListAttempts(ctx, q, paymentID); return })
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}
