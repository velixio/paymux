// Package dblog keeps errors and warnings in the database for later review:
// application log records at warn level and above, failed provider API calls
// (with redacted request and response bodies), 5xx and rejected API/webhook
// requests, and failed jobs. Info and debug records are not stored.
//
// Nothing reaches the table unredacted: every record passes through
// internal/redact (known secret values, credential and personal-data field
// names, value patterns), and rows carry identifiers, never customer details.
//
// Writing never slows the caller: rows go to a bounded queue and a single
// goroutine inserts them in batches. When the queue is full a row is dropped
// and counted. A failure to write is reported on stdout only and never
// becomes a log record itself, so logging cannot recurse into itself.
package dblog

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/redact"
	"github.com/paymux/paymux/internal/store"
)

// BodyCap is the most of a request or response body that is stored, after
// redaction.
const BodyCap = 8 << 10

// Options configures a Recorder.
type Options struct {
	DB       *store.DB // the logs database (store.DB.OpenLogs)
	Redactor *redact.Redactor
	// Level is the lowest level stored; nil means warn. Lower it only on purpose:
	// info and debug records are plentiful.
	Level slog.Leveler
	// Stdout receives the recorder's own messages (write failures, dropped
	// counts). It must not be a logger that tees into this Recorder.
	Stdout      *slog.Logger
	QueueSize   int           // default 4096
	BatchSize   int           // default 100
	FlushEvery  time.Duration // default 1s
	ReportEvery time.Duration // how often a "dropped N" line is written; default 1m
	Now         func() time.Time
	// Write replaces the database insert (tests).
	Write func(ctx context.Context, rows []store.LogRow) error
	// Trim, when set, is called by the writer after every TrimEvery rows stored
	// (and at least every TrimInterval while rows arrive), so the caps on the
	// table hold between the hourly purges.
	Trim         func(ctx context.Context) error
	TrimEvery    int           // default 1000
	TrimInterval time.Duration // default 30s
	// Tee, when set, receives every batch the writer takes from the queue, for
	// shipping the same redacted rows elsewhere. It must not block.
	Tee func(rows []store.LogRow)
}

// Stats are the counters of a Recorder.
type Stats struct {
	Stored, Dropped, Failed uint64
	// Suppressed counts rejected-request rows not written because their source
	// had already had its share in the window.
	Suppressed uint64
	Queued     int
}

// Recorder turns log records, provider calls and HTTP failures into redacted
// rows and writes them asynchronously.
type Recorder struct {
	o     Options
	red   *redact.Redactor
	level atomic.Int64
	queue chan store.LogRow
	flush chan chan struct{}
	stop  chan struct{}
	quit  chan struct{} // closed by Close: ends the report loop
	done  chan struct{}

	closed  atomic.Bool
	running atomic.Bool
	started sync.Once
	stopped sync.Once

	throttle                *throttle
	stored, dropped, failed atomic.Uint64
	suppressed              atomic.Uint64
	trim                    atomic.Pointer[func(ctx context.Context) error]
	sinceTrim               int
	lastTrim                time.Time
	lastReport              atomic.Int64 // unix nanos of the last "dropped N" line
	reported                atomic.Uint64
	lastFailLog             time.Time
}

