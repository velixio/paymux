package connector

import (
	"context"
	"time"
)

// KV is the small per-account key/value store connectors use for any state
// (cached provider plans, tokens). Connectors never touch the DB.
type KV interface {
	// Get returns the value and whether it exists (and has not expired).
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores the value; ttl <= 0 means no expiry.
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}
