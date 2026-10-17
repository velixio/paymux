package paylinks_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/paylinks"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

var ctx = context.Background()

const baseURL = "https://pay.example.com"

type env struct {
	t      *testing.T
	db     *store.DB
	set    *settings.Service
	core   *core.Service
	links  *paylinks.Service
	mu     sync.Mutex
	events []events.Event
	now    time.Time
	scope  core.Scope
}

func (e *env) clock() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func (e *env) eventTypes() (out []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		out = append(out, ev.Type)
	}
	return
}

func (e *env) eventsOf(typ string) (out []events.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return
}

func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	e := &env{t: t, db: db, set: settings.New(db), now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	emitter := events.EmitterFunc(func(_ context.Context, _ store.Tx, ev events.Event) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.events = append(e.events, ev)
		return nil
	})
	if _, err := core.Bootstrap(ctx, db, e.set); err != nil {
		t.Fatal(err)
	}
	err := db.Write(ctx, func(tx store.Tx) error {
		for _, a := range []store.App{
			{ID: "app_1", OrgID: store.DefaultOrgID, Name: "Voice Studio", Slug: "vs", DefaultReturnURL: "https://app.example.com/back", CreatedAt: e.now},
			{ID: "app_2", OrgID: store.DefaultOrgID, Name: "Other", Slug: "other", CreatedAt: e.now},
		} {
			if err := store.InsertApp(ctx, tx, a); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.core = core.NewService(core.Options{DB: db, Settings: e.set, Emitter: emitter, Now: e.clock})
	e.links = paylinks.New(paylinks.Options{DB: db, Core: e.core, Emitter: emitter, BaseURL: baseURL, Now: e.clock})
	e.scope = core.Scope{AppID: "app_1", Mode: "test"}
	return e
}

var apiActor = audit.Actor{Type: audit.ActorAPIKey, ID: "key_1"}

func (e *env) create(mut func(*paylinks.CreateInput)) store.PaymentLink {
	e.t.Helper()
	in := paylinks.CreateInput{Scope: e.scope, Amount: 49900, Currency: "INR", Description: "Course fee"}
	if mut != nil {
		mut(&in)
	}
	l, err := e.links.Create(ctx, apiActor, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

func TestCreateFixedLinkStoresEmitsAndAudits(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		exp := e.now.Add(72 * time.Hour)
		l := e.create(func(in *paylinks.CreateInput) {
			in.Metadata, in.ExpiresAt, in.ReturnURL = map[string]string{"sku": "c1"}, exp, "https://shop.example.com/thanks"
		})
		if !strings.HasPrefix(l.ID, "plink_") || len(l.Slug) != 12 || strings.Trim(l.Slug, "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
			t.Fatalf("id %q slug %q", l.ID, l.Slug)
		}
		if l.Amount != 49900 || l.Currency != "INR" || !l.Active || l.SingleUse || l.Mode != "test" || l.AppID != "app_1" || l.OrgID != store.DefaultOrgID ||
			!l.ExpiresAt.Equal(exp) || l.Metadata["sku"] != "c1" {
			t.Fatalf("link %+v", l)
		}
		if got, want := e.links.URL(l), baseURL+"/l/"+l.Slug; got != want {
			t.Errorf("url %q, want %q", got, want)
		}
		created := e.eventsOf("payment_link.created")
		if len(created) != 1 || created[0].ObjectType != "payment_link" || created[0].ObjectID != l.ID || created[0].AppID != "app_1" || created[0].Mode != "test" {
			t.Fatalf("events: %v", e.eventTypes())
		}
		raw, _ := json.Marshal(created[0].Data)
		var data map[string]any
		_ = json.Unmarshal(raw, &data)
		if data["id"] != l.ID || data["object"] != "payment_link" || data["url"] != baseURL+"/l/"+l.Slug || data["amount"] != float64(49900) || data["active"] != true {
			t.Errorf("event data %s", raw)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "payment_link.create", 10)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "key_1" || aud[0].Target != l.ID {
			t.Errorf("audit %+v", aud)
		}
	})
}

func TestCreateCustomerChoosesLink(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		l := e.create(func(in *paylinks.CreateInput) { in.Amount, in.MinAmount, in.MaxAmount = 0, 10000, 5000000 })
		if l.Amount != 0 || l.MinAmount != 10000 || l.MaxAmount != 5000000 {
			t.Fatalf("%+v", l)
		}
		raw, _ := json.Marshal(paylinks.ViewLink(l, e.links.URL(l)))
		var v map[string]any
		_ = json.Unmarshal(raw, &v)
		if v["amount"] != nil || v["min_amount"] != float64(10000) || v["max_amount"] != float64(5000000) {
			t.Errorf("view %s: a chooser link has no fixed amount", raw)
		}
	})
}

