package whop

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// The Whop sandbox uses its own API host (whop.com/blog/whop-sandbox).
func TestSandboxConfigSelectsSandboxHost(t *testing.T) {
	for _, c := range []struct {
		sandbox bool
		want    string
	}{{false, "https://api.whop.com/api/v1"}, {true, "https://sandbox-api.whop.com/api/v1"}} {
		raw, _ := json.Marshal(map[string]any{"api_key": "k", "company_id": "biz_x", "webhook_secret": "ws_x", "sandbox": c.sandbox})
		conn, err := New(raw, connectortest.NewDeps(nil, time.Now()))
		if err != nil {
			t.Fatal(err)
		}
		if got := conn.(*whop).base(); got != c.want {
			t.Errorf("sandbox=%v: base %q, want %q", c.sandbox, got, c.want)
		}
	}
}
