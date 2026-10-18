// Package logctx carries the identifiers of the work in progress (request id,
// job id, org, app, payment, attempt, ...) through a context, so that a log
// record or a failed provider call written deep inside can say which request
// or job it belongs to without every function passing them along.
//
// A request or a job starts a scope with Begin; code that learns an id calls
// Note (or With) and every later record of that request carries it.
package logctx

import (
	"context"
	"sync"
)

type key struct{}

type scope struct {
	mu sync.Mutex
	m  map[string]string
}

// Begin starts a fresh scope: the ids of an enclosing one are not inherited
// and not changed.
func Begin(ctx context.Context) context.Context {
	return context.WithValue(ctx, key{}, &scope{m: map[string]string{}})
}

func from(ctx context.Context) *scope {
	s, _ := ctx.Value(key{}).(*scope)
	return s
}

// With records ids (key, value, key, value, ...) in the scope of ctx, starting
// one if there is none, and returns the context. Empty values are ignored; a
// later value replaces an earlier one.
func With(ctx context.Context, kv ...string) context.Context {
	s := from(ctx)
	if s == nil {
		ctx = Begin(ctx)
		s = from(ctx)
	}
	s.set(kv)
	return ctx
}

// Note records ids in the scope of ctx, if it has one.
func Note(ctx context.Context, kv ...string) {
	if s := from(ctx); s != nil {
		s.set(kv)
	}
}

func (s *scope) set(kv []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] != "" && kv[i+1] != "" {
			s.m[kv[i]] = kv[i+1]
		}
	}
}

// IDs returns a copy of the ids in the scope of ctx.
func IDs(ctx context.Context) map[string]string {
	out := map[string]string{}
	if s := from(ctx); s != nil {
		s.mu.Lock()
		for k, v := range s.m {
			out[k] = v
		}
		s.mu.Unlock()
	}
	return out
}
