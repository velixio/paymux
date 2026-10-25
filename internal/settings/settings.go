// Package settings resolves policy values: app override, then org value, then
// the built-in default (spec section 8a). It is the only way to read a policy
// value; the settings-via-resolver seam rule enforces that.
package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/store"
)

var (
	// ErrUnknownKey means the key is not a defined setting.
	ErrUnknownKey = errors.New("settings: unknown key")
	// ErrInvalid means the value violates the setting's allowed range.
	ErrInvalid = errors.New("settings: invalid value")
	// ErrUnknownApp means the app does not exist in the given org.
	ErrUnknownApp = errors.New("settings: unknown app")
	// ErrWrongLevel means the key cannot be set at that level (org or app).
	ErrWrongLevel = errors.New("settings: key cannot be set at this level")
)

// Source says where a resolved value came from.
type Source string

const (
	SourceApp     Source = "app"     // overridden on the app
	SourceOrg     Source = "org"     // inherited from the org
	SourceDefault Source = "default" // built-in default
)

// Value is a resolved setting.
type Value struct {
	Raw    json.RawMessage
	Source Source
}

// Decode unmarshals the value into dst.
func (v Value) Decode(dst any) error { return json.Unmarshal(v.Raw, dst) }

// DefaultCacheTTL is how long a resolved value is reused. Changes made through
// this Service show at once (every Set clears the cache); a change made by
// another process, such as a second node on a shared PostgreSQL, shows within
// this time.
const DefaultCacheTTL = 3 * time.Second

// Service reads and writes settings.
type Service struct {
	db  *store.DB
	now func() time.Time

	mu    sync.Mutex
	ttl   time.Duration
	gen   uint64 // bumped by every change; a load only fills the cache if it did not move
	cache map[string]cacheEntry
}

type cacheEntry struct {
	v  Value
	at time.Time
}

// New returns a Service on db with the default cache TTL.
func New(db *store.DB) *Service {
	return &Service{db: db, now: time.Now, ttl: DefaultCacheTTL, cache: map[string]cacheEntry{}}
}

// SetCacheTTL sets how long resolved values are reused; zero or less turns the
// cache off.
func (s *Service) SetCacheTTL(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ttl = d
	s.gen++
	clear(s.cache)
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// cached returns the value under k if it is fresh, else loads and remembers
// it. A value loaded while a change was being made is returned but not kept.
func (s *Service) cached(k string, load func() (Value, error)) (Value, error) {
	s.mu.Lock()
	if s.ttl <= 0 {
		s.mu.Unlock()
		return load()
	}
	now := s.now()
	if e, ok := s.cache[k]; ok && now.Sub(e.at) < s.ttl {
		s.mu.Unlock()
		return e.v.clone(), nil
	}
	gen := s.gen
	s.mu.Unlock()

	v, err := load()
	if err != nil {
		return Value{}, err
	}
	s.mu.Lock()
	if s.gen == gen && s.ttl > 0 {
		s.cache[k] = cacheEntry{v: v.clone(), at: now}
	}
	s.mu.Unlock()
	return v, nil
}

func (v Value) clone() Value {
	return Value{Raw: append(json.RawMessage(nil), v.Raw...), Source: v.Source}
}

// invalidate drops every cached value. It runs after a change has committed.
func (s *Service) invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	clear(s.cache)
}

// Invalidate drops every cached value. A caller that changed a stored setting
// without going through this Service (a store helper that rewrites a value, as
// renaming a theme does) calls it so the next read sees the change at once.
func (s *Service) Invalidate() { s.invalidate() }

// ResolveCachedWith is Resolve for a caller that already holds a reader (often
// a write transaction on another table): a fresh cached value needs no query,
// otherwise the value is read through q. Unlike ResolveWith it may be up to
// the cache TTL old and does not see uncommitted writes, so use it for policy
// reads, never to read back what the same transaction just set.
func (s *Service) ResolveCachedWith(ctx context.Context, q store.Queryer, appID, key string) (Value, error) {
	return s.cached("a\x00"+appID+"\x00"+key, func() (Value, error) { return ResolveWith(ctx, q, appID, key) })
}

// ResolveOrgCachedWith is the org-level ResolveCachedWith.
func (s *Service) ResolveOrgCachedWith(ctx context.Context, q store.Queryer, orgID, key string) (Value, error) {
	return s.cached("o\x00"+orgID+"\x00"+key, func() (Value, error) { return ResolveOrgWith(ctx, q, orgID, key) })
}

// Resolve returns the effective value of key for an app. An empty appID
// resolves at the org level.
func (s *Service) Resolve(ctx context.Context, appID, key string) (Value, error) {
	var v Value
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		v, err = s.ResolveCachedWith(ctx, q, appID, key)
		return
	})
	return v, err
}

// ResolveOrg returns the org-level value of key (org value, else default).
func (s *Service) ResolveOrg(ctx context.Context, orgID, key string) (Value, error) {
	var v Value
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		v, err = s.ResolveOrgCachedWith(ctx, q, orgID, key)
		return
	})
	return v, err
}

