package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/logging"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestTerminalFollowsTheSettings(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		set := settings.New(db)
		if err := db.Write(ctx, func(tx store.Tx) error { return store.SeedOrg(ctx, tx, "Paymux", time.Now()) }); err != nil {
			t.Fatal(err)
		}
		if err := set.SeedOrg(ctx, store.DefaultOrgID); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		term := logging.NewTerminal(&out)
		log := term.Logger()

		put := func(key, raw string) {
			t.Helper()
			if err := set.SetOrg(ctx, audit.System, store.DefaultOrgID, key, json.RawMessage(raw)); err != nil {
				t.Fatal(err)
			}
			if err := applyTerminalSettings(ctx, set, term); err != nil {
				t.Fatal(err)
			}
		}

		if err := applyTerminalSettings(ctx, set, term); err != nil {
			t.Fatal(err)
		}
		log.Debug("default debug")
		log.Info("default info")
		log.Info(logging.HTTPRequestMsg, "path", "/default", "status", 200)

		put("terminal_log_requests", "false")
		log.Info(logging.HTTPRequestMsg, "path", "/off", "status", 200)

		put("terminal_log_requests", "true")
		put("terminal_log_level", `"warn"`)
		log.Info("warn hides info")
		log.Warn("warn keeps warn")

		put("terminal_log_level", `"debug"`)
		log.Log(ctx, slog.LevelDebug, "debug shows debug")

		s := out.String()
		for _, want := range []string{"default info", "/default", "warn keeps warn", "debug shows debug"} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %q in:\n%s", want, s)
			}
		}
		for _, hidden := range []string{"default debug", "/off", "warn hides info"} {
			if strings.Contains(s, hidden) {
				t.Errorf("%q should be hidden in:\n%s", hidden, s)
			}
		}
	})
}

// A setting saved while the hub runs reaches the terminal at the next tick.
func TestTerminalFollowerAppliesASavedSetting(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		set := settings.New(db)
		if err := db.Write(ctx, func(tx store.Tx) error { return store.SeedOrg(ctx, tx, "Paymux", time.Now()) }); err != nil {
			t.Fatal(err)
		}
		if err := set.SeedOrg(ctx, store.DefaultOrgID); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		term := logging.NewTerminal(&out)
		a := &App{term: term, log: term.Logger()}
		tick := make(chan time.Time)
		stop := a.followTerminalTicks(ctx, set, tick)
		defer stop()

		if err := set.SetOrg(ctx, audit.System, store.DefaultOrgID, "terminal_log_level", json.RawMessage(`"warn"`)); err != nil {
			t.Fatal(err)
		}
		// The loop takes one tick at a time, so the second send returns only
		// after the first tick has been applied in full.
		tick <- time.Time{}
		tick <- time.Time{}
		stop()

		term.Logger().Info("hidden after the tick")
		term.Logger().Warn("shown after the tick")
		s := out.String()
		if strings.Contains(s, "hidden after the tick") || !strings.Contains(s, "shown after the tick") {
			t.Errorf("the follower did not apply the saved level:\n%s", s)
		}
	})
}
