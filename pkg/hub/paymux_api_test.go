package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"testing"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/logging"
	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// seedApp migrates the database at url, creates an app and returns a test key.
func seedApp(t *testing.T, url, dataDir string) string {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, url, dataDir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
	set := settings.New(db)
	if _, err := core.Bootstrap(ctx, db, set); err != nil {
		t.Fatal(err)
	}
	svc := core.NewService(core.Options{DB: db, Settings: set})
	app, err := svc.CreateApp(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, core.CreateAppInput{Name: "E2E", Slug: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := svc.CreateAPIKey(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, app.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	// Payments need a routable provider account.
	fake := fakeconn.Register("fake")
	box, err := secrets.NewBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := neturl.Parse("http://localhost:8080")
	conns := connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base})
	if _, err := conns.CreateAccount(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, connectors.CreateAccountInput{
		Connector: fake.Name, Name: "main", Mode: "test", Config: json.RawMessage(fakeconn.ValidConfig), AppID: app.ID}); err != nil {
		t.Fatal(err)
	}
	doc := `{"rules":[],"catch_all":{"name":"all","when":{},"action":{"kind":"route","targets":[{"connector":"` + fake.Name + `"}]}}}`
	if err := set.SetOrg(ctx, audit.System, store.DefaultOrgID, "routing", json.RawMessage(doc)); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestAPIIsMountedAndEmitsThroughInjectedEmitter(t *testing.T) {
	run := func(t *testing.T, url, dataDir string) {
		key := seedApp(t, url, dataDir)
		var mu sync.Mutex
		var got []events.Event
		em := events.EmitterFunc(func(_ context.Context, _ store.Tx, e events.Event) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, e)
			return nil
		})
		cfg := testConfig(t, url, dataDir)
		logs := &syncBuf{}
		app, err := New(cfg, WithLogger(logging.New(logs, -4)), WithEmitter(em))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- app.Start(ctx) }()
		r := &running{app: app, logs: logs, done: make(chan struct{}), stop: cancel}
		go func() { r.err = <-done; close(r.done) }()
		waitReady(t, r)

		post := func(auth string) (int, string) {
			req, _ := http.NewRequest("POST", "http://"+app.Addr()+"/v1/payments",
				bytes.NewReader([]byte(`{"amount":1000,"currency":"INR","order_ref":"o1"}`)))
			if auth != "" {
				req.Header.Set("Authorization", "Bearer "+auth)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			b, _ := io.ReadAll(res.Body)
			return res.StatusCode, string(b)
		}
		if code, _ := post(""); code != 401 {
			t.Errorf("unauthenticated: %d", code)
		}
		code, body := post(key)
		if code != 201 {
			t.Fatalf("create payment: %d %s", code, body)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(body), &m)
		if !strings.HasPrefix(m["checkout_url"].(string), "http://localhost:8080/pay/"+m["id"].(string)+"?t=") {
			t.Errorf("checkout_url %v (BaseURL comes from config)", m["checkout_url"])
		}
		mu.Lock()
		if len(got) != 1 || got[0].Type != "payment.created" || got[0].ObjectID != m["id"] {
			t.Errorf("injected emitter saw %+v", got)
		}
		mu.Unlock()
		shutdown(t, r)
	}
	t.Run("sqlite", func(t *testing.T) {
		dir := t.TempDir()
		run(t, "sqlite://"+dir+"/paymux.db", dir)
	})
	t.Run("postgres", func(t *testing.T) { run(t, storetest.PostgresURL(t), t.TempDir()) })
}

func TestStartSeedsOrgAndSettingsOnce(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, "sqlite://"+dir+"/paymux.db", dir)
	for i := 0; i < 2; i++ { // second boot must not duplicate or fail
		r := startApp(t, cfg)
		waitReady(t, r)
		shutdown(t, r)
	}
	db, err := store.Open(context.Background(), cfg.DatabaseURL, dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var orgs, set int
	_ = db.Read(context.Background(), func(q store.Querier) error {
		_ = q.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM orgs").Scan(&orgs)
		return q.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM org_settings").Scan(&set)
	})
	if orgs != 1 || set != len(settings.Defs()) {
		t.Errorf("orgs=%d settings=%d", orgs, set)
	}
}
