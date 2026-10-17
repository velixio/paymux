package connectors_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/inbound"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/pkg/connector"
)

var (
	ctx = context.Background()
	org = core.OrgScope{OrgID: store.DefaultOrgID}
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

type env struct {
	t    *testing.T
	db   *store.DB
	svc  *connectors.Service
	fake *fakeconn.State
	clk  *clock
	box  *secrets.Box
}

func newSvc(t *testing.T, db *store.DB, box *secrets.Box, clk *clock) *connectors.Service {
	base, _ := url.Parse("https://pay.example.com")
	return connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base, Now: clk.Now})
}

func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	box, err := secrets.NewBox(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	e := &env{t: t, db: db, fake: fakeconn.Register("fake"), clk: clk, box: box, svc: newSvc(t, db, box, clk)}
	err = db.Write(ctx, func(tx store.Tx) error {
		if err := store.SeedOrg(ctx, tx, "Paymux", clk.t); err != nil {
			return err
		}
		return store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: store.DefaultOrgID, Name: "A", Slug: "a", CreatedAt: clk.t})
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func each(t *testing.T, fn func(t *testing.T, e *env)) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) { fn(t, newEnv(t, db)) })
}

func (e *env) create(cfg string) store.ProviderAccount {
	e.t.Helper()
	pa, err := e.svc.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{
		Connector: e.fake.Name, Name: "main", Mode: "test", Config: json.RawMessage(cfg), AppID: "app_1"})
	if err != nil {
		e.t.Fatal(err)
	}
	return pa
}

func TestCreateAccountEncryptsAndDefaults(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		pa := e.create(fakeconn.ValidConfig)
		if !strings.HasPrefix(pa.ID, "pa_") || pa.Status != "active" || pa.Mode != "test" || pa.OrgID != store.DefaultOrgID {
			t.Fatalf("account %+v", pa)
		}
		if len(pa.AcceptedCurrencies) != 3 || pa.EnabledPurposes[0] != "one_time" {
			t.Errorf("defaults: %+v", pa)
		}
		var raw []byte
		_ = e.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT config_enc FROM provider_accounts WHERE id = ?", pa.ID).Scan(&raw)
		})
		if len(raw) == 0 || bytes.Contains(raw, []byte("fake_key_123")) {
			t.Error("config stored in clear")
		}
		var links int
		_ = e.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM app_provider_accounts WHERE app_id = 'app_1' AND provider_account_id = ?", pa.ID).Scan(&links)
		})
		if links != 1 {
			t.Error("account not linked to the app")
		}
		var n int
		_ = e.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_log WHERE action = 'provider_account.create' AND after LIKE '%fake_key%'").Scan(&n)
		})
		if n != 0 {
			t.Error("config leaked into the audit log")
		}
		narrowed, err := e.svc.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{Connector: e.fake.Name, Name: "inr", Mode: "live",
			Config: json.RawMessage(fakeconn.ValidConfig), Currencies: []string{"INR"}})
		if err != nil || len(narrowed.AcceptedCurrencies) != 1 {
			t.Errorf("narrowed: %+v %v", narrowed, err)
		}
	})
}