func TestCreateValidation(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		bigMeta := map[string]string{}
		for i := 0; i < 20; i++ {
			bigMeta[strings.Repeat("k", i+1)] = "v"
		}
		cases := map[string]func(*paylinks.CreateInput){
			"no amount":                func(in *paylinks.CreateInput) { in.Amount = 0 },
			"negative amount":          func(in *paylinks.CreateInput) { in.Amount = -5 },
			"fixed with min":           func(in *paylinks.CreateInput) { in.MinAmount = 100 },
			"fixed with max":           func(in *paylinks.CreateInput) { in.MaxAmount = 100 },
			"chooser without min":      func(in *paylinks.CreateInput) { in.Amount, in.MaxAmount = 0, 100 },
			"max below min":            func(in *paylinks.CreateInput) { in.Amount, in.MinAmount, in.MaxAmount = 0, 500, 100 },
			"unknown currency":         func(in *paylinks.CreateInput) { in.Currency = "XXX" },
			"no currency":              func(in *paylinks.CreateInput) { in.Currency = "" },
			"expired already":          func(in *paylinks.CreateInput) { in.ExpiresAt = e.now.Add(-time.Minute) },
			"expiry beyond five years": func(in *paylinks.CreateInput) { in.ExpiresAt = e.now.AddDate(6, 0, 0) },
			"relative return url":      func(in *paylinks.CreateInput) { in.ReturnURL = "/thanks" },
			"javascript return url":    func(in *paylinks.CreateInput) { in.ReturnURL = "javascript:alert(1)" },
			"long description":         func(in *paylinks.CreateInput) { in.Description = strings.Repeat("d", 1001) },
			"too many metadata keys":   func(in *paylinks.CreateInput) { in.Metadata = bigMeta }, // 20 leaves no room for payment_link_id
			"reserved metadata key":    func(in *paylinks.CreateInput) { in.Metadata = map[string]string{"payment_link_id": "x"} },
			"long metadata value":      func(in *paylinks.CreateInput) { in.Metadata = map[string]string{"k": strings.Repeat("v", 501)} },
			"no app":                   func(in *paylinks.CreateInput) { in.Scope.AppID = "" },
			"no mode":                  func(in *paylinks.CreateInput) { in.Scope.Mode = "" },
		}
		for name, mut := range cases {
			in := paylinks.CreateInput{Scope: e.scope, Amount: 49900, Currency: "INR"}
			mut(&in)
			if _, err := e.links.Create(ctx, apiActor, in); !errors.Is(err, paylinks.ErrInvalid) {
				t.Errorf("%s: err = %v, want ErrInvalid", name, err)
			}
		}
		in := paylinks.CreateInput{Scope: core.Scope{AppID: "app_missing", Mode: "test"}, Amount: 100, Currency: "INR"}
		if _, err := e.links.Create(ctx, apiActor, in); !errors.Is(err, paylinks.ErrNotFound) {
			t.Errorf("unknown app: %v", err)
		}
		in = paylinks.CreateInput{Scope: e.scope, OrgID: "org_other", Amount: 100, Currency: "INR"}
		if _, err := e.links.Create(ctx, apiActor, in); !errors.Is(err, paylinks.ErrNotFound) {
			t.Errorf("app of another org: %v", err)
		}
		if n := len(e.eventsOf("payment_link.created")); n != 0 {
			t.Errorf("%d events for rejected links", n)
		}
	})
}

