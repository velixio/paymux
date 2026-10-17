// Package events defines the outbound domain event shape and the emitter
// contract. Core services emit events inside their write transaction; the
// outbound package persists them and schedules webhook delivery (outbox).
package events

import (
	"context"

	"github.com/paymux/paymux/internal/store"
)

// Event is one domain event about an object at a specific version.
type Event struct {
	OrgID         string
	AppID         string
	Mode          string // "test" | "live"
	Type          string // e.g. "payment.succeeded"
	ObjectType    string // e.g. "payment"
	ObjectID      string
	ObjectVersion int64
	Data          any // JSON-serialisable snapshot of the object
}

// Emitter records an event in the caller's transaction.
type Emitter interface {
	Emit(ctx context.Context, tx store.Tx, e Event) error
}

// EmitterFunc adapts a function to Emitter.
type EmitterFunc func(ctx context.Context, tx store.Tx, e Event) error

// Emit calls f.
func (f EmitterFunc) Emit(ctx context.Context, tx store.Tx, e Event) error { return f(ctx, tx, e) }
