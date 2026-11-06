package server

import (
	"bytes"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/logging"
	"github.com/velixio/paymux/internal/metrics"
)

// The access log's real lines must be the ones the terminal's switch hides.
func TestTerminalRequestSwitchHidesTheAccessLog(t *testing.T) {
	var out bytes.Buffer
	term := logging.NewTerminal(&out)
	s := New(Options{Logger: term.Logger(), Metrics: metrics.NewRegistry()})
	term.SetRequests(false)

	get(s.Handler(), "/healthz")
	if strings.Contains(out.String(), `"path":"/healthz"`) {
		t.Errorf("successful request printed with requests off: %s", out.String())
	}
	get(s.Handler(), "/nope")
	if !strings.Contains(out.String(), `"path":"/nope"`) {
		t.Errorf("failed request hidden with requests off: %s", out.String())
	}
}
