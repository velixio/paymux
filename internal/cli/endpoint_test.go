package cli

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/config"
	"github.com/velixio/paymux/internal/outbound/stdwebhooks"
	"github.com/velixio/paymux/internal/store"
)

func TestEndpointAddPrintsSecretOnce(t *testing.T) {
	dir := t.TempDir()
	lookup := migrateEnv(dir)
	var out, errb bytes.Buffer
	ctx := context.Background()
	if code := Run(ctx, []string{"migrate"}, lookup, &out, &errb); code != 0 {
		t.Fatalf("migrate: %s", errb.String())
	}
	cfg, err := config.Load(lookup)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DataDir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	err = db.Write(ctx, func(tx store.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO orgs (id, name, created_at) VALUES ('org_1', 'Acme', 1)`); err != nil {
			return err
		}
		return store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: "org_1", Name: "A", Slug: "a", CreatedAt: time.Now()})
	})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	out.Reset()
	args := []string{"endpoint", "add", "--app", "app_1", "--url", "https://example.com/hook", "--events", "payment.succeeded,refund.created"}
	if code := Run(ctx, args, lookup, &out, &errb); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	var secret string
	for _, f := range strings.Fields(out.String()) {
		if strings.HasPrefix(f, "whsec_") {
			secret = f
		}
	}
	if _, err := stdwebhooks.NewSigner(secret); err != nil {
		t.Fatalf("no usable secret in output %q: %v", out.String(), err)
	}
	if !strings.Contains(out.String(), "whe_") || !strings.Contains(out.String(), "payment.succeeded,refund.created") {
		t.Fatalf("output missing endpoint details: %q", out.String())
	}

	db, err = store.Open(ctx, cfg.DatabaseURL, cfg.DataDir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	eps, err := db.ListEndpoints(ctx, "app_1")
	if err != nil || len(eps) != 1 || !eps[0].Enabled || len(eps[0].EventTypes) != 2 || eps[0].OrgID != "org_1" {
		t.Fatalf("stored %+v %v", eps, err)
	}
	if bytes.Contains(eps[0].SigningSecretEnc, []byte(secret)) {
		t.Fatal("secret stored in clear")
	}
}

func TestEndpointUsageErrors(t *testing.T) {
	lookup := migrateEnv(t.TempDir())
	for _, args := range [][]string{
		{"endpoint"},
		{"endpoint", "bogus"},
		{"endpoint", "add", "--url", "https://example.com"},
		{"endpoint", "add", "--app", "app_1"},
		{"endpoint", "add", "--app", "app_1", "--url", "ftp://example.com"},
	} {
		var out, errb bytes.Buffer
		code := Run(context.Background(), args, lookup, &out, &errb)
		if code == 0 || errb.Len() == 0 {
			t.Errorf("%v: code=%d stderr=%q", args, code, errb.String())
		}
	}
}
