package dashboard

import (
	"encoding/json"
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
)

type dashboardLiveEvent struct {
	Type       string `json:"type"`
	ObjectType string `json:"object_type"`
	ObjectID   string `json:"object_id"`
	AppID      string `json:"app_id"`
	Version    int64  `json:"version"`
}

func (d *Dashboard) routesLive() {
	d.mux.HandleFunc("GET "+prefix+"/events", d.authed(authz.PaymentRead, d.dashboardEvents))
}

// dashboardEvents streams lightweight invalidations for the signed-in
// organization and selected mode. Last-Event-ID resumes a disconnected stream.
func (d *Dashboard) dashboardEvents(w http.ResponseWriter, r *http.Request, s *session) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	org, err := d.orgID(r.Context())
	if err != nil {
		d.fail(w, err)
		return
	}
	mode := d.mode(r, s)
	cursor := r.Header.Get("Last-Event-ID")
	err = d.o.DB.Read(r.Context(), func(q store.Querier) error {
		if cursor != "" {
			visible, err := store.DashboardEventVisible(r.Context(), q, cursor, org, mode)
			if err != nil {
				return err
			}
			if visible {
				return nil
			}
		}
		cursor, err = store.LatestDashboardEventID(r.Context(), q, org, mode)
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
	flusher.Flush()

	poll := time.NewTicker(dashboardEventPoll)
	heartbeat := time.NewTicker(dashboardEventHeartbeat)
	defer poll.Stop()
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-poll.C:
			for {
				var events []store.EventRow
				err := d.o.DB.Read(r.Context(), func(q store.Querier) (err error) {
					events, err = store.DashboardEventsAfter(r.Context(), q, org, mode, cursor, dashboardEventBatch)
					return err
				})
				if err != nil {
					d.o.Logger.Warn("dashboard: live events", "org_id", org, "mode", mode, "err", err)
					return
				}
				for _, event := range events {
					data, err := json.Marshal(dashboardLiveEvent{Type: event.Type, ObjectType: event.ObjectType,
						ObjectID: event.ObjectID, AppID: event.AppID, Version: event.ObjectVersion})
					if err != nil {
						continue
					}
					if _, err := fmt.Fprintf(w, "id: %s\ndata: %s\n\n", event.ID, data); err != nil {
						return
					}
					cursor = event.ID
				}
				if len(events) > 0 {
					flusher.Flush()
				}
				if len(events) < dashboardEventBatch {
					break
				}
			}
		}
	}
}
