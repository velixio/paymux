package hub

import (
	"context"
	"encoding/json"
	"time"

	"github.com/velixio/paymux/internal/inbound"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
)

// providerEventPurgeKind is the scheduled job that applies provider_event_retention.
const providerEventPurgeKind = "provider_events.purge"

// registerProviderEventPurge schedules the hourly purge of stored provider
// webhooks: everything older than provider_event_retention days (0 keeps
// verified events), and events that failed verification after 30 days at the
// latest.
func registerProviderEventPurge(q *jobs.Queue, db *store.DB, set *settings.Service) error {
	p := inbound.NewPurger(inbound.PurgeOptions{DB: db, Retention: func(ctx context.Context) (int, error) {
		v, err := set.ResolveOrg(ctx, store.DefaultOrgID, "provider_event_retention") // TODO(spec): one org today
		if err != nil {
			return 0, err
		}
		var n int
		if err := v.Decode(&n); err != nil {
			return 0, err
		}
		return n, nil
	}})
	q.Register(providerEventPurgeKind, func(ctx context.Context, _ json.RawMessage) error {
		_, err := p.Run(ctx)
		return err
	})
	return q.AddSchedule(jobs.Schedule{Name: providerEventPurgeKind, Kind: providerEventPurgeKind, Every: time.Hour})
}
