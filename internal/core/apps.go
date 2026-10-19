package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
)

// ErrConflict means the request collides with existing data (a duplicate slug).
var ErrConflict = errors.New("conflict")

// ErrUnauthenticated means an API key is malformed, unknown or revoked.
var ErrUnauthenticated = errors.New("unauthenticated")

// Bootstrap seeds the single org and its settings on first start. It is safe
// to run on every start and never overwrites edited values. It returns the
// org id.
func Bootstrap(ctx context.Context, db *store.DB, set *settings.Service) (string, error) {
	err := db.Write(ctx, func(tx store.Tx) error {
		return store.SeedOrg(ctx, tx, "Paymux", time.Now())
	})
	if err != nil {
		return "", err
	}
	if err := set.SeedOrg(ctx, store.DefaultOrgID); err != nil {
		return "", err
	}
	// List the built-in roles in the roles table (their actions come from code).
	var builtin []store.RoleRow
	for _, name := range store.BuiltinRoleNames {
		row := store.RoleRow{Name: name}
		for _, a := range authz.BuiltinRoles()[authz.Role(name)] {
			row.Actions = append(row.Actions, string(a))
		}
		builtin = append(builtin, row)
	}
	if err := db.SeedBuiltinRoles(ctx, builtin, time.Now()); err != nil {
		return "", err
	}
	return store.DefaultOrgID, nil
}

// OrgScope is the org a management call acts for. Every app, API key, domain
// and provider account mutation takes one and refuses objects of other orgs.
type OrgScope struct{ OrgID string }

func (sc OrgScope) validate() error {
	if sc.OrgID == "" {
		return newErr(ErrInvalid, "an org scope is required")
	}
	return nil
}

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// CreateAppInput is the input of CreateApp.
type CreateAppInput struct {
	Name, Slug, DefaultReturnURL string
}

// CreateApp creates an app in the org and audits it.
func (s *Service) CreateApp(ctx context.Context, actor audit.Actor, sc OrgScope, in CreateAppInput) (store.App, error) {
	if err := sc.validate(); err != nil {
		return store.App{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 200 {
		return store.App{}, newErr(ErrInvalid, "name is required (max 200 chars)")
	}
	if !slugRE.MatchString(in.Slug) {
		return store.App{}, newErr(ErrInvalid, "slug must be 1-40 chars of a-z, 0-9 and '-', starting with a letter or digit")
	}
	if in.DefaultReturnURL != "" && !validURL(in.DefaultReturnURL) {
		return store.App{}, newErr(ErrInvalid, "default_return_url must be an absolute http(s) URL")
	}
	var app store.App
	err := s.db.Write(ctx, func(tx store.Tx) error {
		org := sc.OrgID
		app = store.App{ID: ids.New("app"), OrgID: org, Name: in.Name, Slug: in.Slug, DefaultReturnURL: in.DefaultReturnURL,
			CreatedAt: s.nowUTC()}
		if err := store.InsertApp(ctx, tx, app); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return newErr(ErrConflict, "an app with slug %q already exists", in.Slug)
			}
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: org, Actor: actor, Action: "app.create", Target: app.ID,
			After: map[string]string{"name": app.Name, "slug": app.Slug}})
	})
	return app, err
}

// AppBySlug returns the app with the given slug.
func (s *Service) AppBySlug(ctx context.Context, sc OrgScope, slug string) (store.App, error) {
	if err := sc.validate(); err != nil {
		return store.App{}, err
	}
	var app store.App
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		app, err = store.GetAppBySlug(ctx, q, sc.OrgID, slug)
		return
	})
	return app, mapStoreErr(err, "app")
}

const (
	keyBodyLen   = 43 // 32 random bytes in base62
	keyPrefixLen = 8  // random characters kept in the lookup prefix
	touchEvery   = time.Minute
)

var keyRE = regexp.MustCompile(`^pmx_(test|live)_sk_[0-9A-Za-z]{43}$`)

func keyHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newKeyToken(mode string) string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("core: entropy source failed: " + err.Error())
	}
	body := new(big.Int).SetBytes(b[:]).Text(62)
	body = strings.Repeat("0", keyBodyLen-len(body)) + body
	return "pmx_" + mode + "_sk_" + body
}