func TestGetAndListAreScopedToAppAndMode(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		a := e.create(nil)
		b := e.create(nil)
		c := e.create(func(in *paylinks.CreateInput) { in.Scope.Mode = "live" })
		d := e.create(func(in *paylinks.CreateInput) { in.Scope.AppID = "app_2" })
		if _, err := e.links.Deactivate(ctx, apiActor, e.scope, b.ID); err != nil {
			t.Fatal(err)
		}

		if got, err := e.links.Get(ctx, e.scope, a.ID); err != nil || got.ID != a.ID {
			t.Fatalf("get: %+v %v", got, err)
		}
		for _, other := range []store.PaymentLink{c, d} {
			if _, err := e.links.Get(ctx, e.scope, other.ID); !errors.Is(err, paylinks.ErrNotFound) {
				t.Errorf("link %s of another app or mode must be invisible: %v", other.ID, err)
			}
		}
		ids := func(ls []store.PaymentLink) (out []string) {
			for _, l := range ls {
				out = append(out, l.ID)
			}
			return
		}
		ls, err := e.links.List(ctx, e.scope, paylinks.ListParams{})
		if err != nil || strings.Join(ids(ls), ",") != b.ID+","+a.ID {
			t.Fatalf("list: %v %v", ids(ls), err)
		}
		on := true
		ls, _ = e.links.List(ctx, e.scope, paylinks.ListParams{Active: &on})
		if strings.Join(ids(ls), ",") != a.ID {
			t.Errorf("active only: %v", ids(ls))
		}
		ls, _ = e.links.List(ctx, e.scope, paylinks.ListParams{Limit: 1, StartingAfter: b.ID})
		if strings.Join(ids(ls), ",") != a.ID {
			t.Errorf("cursor: %v", ids(ls))
		}
		// The dashboard asks per org.
		all, err := e.links.ListOrg(ctx, store.DefaultOrgID, paylinks.OrgListParams{})
		if err != nil || len(all) != 4 {
			t.Fatalf("org list: %d %v", len(all), err)
		}
		if got, err := e.links.ListOrg(ctx, "org_other", paylinks.OrgListParams{}); err != nil || len(got) != 0 {
			t.Errorf("another org's list: %d %v", len(got), err)
		}
		if _, err := e.links.GetInOrg(ctx, "org_other", a.ID); !errors.Is(err, paylinks.ErrNotFound) {
			t.Errorf("another org's get: %v", err)
		}
	})
}

func TestDeactivateIsIdempotentAndEmitsOnce(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		l := e.create(nil)
		got, err := e.links.Deactivate(ctx, apiActor, e.scope, l.ID)
		if err != nil || got.Active || got.DeactivatedReason != "manual" {
			t.Fatalf("deactivate: %+v %v", got, err)
		}
		again, err := e.links.Deactivate(ctx, apiActor, e.scope, l.ID)
		if err != nil || again.Active {
			t.Fatalf("second deactivate: %+v %v", again, err)
		}
		ev := e.eventsOf("payment_link.deactivated")
		if len(ev) != 1 || ev[0].ObjectID != l.ID {
			t.Fatalf("events %v", e.eventTypes())
		}
		raw, _ := json.Marshal(ev[0].Data)
		if !strings.Contains(string(raw), `"active":false`) || !strings.Contains(string(raw), `"deactivated_reason":"manual"`) {
			t.Errorf("event data %s", raw)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "payment_link.deactivate", 10)
			return
		})
		if len(aud) != 1 {
			t.Errorf("audit rows %d, want 1", len(aud))
		}
		if _, err := e.links.Deactivate(ctx, apiActor, core.Scope{AppID: "app_2", Mode: "test"}, l.ID); !errors.Is(err, paylinks.ErrNotFound) {
			t.Errorf("another app's key deactivating the link: %v", err)
		}
		if _, err := e.links.Deactivate(ctx, apiActor, e.scope, "plink_missing"); !errors.Is(err, paylinks.ErrNotFound) {
			t.Errorf("unknown link: %v", err)
		}
	})
}

