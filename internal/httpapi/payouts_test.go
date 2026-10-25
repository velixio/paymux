package httpapi_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/httpapi"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/payouts"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/pkg/connector"
)

type payoutFixture struct {
	*fixture
	q    *jobs.Queue
	fake *fakeconn.State
}

func newPayoutFixture(t *testing.T, db *store.DB, purposes string) *payoutFixture {
	t.Helper()
	fake := fakeconn.Register("pf")
	q := jobs.New(db, jobs.Options{Jitter: func() float64 { return 0 }})
	box, err := secrets.NewBox(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureOpts(t, db, nil, func(o *httpapi.Options) {
		svc := payouts.New(payouts.Options{DB: db, Box: box, Jobs: q, Connectors: payouts.ConnectorsFunc(func(ctx context.Context, id string) (connector.Connector, error) {
			factory, _, _ := connector.Lookup(fake.Name)
			return factory([]byte(fakeconn.ValidConfig), connector.Deps{})
		})})
		svc.RegisterJobs(q)
		o.Payouts = svc
	})
	err = db.Write(ctx, func(tx store.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE provider_accounts SET connector = ?, enabled_purposes = ?, accepted_currencies = '["INR"]' WHERE id = 'pa_1'`, fake.Name, purposes); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO app_provider_accounts (app_id, provider_account_id) VALUES (?, 'pa_1')`, f.app.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return &payoutFixture{fixture: f, q: q, fake: fake}
}

func (f *payoutFixture) run() {
	f.t.Helper()
	for i := 0; i < 20; i++ {
		n, err := f.q.RunOnce(ctx)
		if err != nil {
			f.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

var payoutBody = map[string]any{"amount": 250000, "currency": "INR", "narration": "October",
	"beneficiary": map[string]any{"name": "Asha Rao", "account_number": "123456789012", "ifsc": "HDFC0001234"}}

func TestPayoutsNeedTheExplicitScope(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		f := newPayoutFixture(t, db, `["payout"]`)
		sc := func(scopes ...string) string { return f.scopedKey("test", scopes...) }

		// A key made without a scope list has every scope except payouts.
		if r := f.do("POST", "/v1/payouts", f.testKey, payoutBody); r.status != 403 || r.errType(t) != "insufficient_scope" ||
			!strings.Contains(r.header.Get("WWW-Authenticate"), "payouts:write") {
			t.Fatalf("a full-access key paid out: %d %s", r.status, r.raw)
		}
		if r := f.do("GET", "/v1/payouts", f.testKey, nil); r.status != 403 {
			t.Errorf("a full-access key listed payouts: %d", r.status)
		}
		// So do keys with every other scope and keys that can only read payouts.
		other := sc(authz.ScopePaymentsWrite, authz.ScopeRefundsWrite, authz.ScopeSubscriptionsWrite, authz.ScopeInvoicesRead)
		if r := f.do("POST", "/v1/payouts", other, payoutBody); r.status != 403 {
			t.Errorf("a key without the payouts scope: %d", r.status)
		}
		reader := sc(authz.ScopePayoutsRead)
		if r := f.do("POST", "/v1/payouts", reader, payoutBody); r.status != 403 {
			t.Errorf("payouts:read created a payout: %d", r.status)
		}
		if f.count("SELECT COUNT(*) FROM payouts") != 0 {
			t.Fatal("a refused request created a payout")
		}

		writer := sc(authz.ScopePayoutsWrite)
		r := f.do("POST", "/v1/payouts", writer, payoutBody, "Idempotency-Key", "po-1")
		if r.status != 201 {
			t.Fatalf("create: %d %s", r.status, r.raw)
		}
		p := r.json(t)
		id := p["id"].(string)
		if p["object"] != "payout" || p["status"] != "pending" || p["amount"] != float64(250000) || p["currency"] != "INR" ||
			p["beneficiary"].(map[string]any)["label"] != "••••9012 HDFC" || !strings.HasPrefix(id, "po_") {
			t.Fatalf("payout = %v", p)
		}
		if strings.Contains(string(r.raw), "123456789012") {
			t.Errorf("the account number is in the response: %s", r.raw)
		}
		// The same Idempotency-Key does not pay twice.
		if again := f.do("POST", "/v1/payouts", writer, payoutBody, "Idempotency-Key", "po-1"); again.status != 201 || again.header.Get("Idempotent-Replay") != "true" || again.json(t)["id"] != id {
			t.Errorf("replay: %d %s", again.status, again.raw)
		}
		if f.count("SELECT COUNT(*) FROM payouts") != 1 {
			t.Fatal("a repeated request paid twice")
		}

		f.run()
		for _, key := range []string{writer, reader} { // payouts:write covers reading, as everywhere
			g := f.do("GET", "/v1/payouts/"+id, key, nil)
			if g.status != 200 || g.json(t)["status"] != "processing" {
				t.Errorf("get: %d %s", g.status, g.raw)
			}
		}
		l := f.do("GET", "/v1/payouts?limit=5", reader, nil)
		if l.status != 200 || len(l.json(t)["data"].([]any)) != 1 {
			t.Errorf("list: %d %s", l.status, l.raw)
		}
		if l := f.do("GET", "/v1/payouts?status=succeeded", reader, nil); len(l.json(t)["data"].([]any)) != 0 {
			t.Errorf("status filter: %s", l.raw)
		}
		if l := f.do("GET", "/v1/payouts?limit=0", reader, nil); l.status != 400 {
			t.Errorf("bad limit: %d", l.status)
		}
		// Another app's payouts-scoped key sees nothing of it.
		if g := f.do("GET", "/v1/payouts/"+id, f.otherKey, nil); g.status != 403 {
			t.Errorf("other app, no scope: %d", g.status)
		}
		if g := f.do("GET", "/v1/payouts/po_nonexistent", reader, nil); g.status != 404 || g.errType(t) != "not_found" {
			t.Errorf("missing: %d %s", g.status, g.raw)
		}
	})
}

func TestPayoutRequestErrors(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		f := newPayoutFixture(t, db, `["payout"]`)
		w := f.scopedKey("test", authz.ScopePayoutsWrite)
		for name, tc := range map[string]struct {
			body   any
			status int
			typ    string
		}{
			"no beneficiary":                       {map[string]any{"amount": 100, "currency": "INR"}, 400, "invalid_request"},
			"no amount":                            {map[string]any{"currency": "INR", "beneficiary": payoutBody["beneficiary"]}, 400, "invalid_request"},
			"bad ifsc":                             {map[string]any{"amount": 100, "currency": "INR", "beneficiary": map[string]any{"name": "A", "account_number": "123456789", "ifsc": "x"}}, 400, "invalid_request"},
			"unknown field":                        {map[string]any{"amount": 100, "currency": "INR", "beneficiary": payoutBody["beneficiary"], "bogus": 1}, 400, "invalid_request"},
			"not json":                             {"{", 400, "invalid_request"},
			"unsupported currency for the account": {map[string]any{"amount": 100, "currency": "USD", "beneficiary": payoutBody["beneficiary"]}, 422, "no_route"},
		} {
			if r := f.do("POST", "/v1/payouts", w, tc.body, "Idempotency-Key", "k-"+strings.ReplaceAll(name, " ", "-")); r.status != tc.status || r.errType(t) != tc.typ {
				t.Errorf("%s: %d %s", name, r.status, r.raw)
			}
		}
	})
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		// The account never opted in to payouts.
		f := newPayoutFixture(t, db, `["one_time"]`)
		w := f.scopedKey("test", authz.ScopePayoutsWrite)
		if r := f.do("POST", "/v1/payouts", w, payoutBody, "Idempotency-Key", "k-nopurpose"); r.status != 422 || r.errType(t) != "no_route" {
			t.Errorf("account without the payout purpose: %d %s", r.status, r.raw)
		}
	})
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		// Payouts not enabled at all.
		f := newFixture(t, db)
		w := f.scopedKey("test", authz.ScopePayoutsWrite)
		if r := f.do("POST", "/v1/payouts", w, payoutBody, "Idempotency-Key", "k-nosvc"); r.status != 404 {
			t.Errorf("without a payouts service: %d", r.status)
		}
	})
}