func TestCreateAccountValidation(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		in := func(mod func(*connectors.CreateAccountInput)) connectors.CreateAccountInput {
			i := connectors.CreateAccountInput{Connector: e.fake.Name, Name: "n", Mode: "test", Config: json.RawMessage(fakeconn.ValidConfig)}
			mod(&i)
			return i
		}
		for name, c := range map[string]connectors.CreateAccountInput{
			"unknown connector":    in(func(i *connectors.CreateAccountInput) { i.Connector = "nope" }),
			"empty name":           in(func(i *connectors.CreateAccountInput) { i.Name = " " }),
			"bad mode":             in(func(i *connectors.CreateAccountInput) { i.Mode = "prod" }),
			"missing required":     in(func(i *connectors.CreateAccountInput) { i.Config = json.RawMessage(`{"region":"x"}`) }),
			"empty required":       in(func(i *connectors.CreateAccountInput) { i.Config = json.RawMessage(`{"api_key":""}`) }),
			"unknown key":          in(func(i *connectors.CreateAccountInput) { i.Config = json.RawMessage(`{"api_key":"k","bogus":1}`) }),
			"wrong type":           in(func(i *connectors.CreateAccountInput) { i.Config = json.RawMessage(`{"api_key":5}`) }),
			"not an object":        in(func(i *connectors.CreateAccountInput) { i.Config = json.RawMessage(`[]`) }),
			"not json":             in(func(i *connectors.CreateAccountInput) { i.Config = json.RawMessage(`{`) }),
			"unsupported currency": in(func(i *connectors.CreateAccountInput) { i.Currencies = []string{"JPY"} }),
			"bad purpose":          in(func(i *connectors.CreateAccountInput) { i.Purposes = []string{"gift"} }),
		} {
			if _, err := e.svc.CreateAccount(ctx, audit.System, org, c); !errors.Is(err, core.ErrInvalid) {
				t.Errorf("%s: want ErrInvalid, got %v", name, err)
			}
		}
		if _, err := e.svc.CreateAccount(ctx, audit.System, org, in(func(i *connectors.CreateAccountInput) { i.AppID = "app_missing" })); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("unknown app: %v", err)
		}
		if _, err := e.svc.CreateAccount(ctx, audit.System, core.OrgScope{}, in(func(*connectors.CreateAccountInput) {})); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("empty scope: %v", err)
		}
		var n int
		_ = e.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM provider_accounts").Scan(&n)
		})
		if n != 0 {
			t.Errorf("%d accounts created by invalid requests", n)
		}
	})
}

func TestAccountMutationsAreOrgScoped(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		pa := e.create(fakeconn.ValidConfig)
		_ = e.db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO orgs (id, name, created_at) VALUES ('org_other', 'Other', 1)")
			if err != nil {
				return err
			}
			return store.InsertApp(ctx, tx, store.App{ID: "app_other", OrgID: "org_other", Name: "O", Slug: "o", CreatedAt: e.clk.t})
		})
		other := core.OrgScope{OrgID: "org_other"}
		name := "hijack"
		if err := e.svc.UpdateAccount(ctx, audit.System, other, pa.ID, connectors.UpdateAccountInput{Name: &name}); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("cross-org update: %v", err)
		}
		if err := e.svc.LinkApp(ctx, audit.System, other, pa.ID, "app_other"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("cross-org link of an account: %v", err)
		}
		if err := e.svc.LinkApp(ctx, audit.System, org, pa.ID, "app_other"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("link of another org's app: %v", err)
		}
		if _, err := e.svc.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{Connector: e.fake.Name, Name: "x", Mode: "test",
			Config: json.RawMessage(fakeconn.ValidConfig), AppID: "app_other"}); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("create linked to another org's app: %v", err)
		}
		got, _ := e.svc.Account(ctx, org, pa.ID)
		if got.Name != "main" {
			t.Errorf("name changed: %+v", got)
		}
		if _, err := e.svc.Account(ctx, other, pa.ID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("cross-org read: %v", err)
		}
	})
}

