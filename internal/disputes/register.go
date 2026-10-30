package disputes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/velixio/paymux/internal/jobs"
)

// Job kinds of the dispute schedules.
const (
	JobPoll      = "disputes.poll"
	JobDeadlines = "disputes.deadlines"
)

// Register installs the polling and deadline-alert jobs on q.
func (s *Service) Register(q *jobs.Queue) error {
	q.Register(JobPoll, func(ctx context.Context, _ json.RawMessage) error { return s.Poll(ctx) })
	q.Register(JobDeadlines, func(ctx context.Context, _ json.RawMessage) error { return s.CheckDeadlines(ctx) })
	if err := q.AddSchedule(jobs.Schedule{Name: JobPoll, Kind: JobPoll, Every: 30 * time.Minute}); err != nil {
		return err
	}
	return q.AddSchedule(jobs.Schedule{Name: JobDeadlines, Kind: JobDeadlines, Every: 15 * time.Minute})
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
