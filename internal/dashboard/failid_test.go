package dashboard

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// An internal error on a dashboard page is logged with the request id the
// middleware gave the response, so the log row can be found from the page.
func TestDashboardFailLogsTheRequestID(t *testing.T) {
	var buf bytes.Buffer
	d := &Dashboard{o: Options{Logger: slog.New(slog.NewTextHandler(&buf, nil))}}
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "req-dash-1")
	d.fail(rec, errors.New("boom"))
	if rec.Code != 500 || !strings.Contains(buf.String(), "request_id=req-dash-1") || !strings.Contains(buf.String(), "boom") {
		t.Errorf("%d %s", rec.Code, buf.String())
	}
}