// New builds a Recorder. Call Start to begin writing.
func New(o Options) *Recorder {
	if o.QueueSize <= 0 {
		o.QueueSize = 4096
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 100
	}
	if o.FlushEvery <= 0 {
		o.FlushEvery = time.Second
	}
	if o.TrimEvery <= 0 {
		o.TrimEvery = 1000
	}
	if o.TrimInterval <= 0 {
		o.TrimInterval = 30 * time.Second
	}
	if o.ReportEvery <= 0 {
		o.ReportEvery = time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Stdout == nil {
		o.Stdout = slog.New(slog.DiscardHandler)
	}
	if o.Redactor == nil {
		o.Redactor = redact.New()
	}
	r := &Recorder{o: o, red: o.Redactor, throttle: newThrottle(), queue: make(chan store.LogRow, o.QueueSize), flush: make(chan chan struct{}),
		stop: make(chan struct{}), quit: make(chan struct{}), done: make(chan struct{})}
	lv := slog.Level(slog.LevelWarn)
	if o.Level != nil {
		lv = o.Level.Level()
	}
	r.level.Store(int64(lv))
	if o.Trim != nil {
		r.SetTrim(o.Trim)
	}
	r.lastReport.Store(o.Now().UnixNano())
	if o.Write == nil {
		db := o.DB
		r.o.Write = func(ctx context.Context, rows []store.LogRow) error {
			return db.Write(ctx, func(tx store.Tx) error { return store.InsertLogs(ctx, tx, rows) })
		}
	}
	return r
}

// SetTrim sets (or, with nil, removes) the function that applies the caps on the
// table while rows arrive.
func (r *Recorder) SetTrim(fn func(ctx context.Context) error) {
	if fn == nil {
		r.trim.Store(nil)
		return
	}
	r.trim.Store(&fn)
}

// Redactor returns the redactor the rows pass through.
func (r *Recorder) Redactor() *redact.Redactor { return r.red }

// SetLevel changes the lowest level stored.
func (r *Recorder) SetLevel(l slog.Level) { r.level.Store(int64(l)) }

// Level returns the lowest level stored.
func (r *Recorder) Level() slog.Level { return slog.Level(r.level.Load()) }

// Start launches the writer goroutine (once).
func (r *Recorder) Start(context.Context) {
	r.started.Do(func() {
		r.running.Store(true)
		go r.run()
		go r.reportLoop()
	})
}

// Close writes what is queued and stops the writer. It is safe to call twice;
// rows emitted afterwards are dropped silently.
func (r *Recorder) Close() {
	r.stopped.Do(func() {
		r.closed.Store(true)
		close(r.quit)
		if !r.running.Load() {
			return
		}
		select {
		case r.stop <- struct{}{}:
		case <-r.done:
		case <-time.After(15 * time.Second):
			return
		}
		select {
		case <-r.done:
		case <-time.After(15 * time.Second):
		}
	})
}

// Flush waits until everything queued so far has been written.
func (r *Recorder) Flush(ctx context.Context) {
	if !r.running.Load() {
		return
	}
	ack := make(chan struct{})
	select {
	case r.flush <- ack:
		select {
		case <-ack:
		case <-ctx.Done():
		}
	case <-r.done:
	case <-ctx.Done():
	}
}

// Stats returns the counters.
func (r *Recorder) Stats() Stats {
	return Stats{Stored: r.stored.Load(), Dropped: r.dropped.Load(), Failed: r.failed.Load(), Suppressed: r.suppressed.Load(), Queued: len(r.queue)}
}

// Emit queues a finished, already redacted row. It never blocks: when the queue
// is full the row is dropped and counted.
func (r *Recorder) Emit(row store.LogRow) {
	if r.closed.Load() {
		return
	}
	if row.ID == "" {
		row.ID = ids.New("log")
	}
	if row.At.IsZero() {
		row.At = r.o.Now()
	}
	select {
	case r.queue <- row:
	default:
		r.dropped.Add(1)
	}
}

func (r *Recorder) run() {
	defer close(r.done)
	batch := make([]store.LogRow, 0, r.o.BatchSize)
	write := func() {
		if len(batch) > 0 {
			r.write(batch)
			batch = batch[:0]
		}
	}
	add := func(row store.LogRow) {
		batch = append(batch, row)
		if len(batch) >= r.o.BatchSize {
			write()
		}
	}
	drain := func() {
		for {
			select {
			case row := <-r.queue:
				add(row)
			default:
				write()
				return
			}
		}
	}
	tick := time.NewTicker(r.o.FlushEvery)
	defer tick.Stop()
	for {
		select {
		case row := <-r.queue:
			add(row)
		case <-tick.C:
			write()
		case ack := <-r.flush:
			drain()
			close(ack)
		case <-r.stop:
			drain()
			return
		}
	}
}

func (r *Recorder) write(rows []store.LogRow) {
	if r.o.Tee != nil {
		func() {
			defer func() { _ = recover() }()
			r.o.Tee(rows)
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = panicError{p}
			}
		}()
		return r.o.Write(ctx, rows)
	}()
	if err != nil {
		r.failed.Add(uint64(len(rows)))
		if now := r.o.Now(); r.lastFailLog.IsZero() || now.Sub(r.lastFailLog) >= 10*time.Second {
			r.lastFailLog = now
			// Stdout only: a failure to store logs is never itself stored.
			r.o.Stdout.Error("could not store log rows", "rows", len(rows), "err", err.Error())
		}
		return
	}
	r.stored.Add(uint64(len(rows)))
	r.maybeTrim(len(rows))
}

// maybeTrim applies the table's caps once enough rows have come in (or enough
// time has passed). It runs on the writer goroutine, so it needs no lock.
func (r *Recorder) maybeTrim(added int) {
	fn := r.trim.Load()
	if fn == nil {
		return
	}
	r.sinceTrim += added
	now := r.o.Now()
	if r.sinceTrim < r.o.TrimEvery && now.Sub(r.lastTrim) < r.o.TrimInterval {
		return
	}
	r.sinceTrim, r.lastTrim = 0, now
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer func() { _ = recover() }()
	if err := (*fn)(ctx); err != nil {
		r.o.Stdout.Error("could not trim the log table", "err", err.Error())
	}
}

type panicError struct{ v any }

func (p panicError) Error() string { return "panic while storing logs" }

// reportLoop reports drops on its own clock: the writer may be the thing that
// is stuck, and a flood may be over before the next row is emitted.
func (r *Recorder) reportLoop() {
	every := max(r.o.ReportEvery/2, time.Millisecond)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.reportDrops()
		case <-r.quit:
			r.reportDrops()
			return
		}
	}
}

// reportDrops writes a "dropped N" line to stdout, at most every ReportEvery.
func (r *Recorder) reportDrops() {
	now := r.o.Now().UnixNano()
	last := r.lastReport.Load()
	if now-last < int64(r.o.ReportEvery) || !r.lastReport.CompareAndSwap(last, now) {
		return
	}
	total := r.dropped.Load()
	if prev := r.reported.Swap(total); total > prev {
		r.o.Stdout.Warn("log rows dropped: the database log queue is full", "dropped", total-prev, "dropped_total", total)
	}
}
