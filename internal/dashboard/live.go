package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/store"
)

const (
	dashboardEventPoll      = time.Second
	dashboardEventHeartbeat = 15 * time.Second
	dashboardEventBatch     = 100
	dashboardEventLookback  = 5 * time.Minute
)

type dashboardLiveEvent struct {
	Type       string `json:"type"`
	ObjectType string `json:"object_type"`
	ObjectID   string `json:"object_id"`
	AppID      string `json:"app_id"`
	Version    int64  `json:"version"`
}

// routesLive registers the authenticated dashboard event stream.
func (d *Dashboard) routesLive() {
	d.mux.HandleFunc("GET "+prefix+"/events", d.authed(authz.PaymentRead, d.dashboardEvents))
}

// dashboardEvents streams lightweight invalidations for the signed-in
// organization and selected mode. Last-Event-ID resumes a disconnected stream.
func (d *Dashboard) dashboardEvents(w http.ResponseWriter, r *http.Request, s *session) {
	controller := http.NewResponseController(w)
	org, err := d.orgID(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	mode := d.mode(r, s)
	cursorID := r.Header.Get("Last-Event-ID")
	var cursor store.EventCursor
	err = d.o.DB.Read(r.Context(), func(q store.Querier) error {
		if cursorID != "" {
			var visible bool
			cursor, visible, err = store.DashboardEventCursor(r.Context(), q, cursorID, org, mode)
			if err != nil {
				return err
			}
			if visible {
				return nil
			}
		}
		cursor, err = store.LatestDashboardEventCursor(r.Context(), q, org, mode)
		return err
	})
	if err != nil {
		d.fail(w, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	_, _ = fmt.Fprint(w, "retry: 2000\n\n")
	if err := controller.Flush(); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			d.o.Logger.Warn("dashboard: event streaming unsupported", "err", err)
		}
		return
	}

	poll := time.NewTicker(d.o.LivePollInterval)
	heartbeat := time.NewTicker(d.o.LiveHeartbeatInterval)
	seen := make(map[string]time.Time)
	if cursor.ID != "" {
		seen[cursor.ID] = cursor.CreatedAt
	}
	rescan := false
	defer poll.Stop()
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			rescan = true
			_, _ = fmt.Fprint(w, "data: {\"type\":\"dashboard.sync\"}\n\n")
			if controller.Flush() != nil {
				return
			}
		case <-poll.C:
			scan := cursor
			if rescan {
				scan = store.EventCursor{CreatedAt: cursor.CreatedAt.Add(-dashboardEventLookback)}
				rescan = false
			}
			for {
				var events []store.EventRow
				err := d.o.DB.Read(r.Context(), func(q store.Querier) (err error) {
					events, err = store.DashboardEventsPage(r.Context(), q, org, mode, scan, dashboardEventBatch)
					return err
				})
				if err != nil {
					d.o.Logger.Warn("dashboard: live events", "org_id", org, "mode", mode, "err", err)
					return
				}
				for _, event := range events {
					scan = store.EventCursor{ID: event.ID, CreatedAt: event.CreatedAt}
					if _, duplicate := seen[event.ID]; duplicate {
						continue
					}
					data, err := json.Marshal(dashboardLiveEvent{Type: event.Type, ObjectType: event.ObjectType,
						ObjectID: event.ObjectID, AppID: event.AppID, Version: event.ObjectVersion})
					if err != nil {
						continue
					}
					if _, err := fmt.Fprintf(w, "id: %s\ndata: %s\n\n", event.ID, data); err != nil {
						return
					}
					seen[event.ID] = event.CreatedAt
					if event.CreatedAt.After(cursor.CreatedAt) ||
						(event.CreatedAt.Equal(cursor.CreatedAt) && event.ID > cursor.ID) {
						cursor = scan
					}
				}
				if len(events) > 0 {
					if controller.Flush() != nil {
						return
					}
				}
				if len(events) < dashboardEventBatch {
					break
				}
			}
			cutoff := cursor.CreatedAt.Add(-dashboardEventLookback)
			for id, created := range seen {
				if created.Before(cutoff) {
					delete(seen, id)
				}
			}
		}
	}
}
