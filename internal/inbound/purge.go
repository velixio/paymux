package inbound

import (
	"context"
	"time"

	"github.com/paymux/paymux/internal/store"
)

// ForgedRetention is how long events that failed verification are kept (a hash,
// a size and a prefix of evidence) at the longest.
const ForgedRetention = 30 * 24 * time.Hour

// PurgeOptions configures a Purger.
type PurgeOptions struct {
	DB *store.DB
	// Retention is the provider_event_retention setting in days; 0 keeps verified
	// and unsigned events forever.
	Retention func(ctx context.Context) (days int, err error)
	Batch     int // rows per transaction; default 1000
	Now       func() time.Time
}

// PurgeResult counts what a run deleted.
type PurgeResult struct{ Forged, Events int }

// Purger deletes stored provider events past their retention: every event after
// provider_event_retention days, and events that failed verification after 30
// days at the latest, whatever the setting says.
type Purger struct{ o PurgeOptions }

// NewPurger builds a Purger.
func NewPurger(o PurgeOptions) *Purger {
	if o.Batch <= 0 {
		o.Batch = 1000
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Purger{o: o}
}

// Run makes one pass.
func (p *Purger) Run(ctx context.Context) (PurgeResult, error) {
	var res PurgeResult
	days, err := p.o.Retention(ctx)
	if err != nil {
		return res, err
	}
	now := p.o.Now()
	forgedFor := ForgedRetention
	if d := time.Duration(days) * 24 * time.Hour; days > 0 && d < forgedFor {
		forgedFor = d
	}
	if res.Forged, err = p.loop(ctx, true, now.Add(-forgedFor)); err != nil {
		return res, err
	}
	if days > 0 {
		res.Events, err = p.loop(ctx, false, now.AddDate(0, 0, -days))
	}
	return res, err
}

func (p *Purger) loop(ctx context.Context, forged bool, before time.Time) (int, error) {
	total := 0
	for ctx.Err() == nil {
		var n int
		if err := p.o.DB.Write(ctx, func(tx store.Tx) (err error) {
			n, err = store.PurgeProviderEvents(ctx, tx, forged, before, p.o.Batch)
			return
		}); err != nil {
			return total, err
		}
		if n == 0 {
			break
		}
		total += n
	}
	return total, nil
}
