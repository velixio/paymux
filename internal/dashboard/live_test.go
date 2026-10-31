package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestDashboardEventStreamResumesAndScopesMode(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		viewer := e.roleClient("viewer")
		now := e.clk.Now()
		if err := db.Write(ctx, func(tx store.Tx) error {
			for _, event := range []store.EventRow{
				{ID: "evt_01", OrgID: store.DefaultOrgID, AppID: "app_live", Mode: "live", Type: "payment.created", ObjectType: "payment", ObjectID: "pay_old", ObjectVersion: 1, Data: json.RawMessage(`{"secret":"not streamed"}`), CreatedAt: now},
				{ID: "evt_02", OrgID: store.DefaultOrgID, AppID: "app_test", Mode: "test", Type: "payment.succeeded", ObjectType: "payment", ObjectID: "pay_test", ObjectVersion: 2, Data: json.RawMessage(`{}`), CreatedAt: now},
				{ID: "evt_03", OrgID: store.DefaultOrgID, AppID: "app_live", Mode: "live", Type: "payment.succeeded", ObjectType: "payment", ObjectID: "pay_live", ObjectVersion: 2, Data: json.RawMessage(`{"secret":"not streamed"}`), CreatedAt: now},
			} {
				if err := db.InsertEvent(ctx, tx, event); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		streamCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, e.srv.URL+"/dashboard/events", nil)
		req.Header.Set("Last-Event-ID", "evt_01")
		resp, err := viewer.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || resp.Header.Get("X-Accel-Buffering") != "no" {
			t.Fatalf("stream response: %d %q", resp.StatusCode, resp.Header)
		}
		scanner := bufio.NewScanner(resp.Body)
		var lines []string
		for scanner.Scan() {
			line := scanner.Text()
			lines = append(lines, line)
			if strings.HasPrefix(line, "data: ") {
				break
			}
		}
		body := strings.Join(lines, "\n")
		if !strings.Contains(body, "id: evt_03") || !strings.Contains(body, `"object_id":"pay_live"`) ||
			strings.Contains(body, "pay_test") || strings.Contains(body, "not streamed") {
			t.Fatalf("stream body:\n%s", body)
		}
	})
}
