package dblog

import (
	"context"
	"sync"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/store"
)

// MinAuditRetentionDays is the shortest audit retention ever applied (PCI DSS
// 10.5.1: at least a year). The audit_retention_days setting refuses lower
// values; the purger repeats the check so a value that got around it deletes
// nothing.
const MinAuditRetentionDays = 365

// PurgeOptions configures a Purger.
type PurgeOptions struct {
	Logs *store.DB // the logs database
	Main *store.DB // the application database (audit log)
	// Retention reads an integer setting by key: log_retention_error_days,
	// log_retention_warn_days, log_max_rows, log_max_mb and audit_retention_days.
	Retention func(ctx context.Context, key string) (int, error)
	OrgID     string // the org whose audit log is trimmed; default store.DefaultOrgID
	Batch     int    // rows deleted per transaction; default 1000
	Now       func() time.Time
}

// PurgeResult counts what a run deleted.
type PurgeResult struct {
	Errors, Warnings int // by age; warnings include any info and debug rows
	Trimmed          int // beyond the row cap
	Audit            int // audit entries older than the retention
}

// Purger deletes old log rows by level and age, trims the table to its cap,
// oldest first, and applies the audit log's retention. Rows go in batches, so a
// big backlog never holds the database for long.
type Purger struct {
	o PurgeOptions

	mu       sync.Mutex
	lastSize time.Time // when Trim last measured the table's size
}

// sizeEvery is how often Trim measures the table's size (which reads the table),
// between the hourly runs that always do.
const sizeEvery = 30 * time.Second

// NewPurger builds a Purger.
func NewPurger(o PurgeOptions) *Purger {
	if o.Batch <= 0 {
		o.Batch = 1000
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.OrgID == "" {
		o.OrgID = store.DefaultOrgID
	}
	return &Purger{o: o}
}

// Run makes one pass.
func (p *Purger) Run(ctx context.Context) (PurgeResult, error) {
	var res PurgeResult
	errDays, err := p.o.Retention(ctx, "log_retention_error_days")
	if err != nil {
		return res, err
	}
	warnDays, err := p.o.Retention(ctx, "log_retention_warn_days")
	if err != nil {
		return res, err
	}
	now := p.o.Now()
	if res.Errors, err = p.purgeLevels(ctx, now.AddDate(0, 0, -errDays), "error"); err != nil {
		return res, err
	}
	if res.Warnings, err = p.purgeLevels(ctx, now.AddDate(0, 0, -warnDays), "warn", "info", "debug"); err != nil {
		return res, err
	}
	if res.Trimmed, err = p.trim(ctx, true); err != nil {
		return res, err
	}
	if res.Audit, err = p.purgeAudit(ctx, now); err != nil {
		return res, err
	}
	return res, ctx.Err()
}

// Trim applies the caps on the table, the rows and the size, oldest rows
// first, and returns how many rows it deleted. It is cheap enough to call as
// rows arrive: the size, which needs a read of the table, is measured at most
// every half minute.
func (p *Purger) Trim(ctx context.Context) (int, error) { return p.trim(ctx, false) }

func (p *Purger) trim(ctx context.Context, forceSize bool) (int, error) {
	maxRows, err := p.o.Retention(ctx, "log_max_rows")
	if err != nil {
		return 0, err
	}
	total := 0
	for ctx.Err() == nil {
		var n int
		if err := p.o.Logs.Write(ctx, func(tx store.Tx) (err error) { n, err = store.TrimLogs(ctx, tx, maxRows, p.o.Batch); return }); err != nil {
			return total, err
		}
		if n == 0 {
			break
		}
		total += n
	}
	p.mu.Lock()
	now := p.o.Now()
	measure := forceSize || p.lastSize.IsZero() || now.Sub(p.lastSize) >= sizeEvery
	if measure {
		p.lastSize = now
	}
	p.mu.Unlock()
	if !measure {
		return total, ctx.Err()
	}
	mb, err := p.o.Retention(ctx, "log_max_mb")
	if err != nil || mb <= 0 {
		return total, err
	}
	n, err := p.trimToSize(ctx, int64(mb)<<20)
	return total + n, err
}

// trimToSize deletes the oldest rows until the table is at 90% of the cap. The
// row size is estimated from the average, so a run may fall a little short or
// go a little over; the next run corrects it.
func (p *Purger) trimToSize(ctx context.Context, capBytes int64) (int, error) {
	var size int64
	var rows int
	if err := p.o.Logs.Read(ctx, func(q store.Querier) (err error) {
		if size, err = store.LogsBytes(ctx, q); err != nil {
			return err
		}
		rows, err = store.CountLogs(ctx, q)
		return
	}); err != nil {
		return 0, err
	}
	if size <= capBytes || rows == 0 {
		return 0, nil
	}
	avg := max(size/int64(rows), 1)
	drop := int((size-capBytes*9/10)/avg) + 1
	keep := max(rows-drop, 0)
	total := 0
	for ctx.Err() == nil {
		var n int
		if err := p.o.Logs.Write(ctx, func(tx store.Tx) (err error) { n, err = store.TrimLogs(ctx, tx, keep, p.o.Batch); return }); err != nil {
			return total, err
		}
		if n == 0 {
			break
		}
		total += n
	}
	return total, ctx.Err()
}

func (p *Purger) purgeLevels(ctx context.Context, before time.Time, levels ...string) (int, error) {
	total := 0
	for _, level := range levels {
		for ctx.Err() == nil {
			var n int
			if err := p.o.Logs.Write(ctx, func(tx store.Tx) (err error) { n, err = store.PurgeLogs(ctx, tx, level, before, p.o.Batch); return }); err != nil {
				return total, err
			}
			if n == 0 {
				break
			}
			total += n
		}
	}
	return total, nil
}

// purgeAudit deletes audit entries older than audit_retention_days. 0 keeps
// them forever, and so does any value below the PCI minimum.
func (p *Purger) purgeAudit(ctx context.Context, now time.Time) (int, error) {
	days, err := p.o.Retention(ctx, "audit_retention_days")
	if err != nil || days == 0 || days < MinAuditRetentionDays {
		return 0, nil
	}
	before := now.AddDate(0, 0, -days)
	total := 0
	for ctx.Err() == nil {
		var n int
		if err := p.o.Main.Write(ctx, func(tx store.Tx) (err error) {
			n, err = store.PurgeAudit(ctx, tx, p.o.OrgID, before, p.o.Batch)
			return
		}); err != nil {
			return total, err
		}
		if n == 0 {
			break
		}
		total += n
	}
	if total > 0 {
		err = p.o.Main.Write(ctx, func(tx store.Tx) error {
			return audit.Record(ctx, tx, audit.Entry{OrgID: p.o.OrgID, Actor: audit.System, Action: "audit.purge", Target: "audit_log",
				After: map[string]any{"deleted": total, "older_than_days": days}})
		})
	}
	return total, err
}
