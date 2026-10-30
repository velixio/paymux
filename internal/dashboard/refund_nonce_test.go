package dashboard

import (
	"net/url"
	"regexp"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var nonceRE = regexp.MustCompile(`name="nonce" value="([^"]+)"`)

func nonceOf(t *testing.T, body string) string {
	t.Helper()
	m := nonceRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no refund nonce in page:\n%s", body)
	}
	return m[1]
}

// H-14. The refund form carries a single-use nonce that becomes the refund's
// idempotency key: a double click, a resent POST or a back-button resubmit
// creates one refund, not two.
func TestDashboardRefundFormIsIdempotent(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		f := e.detailFixture()
		op := e.roleClient("operator")
		page := "/dashboard/payments/" + f.pay.ID
		path := page + "/refund"
		count := func() int {
			var n int
			_ = db.Read(ctx, func(q store.Querier) error {
				return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM refunds WHERE payment_id = ? AND id <> 'ref_old'", f.pay.ID).Scan(&n)
			})
			return n
		}
		body := e.pageOK(op, page)
		tok, nonce := csrfOf(t, body), nonceOf(t, body)
		form := func(amount, nonce string) url.Values {
			return url.Values{"amount": {amount}, "attempt_id": {"att_ok"}, "csrf": {tok}, "nonce": {nonce}}
		}

		for i := 0; i < 3; i++ { // the same submission, again and again
			if r, b := e.post(op, path, form("10.00", nonce)); r.StatusCode != 303 || r.Header.Get("Location") != page {
				t.Fatalf("submit %d: %d %s", i, r.StatusCode, b)
			}
		}
		if n := count(); n != 1 {
			t.Fatalf("one nonce made %d refunds", n)
		}
		var key string
		_ = db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT COALESCE(idempotency_key, '') FROM refunds WHERE payment_id = ? AND id <> 'ref_old'", f.pay.ID).Scan(&key)
		})
		if key == "" {
			t.Error("the refund does not carry the form's key")
		}
		// The same nonce for other values is not the same submission.
		if r, _ := e.post(op, path, form("20.00", nonce)); r.StatusCode != 400 {
			t.Errorf("same nonce, another amount: %d", r.StatusCode)
		}
		// A new page is a new form.
		nonce2 := nonceOf(t, e.pageOK(op, page))
		if nonce2 == nonce {
			t.Error("two renderings share a nonce")
		}
		if r, _ := e.post(op, path, form("20.00", nonce2)); r.StatusCode != 303 {
			t.Errorf("a fresh form: %d", r.StatusCode)
		}
		if n := count(); n != 2 {
			t.Errorf("%d refunds", n)
		}
		// A form without a nonce, or with one that was not issued for this payment
		// and session, is refused before anything is created.
		for name, n := range map[string]string{"missing": "", "forged": "AAAA.BBBB", "tampered": nonce2 + "x"} {
			if r, _ := e.post(op, path, form("1.00", n)); r.StatusCode != 400 {
				t.Errorf("%s nonce: %d", name, r.StatusCode)
			}
		}
		if n := count(); n != 2 {
			t.Errorf("a refused form created a refund: %d", n)
		}
	})
}
