package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestTerminalStartsAtInfoWithRequestsShown(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf)
	log := term.Logger()

	log.Debug("too quiet")
	log.Info("kept")
	log.Info(HTTPRequestMsg, "status", 200)

	out := buf.String()
	if strings.Contains(out, "too quiet") {
		t.Errorf("debug printed at the default level: %s", out)
	}
	if !strings.Contains(out, "kept") || !strings.Contains(out, HTTPRequestMsg) {
		t.Errorf("default output lost lines: %s", out)
	}
}

func TestTerminalLevelChangesWhileRunning(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf)
	log := term.Logger() // taken before the change, as the whole hub holds it

	term.SetLevel(slog.LevelWarn)
	log.Info("hidden at warn")
	log.Warn("shown at warn")
	term.SetLevel(slog.LevelDebug)
	log.Debug("shown at debug")

	out := buf.String()
	if strings.Contains(out, "hidden at warn") {
		t.Errorf("info printed at warn: %s", out)
	}
	if !strings.Contains(out, "shown at warn") || !strings.Contains(out, "shown at debug") {
		t.Errorf("lines missing after a level change: %s", out)
	}
}

func TestTerminalRequestToggleHidesOnlySuccessfulRequestLines(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf)
	log := term.Logger()

	term.SetRequests(false)
	log.Info(HTTPRequestMsg, "path", "/ok", "status", 200)
	log.Info(HTTPRequestMsg, "path", "/redirect", "status", 303)
	log.Info(HTTPRequestMsg, "path", "/missing", "status", 404)
	log.Info(HTTPRequestMsg, "path", "/broken", "status", 500)
	log.Info("other info line")

	out := buf.String()
	for _, hidden := range []string{"/ok", "/redirect"} {
		if strings.Contains(out, hidden) {
			t.Errorf("%s printed with requests off: %s", hidden, out)
		}
	}
	for _, shown := range []string{"/missing", "/broken", "other info line"} {
		if !strings.Contains(out, shown) {
			t.Errorf("%s missing with requests off: %s", shown, out)
		}
	}

	term.SetRequests(true)
	log.Info(HTTPRequestMsg, "path", "/back-on", "status", 200)
	if !strings.Contains(buf.String(), "/back-on") {
		t.Errorf("request line missing after turning requests back on: %s", buf.String())
	}
}

// A logger derived with With shares the switches and keeps redacting.
func TestTerminalDerivedLoggerFollowsTheSwitchesAndRedacts(t *testing.T) {
	var buf bytes.Buffer
	term := NewTerminal(&buf)
	log := term.Logger().With("component", "web")

	term.SetRequests(false)
	log.Info(HTTPRequestMsg, "path", "/ok", "status", 200)
	if strings.Contains(buf.String(), "/ok") {
		t.Errorf("derived logger printed a hidden request: %s", buf.String())
	}

	term.SetLevel(slog.LevelError)
	log.Warn("hidden warning")
	if strings.Contains(buf.String(), "hidden warning") {
		t.Errorf("derived logger ignored the level: %s", buf.String())
	}

	const s = "Zq7pX2mL9vR4tK8w"
	log.Error("x", "client_secret", s)
	if out := buf.String(); strings.Contains(out, s) || !strings.Contains(out, redacted) {
		t.Errorf("secret not redacted: %s", out)
	}
}
