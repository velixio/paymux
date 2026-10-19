package jobs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/logctx"
	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func logQueue(db *store.DB, c *clock, buf *bytes.Buffer) *jobs.Queue {
	return jobs.New(db, jobs.Options{Workers: 1, Lease: 5 * time.Minute, Clock: c.Now, Jitter: func() float64 { return 0 },
		Logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))})
}

// A failed job is logged with its id and kind: a warning while it will be
// retried, an error when it is dead.
func TestFailedJobsAreLoggedWithTheirIDs(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		if err := db.Migrate(ctx, migrations.FS); err != nil {
			t.Fatal(err)
		}
		c := &clock{t: t0}
		var buf bytes.Buffer
		q := logQueue(db, c, &buf)
		q.Register("flaky", func(context.Context, json.RawMessage) error { return errors.New("provider said no") })
		q.Register("doomed", func(context.Context, json.RawMessage) error { return jobs.Permanent(errors.New("cannot ever work")) })
		enqueue(t, db, q, jobs.Job{Kind: "flaky", MaxAttempts: 3})
		enqueue(t, db, q, jobs.Job{Kind: "doomed"})
		runOnce(t, q)
		runOnce(t, q)
		var recs []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err == nil {
				recs = append(recs, m)
			}
		}
		var warn, errRec map[string]any
		for _, m := range recs {
			switch {
			case m["job_kind"] == "flaky" && m["level"] == "WARN":
				warn = m
			case m["job_kind"] == "doomed" && m["level"] == "ERROR":
				errRec = m
			}
		}
		if warn == nil || errRec == nil {
			t.Fatalf("missing failure records:\n%s", buf.String())
		}
		if !strings.HasPrefix(warn["job_id"].(string), "job_") || warn["err"] != "provider said no" || warn["attempt"] != float64(1) {
			t.Errorf("warn: %v", warn)
		}
		if errRec["err"] != "cannot ever work" || errRec["kind"] != "job" {
			t.Errorf("error: %v", errRec)
		}
	})
}

// Work done inside a job (a provider call, a log line) can tell which job it
// belongs to.
func TestJobHandlersGetTheirIDsInTheContext(t *testing.T) {
	db, c := newMigrated(t)
	q := newQueue(db, c)
	var got map[string]string
	q.Register("ctx.kind", func(ctx context.Context, _ json.RawMessage) error { got = logctx.IDs(ctx); return nil })
	enqueue(t, db, q, jobs.Job{Kind: "ctx.kind"})
	runOnce(t, q)
	if !strings.HasPrefix(got["job_id"], "job_") || got["job_kind"] != "ctx.kind" {
		t.Errorf("%v", got)
	}
}
