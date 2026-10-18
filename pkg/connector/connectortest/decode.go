package connectortest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// RecordsDecodeFailures checks that a connector which gets a 2xx answer it
// cannot decode says so to the HTTP client (connector.ReportDecodeError), so the
// call lands in the error log with its body. The provider is a server that
// answers every request with a 200 and an HTML page; call makes one request
// through the connector built from cfg and may return its error.
func RecordsDecodeFailures(t *testing.T, factory connector.Factory, cfg json.RawMessage, call func(ctx context.Context, c connector.Connector) error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>maintenance page</html>"))
	}))
	defer srv.Close()
	var mu sync.Mutex
	var got []connector.CallRecord
	deps := NewDeps(srv, time.Unix(1760000100, 0))
	deps.HTTP = connector.NewHTTPClient(connector.HTTPOptions{Base: RedirectClient(srv).Transport, Recorder: func(_ context.Context, c connector.CallRecord) {
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
	}})
	c, err := factory(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	_ = call(context.Background(), c) // an error is expected
	mu.Lock()
	defer mu.Unlock()
	for _, r := range got {
		if r.Failure == connector.FailDecode && r.Status == 200 && string(r.ResponseBody) == "<html>maintenance page</html>" && r.Err != nil {
			return
		}
	}
	t.Errorf("an undecodable 2xx answer was not reported through the recorder: %+v", got)
}
