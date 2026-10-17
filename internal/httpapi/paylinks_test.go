package httpapi_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/httpapi"
	"github.com/paymux/paymux/internal/paylinks"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/server"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func newLinkFixture(t *testing.T, db *store.DB, withLinks bool) *fixture {
	t.Helper()
	set := settings.New(db)
	if _, err := core.Bootstrap(ctx, db, set); err != nil {
		t.Fatal(err)
	}
	svc := core.NewService(core.Options{DB: db, Settings: set})
	f := &fixture{t: t, db: db, svc: svc}
	var err error
	sc := core.OrgScope{OrgID: store.DefaultOrgID}
	if f.app, err = svc.CreateApp(ctx, audit.System, sc, core.CreateAppInput{Name: "One", Slug: "one"}); err != nil {
		t.Fatal(err)
	}
	other, err := svc.CreateApp(ctx, audit.System, sc, core.CreateAppInput{Name: "Two", Slug: "two"})
	if err != nil {
		t.Fatal(err)
	}
	f.liveKey, _, _ = svc.CreateAPIKey(ctx, audit.System, sc, f.app.ID, "live")
	f.testKey, _, _ = svc.CreateAPIKey(ctx, audit.System, sc, f.app.ID, "test")
	f.otherKey, _, _ = svc.CreateAPIKey(ctx, audit.System, sc, other.ID, "test")
	box, _ := secrets.NewBox(bytes.Repeat([]byte{7}, 32))
	base, _ := url.Parse("https://pay.example.com")
	opts := httpapi.Options{Core: svc, DB: db, Box: box, BaseURL: base}
	if withLinks {
		opts.PaymentLinks = paylinks.New(paylinks.Options{DB: db, Core: svc, BaseURL: "https://pay.example.com",
			Emitter: events.EmitterFunc(func(context.Context, store.Tx, events.Event) error { return nil })})
	}
	api, err := httpapi.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	s := server.New(server.Options{})
	api.Register(s)
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)
	return f
}

func TestPaymentLinkAPI(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		f := newLinkFixture(t, db, true)
		expires := time.Now().UTC().AddDate(1, 0, 0).Truncate(time.Second).Format(time.RFC3339)
		body := map[string]any{"amount": 49900, "currency": "INR", "description": "Course fee", "single_use": true,
			"metadata": map[string]string{"sku": "c1"}, "expires_at": expires, "return_url": "https://shop.example.com/thanks"}
		r := f.do("POST", "/v1/payment_links", f.testKey, body, "Idempotency-Key", "k1")
		if r.status != 201 {
			t.Fatalf("create: %d %s", r.status, r.raw)
		}
		l := r.json(t)
		id := l["id"].(string)
		if l["object"] != "payment_link" || !strings.HasPrefix(id, "plink_") || l["amount"] != float64(49900) || l["currency"] != "INR" ||
			l["single_use"] != true || l["active"] != true || l["expires_at"] != expires || l["metadata"].(map[string]any)["sku"] != "c1" ||
			l["url"] != "https://pay.example.com/l/"+l["slug"].(string) || l["return_url"] != "https://shop.example.com/thanks" {
			t.Fatalf("link = %v", l)
		}
		// Idempotent replay: the same link, not another.
		if again := f.do("POST", "/v1/payment_links", f.testKey, body, "Idempotency-Key", "k1"); again.status != 201 || again.json(t)["id"] != id {
			t.Errorf("replay: %d %s", again.status, again.raw)
		}
		if n := f.count(`SELECT COUNT(*) FROM payment_links`); n != 1 {
			t.Errorf("%d links", n)
		}

		if g := f.do("GET", "/v1/payment_links/"+id, f.testKey, nil); g.status != 200 || g.json(t)["id"] != id {
			t.Errorf("get: %d %s", g.status, g.raw)
		}
		// Keys see only their own app and mode.
		if g := f.do("GET", "/v1/payment_links/"+id, f.liveKey, nil); g.status != 404 {
			t.Errorf("live key sees a test link: %d", g.status)
		}
		if g := f.do("GET", "/v1/payment_links/"+id, f.otherKey, nil); g.status != 404 {
			t.Errorf("another app's key sees the link: %d", g.status)
		}
		if g := f.do("POST", "/v1/payment_links/"+id+"/deactivate", f.otherKey, nil); g.status != 404 {
			t.Errorf("another app's key deactivates the link: %d", g.status)
		}
		if g := f.do("GET", "/v1/payment_links/"+id, "", nil); g.status != 401 {
			t.Errorf("no key: %d", g.status)
		}

		// A customer-chooses link, then listing.
		r2 := f.do("POST", "/v1/payment_links", f.testKey, map[string]any{"currency": "INR", "min_amount": 10000, "max_amount": 500000}, "Idempotency-Key", "k2")
		if r2.status != 201 || r2.json(t)["amount"] != nil || r2.json(t)["min_amount"] != float64(10000) {
			t.Fatalf("chooser: %d %s", r2.status, r2.raw)
		}
		id2 := r2.json(t)["id"].(string)
		list := f.do("GET", "/v1/payment_links?limit=1", f.testKey, nil)
		lj := list.json(t)
		data := lj["data"].([]any)
		if list.status != 200 || lj["object"] != "list" || len(data) != 1 || data[0].(map[string]any)["id"] != id2 || lj["has_more"] != true || lj["next_cursor"] != id2 {
			t.Fatalf("page 1: %d %s", list.status, list.raw)
		}
		list = f.do("GET", "/v1/payment_links?limit=1&starting_after="+id2, f.testKey, nil)
		lj = list.json(t)
		if data = lj["data"].([]any); len(data) != 1 || data[0].(map[string]any)["id"] != id || lj["has_more"] != false {
			t.Fatalf("page 2: %s", list.raw)
		}
		if l := f.do("GET", "/v1/payment_links", f.liveKey, nil); len(l.json(t)["data"].([]any)) != 0 {
			t.Errorf("live key lists test links: %s", l.raw)
		}

		// Deactivate, twice.
		for i := 0; i < 2; i++ {
			d := f.do("POST", "/v1/payment_links/"+id+"/deactivate", f.testKey, nil, "Idempotency-Key", "d"+string(rune('0'+i)))
			dj := d.json(t)
			if d.status != 200 || dj["active"] != false || dj["deactivated_reason"] != "manual" {
				t.Fatalf("deactivate %d: %d %s", i, d.status, d.raw)
			}
		}
		on := f.do("GET", "/v1/payment_links?active=true", f.testKey, nil)
		if data = on.json(t)["data"].([]any); len(data) != 1 || data[0].(map[string]any)["id"] != id2 {
			t.Errorf("active=true: %s", on.raw)
		}
	})
}

