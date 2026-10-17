package jobs

import "time"

const (
	backoffBase = 5 * time.Second
	backoffCap  = time.Hour
)

// Backoff returns 5s * 2^(attempt-1), capped at 1h, scaled by (1+jitter).
func Backoff(attempt int, jitter float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := backoffCap
	if attempt <= 13 { // 5s<<12 = 20480s > 1h, so larger shifts always cap
		d = min(backoffBase<<(attempt-1), backoffCap)
	}
	return time.Duration(float64(d) * (1 + jitter))
}