func keyPrefix(token string) string {
	return token[:len("pmx_test_sk_")+keyPrefixLen]
}

// CreateAPIKey creates a key for the app in the given mode and returns the
// plaintext token. It is shown once: only its SHA-256 is stored.
func (s *Service) CreateAPIKey(ctx context.Context, actor audit.Actor, sc OrgScope, appID, mode string) (string, store.APIKey, error) {
	return s.CreateAPIKeyWith(ctx, actor, sc, appID, mode, KeyOptions{})
}

// KeyOptions are the optional properties of a new API key.
type KeyOptions struct {
	// Scopes limits the key (see authz.AllScopes). Nil means every scope.
	Scopes []string
	// Authorize, when set, is asked about the scopes the new key will really
	// have (those inherited on a rotation included) before it is made; an error
	// refuses the key. The dashboard uses it to stop a role handing out power it
	// does not hold.
	Authorize func(scopes []string) error
}

// ValidateScopes checks a scope list: nil is fine (all scopes); otherwise it
// must be non-empty, known and free of duplicates. The result is sorted in the
// canonical order of authz.AllScopes.
func ValidateScopes(scopes []string) ([]string, error) {
	if scopes == nil {
		return nil, nil
	}
	if len(scopes) == 0 {
		return nil, newErr(ErrInvalid, "a scoped key needs at least one scope; omit scopes for a full-access key")
	}
	var out []string
	for _, want := range authz.AllScopes {
		if slices.Contains(scopes, want) {
			out = append(out, want)
		}
	}
	for _, sc := range scopes {
		if !authz.ValidScope(sc) {
			return nil, newErr(ErrInvalid, "unknown scope %q (valid: %s)", sc, strings.Join(authz.AllScopes, ", "))
		}
	}
	return out, nil
}

// CreateAPIKeyWith is CreateAPIKey with options.
func (s *Service) CreateAPIKeyWith(ctx context.Context, actor audit.Actor, sc OrgScope, appID, mode string, opts KeyOptions) (string, store.APIKey, error) {
	if err := sc.validate(); err != nil {
		return "", store.APIKey{}, err
	}
	if mode != "test" && mode != "live" {
		return "", store.APIKey{}, newErr(ErrInvalid, "mode must be test or live")
	}
	scopes, err := ValidateScopes(opts.Scopes)
	if err != nil {
		return "", store.APIKey{}, err
	}
	token := newKeyToken(mode)
	var key store.APIKey
	err = s.db.Write(ctx, func(tx store.Tx) error {
		app, err := store.GetAppInOrg(ctx, tx, sc.OrgID, appID)
		if err != nil {
			return mapStoreErr(err, "app")
		}
		key = store.APIKey{ID: ids.New("key"), OrgID: app.OrgID, AppID: app.ID, Mode: mode, Prefix: keyPrefix(token),
			Hash: keyHash(token), CreatedAt: s.nowUTC(), Scopes: scopes}
		if opts.Authorize != nil {
			if err := opts.Authorize(key.Scopes); err != nil {
				return err
			}
		}
		if err := store.InsertAPIKey(ctx, tx, key); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: app.OrgID, Actor: actor, Action: "api_key.create", Target: key.ID,
			After: map[string]any{"app_id": app.ID, "mode": mode, "prefix": key.Prefix, "scopes": scopeLabel(scopes)}})
	})
	if err != nil {
		return "", store.APIKey{}, err
	}
	return token, key, nil
}

// scopeLabel renders scopes for the audit log: "all" for a full-access key.
func scopeLabel(scopes []string) string {
	if scopes == nil {
		return "all"
	}
	return strings.Join(scopes, ",")
}