func TestGetCachesAndReloadsOnChange(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		pa := e.create(fakeconn.ValidConfig)
		for i := 0; i < 3; i++ {
			live, err := e.svc.Get(ctx, pa.ID)
			if err != nil || live.Connector == nil || live.Account.ID != pa.ID {
				t.Fatalf("Get: %+v %v", live, err)
			}
		}
		e.fake.Locked(func() {
			if e.fake.Built != 2 { // once by CreateAccount validation, once by the first Get
				t.Errorf("factory ran %d times, want 2 (validation + one cached instance)", e.fake.Built)
			}
		})
		cfg := `{"api_key":"rotated_key"}`
		if err := e.svc.UpdateAccount(ctx, audit.System, org, pa.ID, connectors.UpdateAccountInput{Config: json.RawMessage(cfg)}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Get(ctx, pa.ID); err != nil {
			t.Fatal(err)
		}
		e.fake.Locked(func() {
			last := string(e.fake.Configs[len(e.fake.Configs)-1])
			if !strings.Contains(last, "rotated_key") {
				t.Errorf("cache not invalidated on config change: %s", last)
			}
		})
		// A change made through another instance (another process) is noticed too.
		other := newSvc(t, e.db, e.box, e.clk)
		if err := other.UpdateAccount(ctx, audit.System, org, pa.ID, connectors.UpdateAccountInput{Config: json.RawMessage(`{"api_key":"third"}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Get(ctx, pa.ID); err != nil {
			t.Fatal(err)
		}
		e.fake.Locked(func() {
			if !strings.Contains(string(e.fake.Configs[len(e.fake.Configs)-1]), "third") {
				t.Error("stale instance served after a change from another process")
			}
		})
		if err := e.svc.UpdateAccount(ctx, audit.System, org, pa.ID, connectors.UpdateAccountInput{Config: json.RawMessage(`{}`)}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("invalid replacement config accepted: %v", err)
		}
		if _, err := e.svc.Get(ctx, "pa_missing"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("missing account: %v", err)
		}
	})
}

func TestUpdateAccountFields(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		pa := e.create(fakeconn.ValidConfig)
		name, status := "renamed", "disabled"
		if err := e.svc.UpdateAccount(ctx, audit.System, org, pa.ID, connectors.UpdateAccountInput{Name: &name, Status: &status, Currencies: []string{"USD"}}); err != nil {
			t.Fatal(err)
		}
		got, _ := e.svc.Account(ctx, org, pa.ID)
		if got.Name != "renamed" || got.Status != "disabled" || len(got.AcceptedCurrencies) != 1 || got.AcceptedCurrencies[0] != "USD" {
			t.Errorf("%+v", got)
		}
		bad := "frozen"
		if err := e.svc.UpdateAccount(ctx, audit.System, org, pa.ID, connectors.UpdateAccountInput{Status: &bad}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("bad status: %v", err)
		}
		if err := e.svc.UpdateAccount(ctx, audit.System, org, pa.ID, connectors.UpdateAccountInput{Currencies: []string{"JPY"}}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("unsupported currency: %v", err)
		}
	})
}

func TestDepsWiring(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		pa := e.create(fakeconn.ValidConfig)
		live, err := e.svc.Get(ctx, pa.ID)
		if err != nil {
			t.Fatal(err)
		}
		_ = live
		var deps connector.Deps
		e.fake.Locked(func() { deps = e.fake.Deps[len(e.fake.Deps)-1] })
		if deps.HTTP == nil || deps.KV == nil || deps.URLs == nil || deps.Logger == nil || deps.Now().IsZero() {
			t.Fatalf("incomplete deps %+v", deps)
		}
		if got := deps.URLs.WebhookURL(); got != "https://pay.example.com/hooks/"+pa.ID {
			t.Errorf("webhook url %q", got)
		}
		if got := deps.URLs.ReturnURL("REF 1"); got != "https://pay.example.com/return/"+pa.ID+"/REF%201" {
			t.Errorf("return url %q", got)
		}
		if got := e.svc.WebhookURL(pa.ID); got != "https://pay.example.com/hooks/"+pa.ID {
			t.Errorf("WebhookURL %q", got)
		}
	})
}

func TestReturnURLUsesPrimaryActiveDomainOfTheAttemptsApp(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		pa := e.create(fakeconn.ValidConfig)
		err := e.db.Write(ctx, func(tx store.Tx) error {
			if err := store.InsertPayment(ctx, tx, store.Payment{ID: "pay_1", OrgID: store.DefaultOrgID, AppID: "app_1", Mode: "test", Kind: "one_time",
				Amount: 1, Currency: "INR", Status: "created", ClientSecretHash: "x", ExpiresAt: e.clk.t, Version: 1, CreatedAt: e.clk.t, UpdatedAt: e.clk.t}); err != nil {
				return err
			}
			if err := store.InsertAttempt(ctx, tx, store.Attempt{ID: "att_1", OrgID: store.DefaultOrgID, Mode: "test", PaymentID: "pay_1",
				ProviderAccountID: pa.ID, ProviderRef: "REF1", Status: "created", CreatedAt: e.clk.t, UpdatedAt: e.clk.t}); err != nil {
				return err
			}
			return store.InsertDomain(ctx, tx, store.Domain{ID: "dom_1", OrgID: store.DefaultOrgID, AppID: "app_1", Host: "pay.shop.example.org",
				IsPrimary: true, Status: "pending", Token: "t", CreatedAt: e.clk.t})
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Get(ctx, pa.ID); err != nil {
			t.Fatal(err)
		}
		var urls connector.URLs
		e.fake.Locked(func() { urls = e.fake.Deps[len(e.fake.Deps)-1].URLs })
		if got := urls.ReturnURL("REF1"); !strings.HasPrefix(got, "https://pay.example.com/return/") {
			t.Errorf("pending domain must not be used: %q", got)
		}
		_ = e.db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE app_domains SET status = 'active'")
			return err
		})
		if got := urls.ReturnURL("REF1"); got != "https://pay.shop.example.org/return/"+pa.ID+"/REF1" {
			t.Errorf("active primary domain not used: %q", got)
		}
		if got := urls.ReturnURL("UNKNOWN"); !strings.HasPrefix(got, "https://pay.example.com/return/") {
			t.Errorf("unknown ref must fall back to the base URL: %q", got)
		}
	})
}

func TestKVIsEncryptedExpiringAndPerAccount(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		a, b := e.create(fakeconn.ValidConfig), e.create(fakeconn.ValidConfig)
		for _, pa := range []store.ProviderAccount{a, b} {
			if _, err := e.svc.Get(ctx, pa.ID); err != nil {
				t.Fatal(err)
			}
		}
		var kvA, kvB connector.KV
		e.fake.Locked(func() {
			n := len(e.fake.Deps)
			kvA, kvB = e.fake.Deps[n-2].KV, e.fake.Deps[n-1].KV
		})
		if err := kvA.Set(ctx, "token", []byte("secret-token"), time.Hour); err != nil {
			t.Fatal(err)
		}
		if v, ok, err := kvA.Get(ctx, "token"); err != nil || !ok || string(v) != "secret-token" {
			t.Fatalf("get: %q %v %v", v, ok, err)
		}
		if _, ok, _ := kvB.Get(ctx, "token"); ok {
			t.Error("accounts share KV state")
		}
		var raw []byte
		_ = e.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT value_enc FROM connector_kv WHERE provider_account_id = ?", a.ID).Scan(&raw)
		})
		if len(raw) == 0 || bytes.Contains(raw, []byte("secret-token")) {
			t.Error("KV value stored in clear")
		}
		e.clk.t = e.clk.t.Add(2 * time.Hour)
		if _, ok, _ := kvA.Get(ctx, "token"); ok {
			t.Error("expired value returned")
		}
		_ = kvA.Set(ctx, "forever", []byte("x"), 0)
		e.clk.t = e.clk.t.Add(1000 * time.Hour)
		if _, ok, _ := kvA.Get(ctx, "forever"); !ok {
			t.Error("ttl 0 must not expire")
		}
		if err := kvA.Delete(ctx, "forever"); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := kvA.Get(ctx, "forever"); ok {
			t.Error("delete failed")
		}
		// A value sealed for another account/key cannot be moved.
		_ = kvA.Set(ctx, "k", []byte("v"), 0)
		_ = e.db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE connector_kv SET provider_account_id = ? WHERE provider_account_id = ? AND key = 'k'", b.ID, a.ID)
			return err
		})
		if _, ok, err := kvB.Get(ctx, "k"); ok || err == nil {
			t.Errorf("moved ciphertext was accepted (ok=%v err=%v)", ok, err)
		}
	})
}

func TestVerifiers(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		pa := e.create(fakeconn.ValidConfig)
		v, err := e.svc.Verifiers()(ctx, pa.ID)
		if err != nil {
			t.Fatal(err)
		}
		body := []byte(`[{"kind":"payment","event_id":"evt_1","event_type":"x"}]`)
		id, err := v.Verify(http.Header{"X-Fake-Sig": {"ok"}}, body)
		if err != nil || id != "evt_1" {
			t.Errorf("signed: %q %v", id, err)
		}
		id, err = v.Verify(http.Header{"X-Fake-Sig": {"unsigned"}}, body)
		if !errors.Is(err, inbound.ErrUnsigned) || id != "evt_1" {
			t.Errorf("an event type the provider does not sign must report ErrUnsigned with its event id: %q %v", id, err)
		}
		for name, h := range map[string]http.Header{"missing": {}, "wrong": {"X-Fake-Sig": {"nope"}}} {
			id, err = v.Verify(h, body)
			if err == nil || errors.Is(err, inbound.ErrUnsigned) || id != "" {
				t.Errorf("%s signature must be a hard failure (401), got id=%q err=%v", name, id, err)
			}
		}
		id, err = v.Verify(http.Header{"X-Fake-Sig": {"ok"}}, []byte(`not json`))
		if err != nil || id != "" {
			t.Errorf("signed but unparseable body: %q %v", id, err)
		}
		if _, err := e.svc.Verifiers()(ctx, "pa_missing"); !errors.Is(err, inbound.ErrUnknownAccount) {
			t.Errorf("unknown account: %v", err)
		}
	})
}

func TestValidateConfig(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["a","n"],"additionalProperties":false,
		"properties":{"a":{"type":"string","minLength":1},"n":{"type":"integer"},"b":{"type":"boolean"},"o":{"type":"object"}}}`)
	ok := []string{`{"a":"x","n":3}`, `{"a":"x","n":3,"b":false,"o":{}}`}
	bad := []string{`{"a":"x"}`, `{"a":"","n":1}`, `{"a":"x","n":1.5}`, `{"a":"x","n":"1"}`, `{"a":"x","n":1,"b":"yes"}`,
		`{"a":"x","n":1,"zzz":1}`, `{"a":null,"n":1}`, `null`, `"s"`, ``}
	for _, c := range ok {
		if err := connectors.ValidateConfig(schema, json.RawMessage(c)); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
	for _, c := range bad {
		if err := connectors.ValidateConfig(schema, json.RawMessage(c)); err == nil {
			t.Errorf("%s: accepted", c)
		}
	}
	if err := connectors.ValidateConfig(nil, json.RawMessage(`{"anything":1}`)); err != nil {
		t.Errorf("no schema means any object: %v", err)
	}
}

func TestTestConnection(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		pa := e.create(fakeconn.ValidConfig)
		// the provider does not know the dummy payment: credentials work
		res, err := e.svc.TestConnection(ctx, org, pa.ID)
		if err != nil || !res.OK || !strings.Contains(res.Message, "accepted") {
			t.Fatalf("%+v %v", res, err)
		}
		if len(e.fake.Fetches) != 1 || e.fake.Fetches[0].ProviderRef == "" {
			t.Fatalf("expected one FetchPayment of a dummy ref: %+v", e.fake.Fetches)
		}
		// rejected credentials
		e.fake.Locked(func() { e.fake.FetchErr = connector.ErrInvalidCredentials })
		res, err = e.svc.TestConnection(ctx, org, pa.ID)
		if err != nil || res.OK || !strings.Contains(res.Message, "credentials") {
			t.Fatalf("invalid credentials: %+v %v", res, err)
		}
		// any other failure is inconclusive, not a pass
		e.fake.Locked(func() { e.fake.FetchErr = errors.New("connection refused") })
		res, err = e.svc.TestConnection(ctx, org, pa.ID)
		if err != nil || res.OK || !strings.Contains(res.Message, "connection refused") {
			t.Fatalf("other error: %+v %v", res, err)
		}
		// accounts of another org are invisible
		if _, err := e.svc.TestConnection(ctx, core.OrgScope{OrgID: "org_other"}, pa.ID); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("other org: %v", err)
		}
		if _, err := e.svc.TestConnection(ctx, org, "pa_missing"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
	})
}

func TestUpdateAccountPurposes(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		pa := e.create(fakeconn.ValidConfig)
		if err := e.svc.UpdateAccount(ctx, audit.System, org, pa.ID, connectors.UpdateAccountInput{Purposes: []string{"one_time", "subscription"}}); err != nil {
			t.Fatal(err)
		}
		got, _ := e.svc.Account(ctx, org, pa.ID)
		if strings.Join(got.EnabledPurposes, ",") != "one_time,subscription" {
			t.Fatalf("purposes %v", got.EnabledPurposes)
		}
		if err := e.svc.UpdateAccount(ctx, audit.System, org, pa.ID, connectors.UpdateAccountInput{Purposes: []string{"barter"}}); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("bad purpose: %v", err)
		}
		got, _ = e.svc.Account(ctx, org, pa.ID)
		if len(got.EnabledPurposes) != 2 {
			t.Fatalf("a rejected update changed purposes: %v", got.EnabledPurposes)
		}
	})
}