func TestStartCreatesANormalPaymentCarryingTheLinkId(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		l := e.create(func(in *paylinks.CreateInput) {
			in.Metadata, in.ReturnURL = map[string]string{"sku": "c1"}, "https://shop.example.com/thanks"
		})
		st, err := e.links.Start(ctx, l.Slug, paylinks.StartInput{Name: " Asha K ", Email: "Asha@Example.com", Phone: "+91 98765 43210", Holder: "h1"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(st.PaymentID, "pay_") || st.ClientSecret == "" {
			t.Fatalf("started %+v", st)
		}
		p, err := e.core.GetPaymentByID(ctx, st.PaymentID)
		if err != nil {
			t.Fatal(err)
		}
		if p.Amount != 49900 || p.Currency != "INR" || p.AppID != "app_1" || p.Mode != "test" || p.Status != "created" || p.Description != "Course fee" ||
			p.Metadata["payment_link_id"] != l.ID || p.Metadata["sku"] != "c1" || p.OrderRef != l.ID || p.ReturnURL != "https://shop.example.com/thanks" {
			t.Fatalf("payment %+v", p)
		}
		if !core.ClientSecretMatches(p, st.ClientSecret) {
			t.Error("the secret returned must open the payment")
		}
		var c store.Customer
		_ = db.Read(ctx, func(q store.Querier) (err error) { c, err = store.GetCustomer(ctx, q, p.CustomerID); return })
		if c.Name != "Asha K" || c.Email != "asha@example.com" || c.Phone != "+91 98765 43210" {
			t.Errorf("customer %+v", c)
		}
		// The link's own metadata is not changed by payments.
		got, _ := e.links.Get(ctx, e.scope, l.ID)
		if _, leaked := got.Metadata["payment_link_id"]; leaked || !got.Active {
			t.Errorf("link %+v", got)
		}
		// A reusable link serves many payments.
		st2, err := e.links.Start(ctx, l.Slug, paylinks.StartInput{Name: "Ravi", Email: "ravi@example.com", Holder: "h2"})
		if err != nil || st2.PaymentID == st.PaymentID {
			t.Fatalf("second payment: %+v %v", st2, err)
		}
	})
}

func TestStartAmounts(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		fixed := e.create(nil)
		choose := e.create(func(in *paylinks.CreateInput) { in.Amount, in.MinAmount, in.MaxAmount = 0, 10000, 500000 })
		openEnded := e.create(func(in *paylinks.CreateInput) { in.Amount, in.MinAmount = 0, 100 })
		who := paylinks.StartInput{Name: "Asha", Email: "asha@example.com"}
		amountOf := func(slug string, amount int64) (int64, error) {
			in := who
			in.Amount = amount
			st, err := e.links.Start(ctx, slug, in)
			if err != nil {
				return 0, err
			}
			p, _ := e.core.GetPaymentByID(ctx, st.PaymentID)
			return p.Amount, nil
		}
		if a, err := amountOf(fixed.Slug, 0); err != nil || a != 49900 {
			t.Errorf("fixed: %d %v", a, err)
		}
		// A fixed price cannot be changed by the visitor, whatever they send.
		if a, err := amountOf(fixed.Slug, 100); err != nil || a != 49900 {
			t.Errorf("fixed price overridden: %d %v", a, err)
		}
		for amount, ok := range map[int64]bool{9999: false, 10000: true, 250000: true, 500000: true, 500001: false, 0: false, -1: false} {
			a, err := amountOf(choose.Slug, amount)
			if ok && (err != nil || a != amount) {
				t.Errorf("chooser %d: %d %v", amount, a, err)
			}
			if !ok && !errors.Is(err, paylinks.ErrInvalid) {
				t.Errorf("chooser %d: err = %v, want ErrInvalid", amount, err)
			}
		}
		if _, err := amountOf(openEnded.Slug, 99); !errors.Is(err, paylinks.ErrInvalid) {
			t.Errorf("below min: %v", err)
		}
		if a, err := amountOf(openEnded.Slug, 1_000_000_00); err != nil || a != 1_000_000_00 {
			t.Errorf("no max: %d %v", a, err)
		}
	})
}

