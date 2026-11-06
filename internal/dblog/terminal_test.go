package dblog_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/dblog"
	"github.com/velixio/paymux/internal/logging"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// The terminal's switches decide what prints, never what the Logs page keeps.
func TestTerminalSwitchesDoNotChangeWhatTheDatabaseKeeps(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(o *dblog.Options) { o.Level = slog.LevelInfo })
		var out bytes.Buffer
		term := logging.NewTerminal(&syncBuf{b: &out})
		log := slog.New(e.rec.Handler(term.Logger().Handler()))

		term.SetLevel(slog.LevelError)
		log.Warn("quiet warning")
		term.SetLevel(slog.LevelInfo)
		term.SetRequests(false)
		log.Info(logging.HTTPRequestMsg, "path", "/ok", "status", 200)

		rows := e.rows(t)
		for _, want := range []string{"quiet warning", logging.HTTPRequestMsg} {
			found := false
			for _, r := range rows {
				found = found || r.Msg == want
			}
			if !found {
				t.Errorf("%q not kept in the database: %+v", want, rows)
			}
		}
		if s := out.String(); strings.Contains(s, "quiet warning") || strings.Contains(s, "/ok") {
			t.Errorf("hidden lines reached the terminal: %s", s)
		}
	})
}
