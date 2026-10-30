package jobs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/velixio/paymux/internal/store"
)

// Schedule enqueues a job of Kind repeatedly. Set exactly one of Every
// (fixed interval) or DailyAt ("HH:MM" wall-clock time in Location).
type Schedule struct {
	Name, Kind string
	Every      time.Duration
	DailyAt    string
	Location   *time.Location
}

func parseDailyAt(s string) (hour, min int, err error) {
	var tail string
	if n, _ := fmt.Sscanf(s, "%d:%d%s", &hour, &min, &tail); n != 2 || len(s) != 5 || s[2] != ':' {
		return 0, 0, fmt.Errorf("DailyAt %q must be HH:MM", s)
	}
	if hour < 0 || hour > 23 || min < 0 || min > 59 {
		return 0, 0, fmt.Errorf("DailyAt %q out of range", s)
	}
	return hour, min, nil
}

func (s Schedule) validate() error {
	switch {
	case s.Name == "":
		return errors.New("schedule name is required")
	case s.Kind == "":
		return fmt.Errorf("schedule %q: kind is required", s.Name)
	case s.Every != 0 && s.DailyAt != "":
		return fmt.Errorf("schedule %q: set exactly one of Every or DailyAt, got both", s.Name)
	case s.Every == 0 && s.DailyAt == "":
		return fmt.Errorf("schedule %q: set exactly one of Every or DailyAt, got neither", s.Name)
	case s.Every < 0:
		return fmt.Errorf("schedule %q: Every must be positive", s.Name)
	}
	if s.DailyAt != "" {
		if s.Location == nil {
			return fmt.Errorf("schedule %q: Location is required with DailyAt", s.Name)
		}
		if _, _, err := parseDailyAt(s.DailyAt); err != nil {
			return fmt.Errorf("schedule %q: %w", s.Name, err)
		}
	}
	return nil
}

// AddSchedule registers s. Schedules are evaluated at the start of RunOnce.
func (q *Queue) AddSchedule(s Schedule) error {
	if err := s.validate(); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range q.schedules {
		if e.Name == s.Name {
			return fmt.Errorf("schedule %q already added", s.Name)
		}
	}
	q.schedules = append(q.schedules, s)
	return nil
}

// NextRun returns the first run strictly after the given instant. s must be valid.
//
// Daily schedules fire at the wall-clock time in Location: a time skipped by a
// spring-forward gap runs at the equivalent instant after the gap (02:30 becomes
// 03:30), and a time repeated by a fall-back runs once, at its first occurrence.
func NextRun(s Schedule, after time.Time) time.Time {
	if s.Every > 0 {
		return after.Add(s.Every)
	}
	hour, min, _ := parseDailyAt(s.DailyAt)
	local := after.In(s.Location)
	y, m, d := local.Date()
	for i := 0; i < 3; i++ {
		c := time.Date(y, m, d+i, hour, min, 0, 0, s.Location)
		if c.Hour() != hour || c.Minute() != min {
			// Skipped by a spring-forward gap; time.Date normalised it to an
			// instant before the gap. Shift by the offset change.
			_, offBefore := c.Zone()
			_, offAfter := c.Add(3 * time.Hour).Zone()
			c = c.Add(time.Duration(offAfter-offBefore) * time.Second)
		}
		// Repeated wall time: prefer the earliest occurrence.
		for {
			p := c.Add(-time.Hour)
			if p.Hour() == hour && p.Minute() == min && p.Day() == c.Day() {
				c = p
				continue
			}
			break
		}
		if c.After(after) {
			return c
		}
	}
	return after.Add(24 * time.Hour) // unreachable for sane zones
}

// enqueueDueSchedules enqueues a job for every due schedule and advances its
// next run, all in one transaction. A schedule seen for the first time first
// runs at its next slot, not immediately. After downtime a schedule fires once
// and resumes from now, so there is no catch-up burst.
func (q *Queue) enqueueDueSchedules(ctx context.Context) error {
	q.mu.RLock()
	scheds := append([]Schedule(nil), q.schedules...)
	q.mu.RUnlock()
	if len(scheds) == 0 {
		return nil
	}
	sort.Slice(scheds, func(i, j int) bool { return scheds[i].Name < scheds[j].Name }) // stable lock order
	now := q.opts.Clock()
	return q.db.Write(ctx, func(tx store.Tx) error {
		for _, s := range scheds {
			next := NextRun(s, now)
			due, runAt, err := q.db.ClaimDueSchedule(ctx, tx, s.Name, now, next)
			if err != nil {
				return err
			}
			if !due {
				continue
			}
			err = q.Enqueue(ctx, tx, Job{
				Kind:      s.Kind,
				Payload:   struct{}{},
				RunAt:     runAt,
				DedupeKey: fmt.Sprintf("sched:%s:%d", s.Name, runAt.UnixMilli()),
			})
			if err != nil {
				return err
			}
			if err := q.db.SetScheduleNext(ctx, tx, s.Name, next, now); err != nil {
				return err
			}
		}
		return nil
	})
}