func TestStartValidatesWhoIsPaying(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		l := e.create(nil)
		bad := map[string]paylinks.StartInput{
			"no name":       {Email: "a@example.com"},
			"blank name":    {Name: "   ", Email: "a@example.com"},
			"long name":     {Name: strings.Repeat("n", 201), Email: "a@example.com"},
			"no email":      {Name: "A"},
			"bad email":     {Name: "A", Email: "not-an-email"},
			"email spaces":  {Name: "A", Email: "a b@example.com"},
			"email newline": {Name: "A", Email: "a@example.com\r\nBcc: x@example.com"},
			"letters phone": {Name: "A", Email: "a@example.com", Phone: "call me"},
			"long phone":    {Name: "A", Email: "a@example.com", Phone: strings.Repeat("1", 25)},
		}
		for name, in := range bad {
			if _, err := e.links.Start(ctx, l.Slug, in); !errors.Is(err, paylinks.ErrInvalid) {
				t.Errorf("%s: err = %v, want ErrInvalid", name, err)
			}
		}
		var n int
		_ = db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM payments`).Scan(&n) })
		if n != 0 {
			t.Errorf("%d payments created by rejected submissions", n)
		}
		if _, err := e.links.Start(ctx, l.Slug, paylinks.StartInput{Name: "A", Email: "a@example.com"}); err != nil {
			t.Errorf("phone is optional: %v", err)
		}
	})
}

func TestStartRefusesInactiveExpiredAndUnknownLinks(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		off := e.create(nil)
		timed := e.create(func(in *paylinks.CreateInput) { in.ExpiresAt = e.now.Add(time.Hour) })
		if _, err := e.links.Deactivate(ctx, apiActor, e.scope, off.ID); err != nil {
			t.Fatal(err)
		}
		who := paylinks.StartInput{Name: "A", Email: "a@example.com"}
		if _, err := e.links.Start(ctx, off.Slug, who); !errors.Is(err, paylinks.ErrInactive) {
			t.Errorf("deactivated: %v", err)
		}
		if _, err := e.links.Start(ctx, "nosuchslug12", who); !errors.Is(err, paylinks.ErrNotFound) {
			t.Errorf("unknown: %v", err)
		}
		if _, err := e.links.Start(ctx, timed.Slug, who); err != nil {
			t.Errorf("before expiry: %v", err)
		}
		e.advance(61 * time.Minute)
		if _, err := e.links.Start(ctx, timed.Slug, who); !errors.Is(err, paylinks.ErrExpired) {
			t.Errorf("expired: %v", err)
		}
		// Open reports the same states for the page that shows the form.
		if _, err := e.links.Open(ctx, timed.Slug); !errors.Is(err, paylinks.ErrExpired) {
			t.Errorf("Open expired: %v", err)
		}
		if _, err := e.links.Open(ctx, off.Slug); !errors.Is(err, paylinks.ErrInactive) {
			t.Errorf("Open inactive: %v", err)
		}
		if l, err := e.links.Open(ctx, e.create(nil).Slug); err != nil || !l.Active {
			t.Errorf("Open active: %+v %v", l, err)
		}
	})
}

func TestSingleUseLinkIsHeldByOneVisitorThenSpentByTheFirstSuccess(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		l := e.create(func(in *paylinks.CreateInput) { in.SingleUse = true })
		who := func(holder string) paylinks.StartInput {
			return paylinks.StartInput{Name: "A", Email: "a@example.com", Holder: holder}
		}
		first, err := e.links.Start(ctx, l.Slug, who("visitor-a"))
		if err != nil {
			t.Fatal(err)
		}
		// Somebody else is turned away while the first payment is open...
		if _, err := e.links.Start(ctx, l.Slug, who("visitor-b")); !errors.Is(err, paylinks.ErrBusy) {
			t.Fatalf("second visitor: %v, want ErrBusy", err)
		}
		// ...but the first visitor can come back (back button, reload).
		if _, err := e.links.Start(ctx, l.Slug, who("visitor-a")); err != nil {
			t.Fatalf("same visitor: %v", err)
		}
		// A visitor without an identity cannot slip past a held link.
		if _, err := e.links.Start(ctx, l.Slug, who("")); !errors.Is(err, paylinks.ErrBusy) {
			t.Fatalf("anonymous visitor: %v", err)
		}
		// The hold lapses with the payment's lifetime (payment_ttl, 60 minutes).
		e.advance(61 * time.Minute)
		if _, err := e.links.Start(ctx, l.Slug, who("visitor-b")); err != nil {
			t.Fatalf("after the hold lapsed: %v", err)
		}

		// The first successful payment spends the link, once.
		p, _ := e.core.GetPaymentByID(ctx, first.PaymentID)
		for i := 0; i < 2; i++ {
			if err := db.Write(ctx, func(tx store.Tx) error { return e.links.OnPaymentSucceeded(ctx, tx, p, store.Attempt{}) }); err != nil {
				t.Fatal(err)
			}
		}
		got, _ := e.links.Get(ctx, e.scope, l.ID)
		if got.Active || got.DeactivatedReason != "used" {
			t.Fatalf("link after payment: %+v", got)
		}
		ev := e.eventsOf("payment_link.deactivated")
		if len(ev) != 1 {
			t.Fatalf("events %v: want exactly one deactivation", e.eventTypes())
		}
		raw, _ := json.Marshal(ev[0].Data)
		if !strings.Contains(string(raw), `"deactivated_reason":"used"`) || !strings.Contains(string(raw), first.PaymentID) {
			t.Errorf("event data %s should name the reason and the payment", raw)
		}
		if _, err := e.links.Start(ctx, l.Slug, who("visitor-c")); !errors.Is(err, paylinks.ErrInactive) {
			t.Errorf("spent link: %v", err)
		}
	})
}

func TestReusableLinksSurviveSuccessfulPayments(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		l := e.create(nil)
		st, err := e.links.Start(ctx, l.Slug, paylinks.StartInput{Name: "A", Email: "a@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		p, _ := e.core.GetPaymentByID(ctx, st.PaymentID)
		if err := db.Write(ctx, func(tx store.Tx) error { return e.links.OnPaymentSucceeded(ctx, tx, p, store.Attempt{}) }); err != nil {
			t.Fatal(err)
		}
		if got, _ := e.links.Get(ctx, e.scope, l.ID); !got.Active {
			t.Error("a reusable link stays active")
		}
		// Payments that do not come from a link are none of its business.
		plain := store.Payment{ID: "pay_x", OrgID: store.DefaultOrgID, AppID: "app_1", Mode: "test", Metadata: map[string]string{"order": "1"}}
		if err := db.Write(ctx, func(tx store.Tx) error { return e.links.OnPaymentSucceeded(ctx, tx, plain, store.Attempt{}) }); err != nil {
			t.Errorf("plain payment: %v", err)
		}
		gone := store.Payment{ID: "pay_y", OrgID: store.DefaultOrgID, AppID: "app_1", Mode: "test", Metadata: map[string]string{"payment_link_id": "plink_missing"}}
		if err := db.Write(ctx, func(tx store.Tx) error { return e.links.OnPaymentSucceeded(ctx, tx, gone, store.Attempt{}) }); err != nil {
			t.Errorf("a payment naming a link that does not exist must not fail the payment: %v", err)
		}
	})
}

func TestStartIsRateLimitedPerLink(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		links := paylinks.New(paylinks.Options{DB: db, Core: e.core, BaseURL: baseURL, Now: e.clock, StartsPerMinute: 3,
			Emitter: events.EmitterFunc(func(context.Context, store.Tx, events.Event) error { return nil })})
		l, err := links.Create(ctx, apiActor, paylinks.CreateInput{Scope: e.scope, Amount: 100, Currency: "INR"})
		if err != nil {
			t.Fatal(err)
		}
		who := paylinks.StartInput{Name: "A", Email: "a@example.com"}
		for i := 0; i < 3; i++ {
			if _, err := links.Start(ctx, l.Slug, who); err != nil {
				t.Fatalf("start %d: %v", i, err)
			}
		}
		if _, err := links.Start(ctx, l.Slug, who); !errors.Is(err, paylinks.ErrRateLimited) {
			t.Fatalf("fourth start: %v, want ErrRateLimited", err)
		}
		e.advance(61 * time.Second)
		if _, err := links.Start(ctx, l.Slug, who); err != nil {
			t.Errorf("after a minute: %v", err)
		}
	})
}