func TestPaymentLinkAPIValidation(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		f := newLinkFixture(t, db, true)
		cases := map[string]any{
			"no amount":        map[string]any{"currency": "INR"},
			"amount and min":   map[string]any{"currency": "INR", "amount": 100, "min_amount": 50},
			"no currency":      map[string]any{"amount": 100},
			"bad currency":     map[string]any{"currency": "ZZZ", "amount": 100},
			"past expiry":      map[string]any{"currency": "INR", "amount": 100, "expires_at": "2001-01-01T00:00:00Z"},
			"malformed expiry": map[string]any{"currency": "INR", "amount": 100, "expires_at": "tomorrow"},
			"string amount":    map[string]any{"currency": "INR", "amount": "100"},
			"unknown field":    map[string]any{"currency": "INR", "amount": 100, "colour": "red"},
			"bad return url":   map[string]any{"currency": "INR", "amount": 100, "return_url": "ftp://x"},
			"reserved meta":    map[string]any{"currency": "INR", "amount": 100, "metadata": map[string]string{"payment_link_id": "x"}},
		}
		for name, body := range cases {
			r := f.do("POST", "/v1/payment_links", f.testKey, body)
			if r.status != 400 || r.json(t)["error"].(map[string]any)["type"] != "invalid_request" {
				t.Errorf("%s: %d %s", name, r.status, r.raw)
			}
		}
		if r := f.do("POST", "/v1/payment_links", f.testKey, nil); r.status != 400 {
			t.Errorf("empty body: %d", r.status)
		}
		for _, q := range []string{"limit=0", "limit=101", "limit=x", "active=maybe"} {
			if r := f.do("GET", "/v1/payment_links?"+q, f.testKey, nil); r.status != 400 {
				t.Errorf("?%s: %d", q, r.status)
			}
		}
		if f.count(`SELECT COUNT(*) FROM payment_links`) != 0 {
			t.Error("rejected requests created links")
		}
	})
}

func TestPaymentLinkAPIIsOffWithoutTheService(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		f := newLinkFixture(t, db, false)
		for _, c := range [][2]string{{"POST", "/v1/payment_links"}, {"GET", "/v1/payment_links"}, {"GET", "/v1/payment_links/plink_x"}, {"POST", "/v1/payment_links/plink_x/deactivate"}} {
			var body any
			if c[0] == "POST" {
				body = map[string]any{"currency": "INR", "amount": 100}
			}
			if r := f.do(c[0], c[1], f.testKey, body); r.status != 404 {
				t.Errorf("%s %s: %d", c[0], c[1], r.status)
			}
		}
	})
}