// RotateAPIKey creates a new key for the app and mode and makes the keys that
// are live now stop working after grace; both work meanwhile. The new key takes
// the scopes of the newest key it replaces (all scopes when there is none)
// unless opts.Scopes is set. It returns the new token, the new key and the
// keys that were given an expiry.
func (s *Service) RotateAPIKey(ctx context.Context, actor audit.Actor, sc OrgScope, appID, mode string, grace time.Duration, opts KeyOptions) (string, store.APIKey, []store.APIKey, error) {
	if err := sc.validate(); err != nil {
		return "", store.APIKey{}, nil, err
	}
	if mode != "test" && mode != "live" {
		return "", store.APIKey{}, nil, newErr(ErrInvalid, "mode must be test or live")
	}
	if grace <= 0 {
		return "", store.APIKey{}, nil, newErr(ErrInvalid, "the grace period must be positive")
	}
	scopes, err := ValidateScopes(opts.Scopes)
	if err != nil {
		return "", store.APIKey{}, nil, err
	}
	token := newKeyToken(mode)
	var key store.APIKey
	var expired []store.APIKey
	err = s.db.Write(ctx, func(tx store.Tx) error {
		app, err := store.GetAppInOrg(ctx, tx, sc.OrgID, appID)
		if err != nil {
			return mapStoreErr(err, "app")
		}
		now := s.nowUTC()
		key = store.APIKey{ID: ids.New("key"), OrgID: app.OrgID, AppID: app.ID, Mode: mode, Prefix: keyPrefix(token),
			Hash: keyHash(token), CreatedAt: now, Scopes: scopes}
		if expired, err = store.ExpireAPIKeys(ctx, tx, app.ID, mode, key.ID, now.Add(grace)); err != nil {
			return err
		}
		if opts.Scopes == nil && len(expired) > 0 {
			key.Scopes = expired[len(expired)-1].Scopes // the newest old key
		}
		if opts.Authorize != nil {
			if err := opts.Authorize(key.Scopes); err != nil {
				return err // the transaction rolls back: no key, no expiry
			}
		}
		if err := store.InsertAPIKey(ctx, tx, key); err != nil {
			return err
		}
		expiring := make([]string, len(expired))
		for i, k := range expired {
			expiring[i] = k.ID
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: app.OrgID, Actor: actor, Action: "api_key.rotate", Target: key.ID,
			After: map[string]any{"app_id": app.ID, "mode": mode, "prefix": key.Prefix, "scopes": scopeLabel(key.Scopes),
				"expiring": expiring, "expires_at": now.Add(grace).Format(time.RFC3339)}})
	})
	if err != nil {
		return "", store.APIKey{}, nil, err
	}
	return token, key, expired, nil
}

// Principal is the identity behind an authenticated API key.
type Principal struct {
	KeyID, OrgID, AppID, Mode string
	// Scopes limits what the key may do; nil means every scope.
	Scopes []string
}

// dummyHash makes lookups of unknown prefixes cost the same comparison.
var dummyHash = keyHash("pmx-dummy")

// Authenticate resolves a bearer token to the app and mode of its key. The
// key is looked up by prefix and its SHA-256 compared in constant time.
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	if !keyRE.MatchString(token) {
		return Principal{}, ErrUnauthenticated
	}
	var key store.APIKey
	var lookupErr error
	if err := s.db.Read(ctx, func(q store.Querier) (err error) {
		key, err = store.GetAPIKeyByPrefix(ctx, q, keyPrefix(token))
		return
	}); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return Principal{}, err
		}
		lookupErr = err
		key.Hash = dummyHash
	}
	match := subtle.ConstantTimeCompare([]byte(keyHash(token)), []byte(key.Hash)) == 1
	now := s.nowUTC()
	expired := !key.ExpiresAt.IsZero() && !now.Before(key.ExpiresAt) // a rotated-out key past its grace period
	if lookupErr != nil || !match || !key.RevokedAt.IsZero() || expired || !strings.HasPrefix(token, "pmx_"+key.Mode+"_sk_") {
		return Principal{}, ErrUnauthenticated
	}
	if key.LastUsedAt.IsZero() || now.Sub(key.LastUsedAt) > touchEvery {
		// Best effort: a failed touch must not fail the request.
		_ = s.db.Write(ctx, func(tx store.Tx) error { return store.TouchAPIKey(ctx, tx, key.ID, now, touchEvery) })
	}
	return Principal{KeyID: key.ID, OrgID: key.OrgID, AppID: key.AppID, Mode: key.Mode, Scopes: key.Scopes}, nil
}