// Money leaves the account on a payout, and a client that retries after a
// timeout must not pay twice, so the key is not optional.
func TestPayoutsRequireAnIdempotencyKey(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		f := newPayoutFixture(t, db, `["payout"]`)
		w := f.scopedKey("test", authz.ScopePayoutsWrite)
		if r := f.do("POST", "/v1/payouts", w, payoutBody); r.status != 400 || r.errType(t) != "invalid_request" || !strings.Contains(string(r.raw), "Idempotency-Key") {
			t.Errorf("no key: %d %s", r.status, r.raw)
		}
		if r := f.do("POST", "/v1/payouts", w, payoutBody, "Idempotency-Key", ""); r.status != 400 {
			t.Errorf("empty key: %d %s", r.status, r.raw)
		}
		if r := f.do("POST", "/v1/payouts", w, payoutBody, "Idempotency-Key", "po-required-1"); r.status != 201 {
			t.Errorf("with a key: %d %s", r.status, r.raw)
		}
	})
}

func TestPayoutToABeneficiaryInReviewNeedsForce(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		f := newPayoutFixture(t, db, `["payout"]`)
		writer := f.scopedKey("test", authz.ScopePayoutsWrite)
		r := f.do("POST", "/v1/payouts", writer, payoutBody, "Idempotency-Key", "po-a")
		if r.status != 201 {
			t.Fatalf("create: %d %s", r.status, r.raw)
		}
		if err := db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE payouts SET status = 'needs_review'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if r := f.do("POST", "/v1/payouts", writer, payoutBody, "Idempotency-Key", "po-b"); r.status != 409 || r.errType(t) != "payout_needs_review" {
			t.Fatalf("re-send: %d %s", r.status, r.raw)
		}
		forced := map[string]any{"force": true}
		for k, v := range payoutBody {
			forced[k] = v
		}
		if r := f.do("POST", "/v1/payouts", writer, forced, "Idempotency-Key", "po-c"); r.status != 201 {
			t.Fatalf("forced: %d %s", r.status, r.raw)
		}
		if n := f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'payout.force_create'"); n != 1 {
			t.Errorf("force audit rows = %d", n)
		}
	})
}
