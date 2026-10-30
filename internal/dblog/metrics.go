package dblog

import "github.com/velixio/paymux/internal/metrics"

// RegisterMetrics exposes the writer's counters: rows stored, rows dropped
// because the queue was full, rows lost to a failed write, and the queue depth.
func (r *Recorder) RegisterMetrics(reg *metrics.Registry) {
	stored := reg.Counter("paymux_logs_stored_total", "Error and warning log rows written to the database.")
	dropped := reg.Counter("paymux_logs_dropped_total", "Log rows dropped because the database log queue was full.")
	failed := reg.Counter("paymux_logs_write_failures_total", "Log rows lost because writing them to the database failed.")
	suppressed := reg.Counter("paymux_logs_suppressed_total", "Rejected-request log rows not written because their source had its share in the window.")
	reg.GaugeFunc("paymux_logs_queue_depth", "Log rows waiting to be written.", func() float64 { return float64(r.Stats().Queued) })
	reg.OnScrape(func() {
		s := r.Stats()
		stored.Set(float64(s.Stored))
		dropped.Set(float64(s.Dropped))
		failed.Set(float64(s.Failed))
		suppressed.Set(float64(s.Suppressed))
	})
}