// ResolveWith resolves using an existing reader, such as the Tx of a write
// transaction, so the read sees that transaction's own writes.
func ResolveWith(ctx context.Context, q store.Queryer, appID, key string) (Value, error) {
	if _, ok := Lookup(key); !ok {
		return Value{}, fmt.Errorf("%w: %q", ErrUnknownKey, key)
	}
	if appID == "" {
		orgID, err := store.FirstOrgID(ctx, q)
		if err != nil {
			return Value{}, err
		}
		return ResolveOrgWith(ctx, q, orgID, key)
	}
	app, err := store.GetApp(ctx, q, appID)
	if err != nil {
		return Value{}, fmt.Errorf("settings: app %s: %w", appID, err)
	}
	if raw, found, err := store.GetAppSetting(ctx, q, appID, key); err != nil {
		return Value{}, err
	} else if found {
		return Value{Raw: json.RawMessage(raw), Source: SourceApp}, nil
	}
	return ResolveOrgWith(ctx, q, app.OrgID, key)
}

// ResolveOrgWith resolves the org-level value (org value, else default) using an
// existing reader.
func ResolveOrgWith(ctx context.Context, q store.Queryer, orgID, key string) (Value, error) {
	def, ok := Lookup(key)
	if !ok {
		return Value{}, fmt.Errorf("%w: %q", ErrUnknownKey, key)
	}
	raw, found, err := store.GetOrgSetting(ctx, q, orgID, key)
	if err != nil {
		return Value{}, err
	}
	if found {
		return Value{Raw: json.RawMessage(raw), Source: SourceOrg}, nil
	}
	return Value{Raw: json.RawMessage(def.Default), Source: SourceDefault}, nil
}

// SeedOrg copies every built-in default into the org's values, leaving any
// existing value untouched. It is safe to run on every start.
func (s *Service) SeedOrg(ctx context.Context, orgID string) error {
	defer s.invalidate()
	return s.db.Write(ctx, func(tx store.Tx) error {
		for _, d := range defs {
			if err := store.SeedOrgSetting(ctx, tx, orgID, d.Key, d.Default, s.now()); err != nil {
				return err
			}
		}
		return nil
	})
}

func prepare(key string, raw json.RawMessage, level Level) (string, error) {
	def, ok := Lookup(key)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownKey, key)
	}
	if def.Levels&level == 0 {
		return "", fmt.Errorf("%w: %q", ErrWrongLevel, key)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", fmt.Errorf("%w: %s: not JSON", ErrInvalid, key)
	}
	if err := def.Validate(buf.Bytes()); err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrInvalid, key, err)
	}
	return buf.String(), nil
}

// SetOrg validates and stores an org value and audits the change.
func (s *Service) SetOrg(ctx context.Context, actor audit.Actor, orgID, key string, raw json.RawMessage) error {
	val, err := prepare(key, raw, LevelOrg)
	if err != nil {
		return err
	}
	defer s.invalidate()
	return s.db.Write(ctx, func(tx store.Tx) error {
		before, err := ResolveOrgWith(ctx, tx, orgID, key)
		if err != nil {
			return err
		}
		if err := store.SetOrgSetting(ctx, tx, orgID, key, val, s.now()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: orgID, Actor: actor, Action: "setting.update",
			Target: "org:" + orgID + "/" + key, Before: before.Raw, After: json.RawMessage(val)})
	})
}

// SetApp validates and stores an app override and audits the change.
func (s *Service) SetApp(ctx context.Context, actor audit.Actor, orgID, appID, key string, raw json.RawMessage) error {
	val, err := prepare(key, raw, LevelApp)
	if err != nil {
		return err
	}
	defer s.invalidate()
	return s.db.Write(ctx, func(tx store.Tx) error {
		app, err := store.GetAppInOrg(ctx, tx, orgID, appID)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrUnknownApp, appID)
		}
		before, err := ResolveWith(ctx, tx, appID, key)
		if err != nil {
			return err
		}
		if err := store.SetAppSetting(ctx, tx, appID, key, val, s.now()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: app.OrgID, Actor: actor, Action: "setting.update",
			Target: "app:" + appID + "/" + key, Before: before.Raw, After: json.RawMessage(val)})
	})
}

// ResetApp removes an app override so the app inherits the org value again.
// It is a no-op (and writes no audit entry) when there is no override.
func (s *Service) ResetApp(ctx context.Context, actor audit.Actor, orgID, appID, key string) error {
	if _, ok := Lookup(key); !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKey, key)
	}
	defer s.invalidate()
	return s.db.Write(ctx, func(tx store.Tx) error {
		app, err := store.GetAppInOrg(ctx, tx, orgID, appID)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrUnknownApp, appID)
		}
		old, found, err := store.GetAppSetting(ctx, tx, appID, key)
		if err != nil || !found {
			return err
		}
		if err := store.DeleteAppSetting(ctx, tx, appID, key); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: app.OrgID, Actor: actor, Action: "setting.reset",
			Target: "app:" + appID + "/" + key, Before: json.RawMessage(old)})
	})
}
