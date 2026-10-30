package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"unicode"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/auth"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
)

// First-run setup. While the hub has no users, /setup shows a one-time form
// that creates the admin, names the org and optionally creates a first app
// with its API keys. It is protected by a token printed to the log at start,
// so only someone who can read the server's log can claim a fresh hub. Once a
// user exists /setup answers 404 for good.

// setupSlugRE is the app slug rule of core.CreateApp.
var setupSlugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// SetupToken returns the token that opens /setup, or "" when setup is closed
// because a user exists. The first call on a hub without users makes the token
// and logs it.
func (d *Dashboard) SetupToken() string {
	d.setupMu.Lock()
	defer d.setupMu.Unlock()
	return d.setupTokenLocked()
}

func (d *Dashboard) setupTokenLocked() string {
	n, err := d.o.DB.CountUsers(context.Background())
	if err != nil || n > 0 {
		d.setupTok = ""
		return ""
	}
	if d.setupTok == "" {
		d.setupTok = newSetupToken()
		// Logged under a key the logger does not redact, on purpose: the log is
		// where the operator reads it. Letters only, so that the logger's
		// number masking cannot alter it.
		d.o.Logger.Info("first-run setup is open: there are no users yet. Open the URL and enter the code to create the admin",
			"url", strings.TrimSuffix(d.o.BaseURL.String(), "/")+"/setup", "setup_code", d.setupTok)
	}
	return d.setupTok
}

// newSetupToken returns 24 random lower-case letters in four groups (about
// 112 bits).
func newSetupToken() string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	var b []byte
	for len(b) < 24 {
		var one [1]byte
		_, _ = rand.Read(one[:])
		if one[0] < 26*9 { // below 234: rejection sampling keeps the letters uniform
			b = append(b, letters[int(one[0])%26])
		}
	}
	return string(b[0:6]) + "-" + string(b[6:12]) + "-" + string(b[12:18]) + "-" + string(b[18:24])
}

// SetupHandler serves /setup.
func (d *Dashboard) SetupHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if d.SetupToken() == "" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			d.setupForm(w, r, http.StatusOK, setupValues{}, "")
		case http.MethodPost:
			d.setupSubmit(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// setupValues are the form fields that are shown again after a mistake
// (never the token or the passwords).
type setupValues struct{ Email, Org, AppName, AppSlug string }

func (d *Dashboard) setupForm(w http.ResponseWriter, r *http.Request, status int, vals setupValues, msg string) {
	v := view{Title: "First-run setup", CSRF: d.setupPreToken(w, r), Error: msg, Email: vals.Email}
	v.SetupOrg, v.SetupApp, v.SetupSlug = vals.Org, vals.AppName, vals.AppSlug
	d.render(w, status, "dash_setup", v)
}

func deriveSlug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if r < unicode.MaxASCII && (unicode.IsLower(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "app"
	}
	return s
}

func (d *Dashboard) setupSubmit(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !d.validSetupPre(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	f := r.PostForm
	vals := setupValues{Email: strings.ToLower(strings.TrimSpace(f.Get("email"))), Org: strings.TrimSpace(f.Get("org_name")),
		AppName: strings.TrimSpace(f.Get("app_name")), AppSlug: strings.TrimSpace(f.Get("app_slug"))}

	// One at a time: the token is checked and spent under the same lock that
	// decides there are still no users.
	d.setupMu.Lock()
	defer d.setupMu.Unlock()
	tok := d.setupTokenLocked()
	if tok == "" {
		http.NotFound(w, r)
		return
	}
	key, now := "setup|"+d.clientIP(r), d.o.Now()
	if d.lim.locked(key, now) {
		d.setupForm(w, r, http.StatusTooManyRequests, vals, msgLockedOut)
		return
	}
	given := strings.TrimSpace(f.Get("token"))
	if given == "" || subtle.ConstantTimeCompare([]byte(given), []byte(tok)) != 1 {
		d.lim.fail(key, now)
		d.setupForm(w, r, http.StatusForbidden, vals, "That setup code is not right. It is in the hub's log from when it started.")
		return
	}

	bad := func(msg string) { d.setupForm(w, r, http.StatusBadRequest, vals, msg) }
	if a, err := mail.ParseAddress(vals.Email); err != nil || a.Address != vals.Email {
		bad("Please enter a valid email address.")
		return
	}
	pw := f.Get("password")
	switch {
	case len([]rune(pw)) < auth.MinPasswordLen:
		bad("The password must be at least 12 characters.")
		return
	case pw != f.Get("password2"):
		bad("The two passwords do not match.")
		return
	case len(vals.Org) > 100 || strings.ContainsFunc(vals.Org, unicode.IsControl):
		bad("The organisation name is too long (100 characters at most).")
		return
	case vals.AppName == "" && vals.AppSlug != "":
		bad("Give the app a name, or clear its slug.")
		return
	case len(vals.AppName) > 200 || strings.ContainsFunc(vals.AppName, unicode.IsControl):
		bad("The app name is too long (200 characters at most).")
		return
	}
	if vals.AppName != "" {
		if vals.AppSlug == "" {
			vals.AppSlug = deriveSlug(vals.AppName)
		}
		if !setupSlugRE.MatchString(vals.AppSlug) {
			bad("The app slug may only use a-z, 0-9 and hyphens, and must start with a letter or digit (40 characters at most).")
			return
		}
	}

	ctx := r.Context()
	u, err := d.users.Create(ctx, vals.Email, RoleAdmin, pw)
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			bad(strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
			return
		}
		d.fail(w, err)
		return
	}
	d.setupTok = "" // spent: /setup is closed from here on
	// The setup code already vouches for this person, so no enrolment code is needed.
	if err := d.users.AuthoriseEnrolment(ctx, u.ID); err != nil {
		d.fail(w, err)
		return
	}

	org, err := d.orgID(ctx)
	if err == nil {
		err = d.o.DB.Write(ctx, func(tx store.Tx) error {
			if vals.Org != "" {
				if err := store.RenameOrg(ctx, tx, org, vals.Org); err != nil {
					return err
				}
			}
			return audit.Record(ctx, tx, audit.Entry{OrgID: org, Actor: audit.Actor{Type: audit.ActorUser, ID: u.ID}, Action: "setup.complete",
				Target: u.ID, After: map[string]string{"email": u.Email, "org_name": vals.Org, "app": vals.AppSlug}})
		})
	}
	var keys []setupKey
	if err == nil && vals.AppName != "" {
		actor, scope := audit.Actor{Type: audit.ActorUser, ID: u.ID}, core.OrgScope{OrgID: org}
		var app store.App
		if app, err = d.o.Core.CreateApp(ctx, actor, scope, core.CreateAppInput{Name: vals.AppName, Slug: vals.AppSlug}); err == nil {
			for _, mode := range []string{"test", "live"} {
				var tokenStr string
				if tokenStr, _, err = d.o.Core.CreateAPIKey(ctx, actor, scope, app.ID, mode); err != nil {
					break
				}
				keys = append(keys, setupKey{Mode: mode, Token: tokenStr})
			}
		}
	}
	if err != nil {
		d.o.Logger.Error("dashboard: first-run setup stopped after creating the admin", "err", err)
		http.Error(w, "The admin account was created, but the rest of the setup failed. Sign in at /dashboard/login and finish from the Apps page.", http.StatusInternalServerError)
		return
	}

	v := view{Title: "Setup complete", Email: u.Email, CSRF: d.preToken(w, r)}
	v.SetupOrg, v.SetupApp, v.SetupKeys = vals.Org, vals.AppName, keys
	if d.o.DevSkip2FA {
		if err := d.createSession(ctx, w, r, u, false); err != nil {
			d.fail(w, err)
			return
		}
		v.SetupSignedIn = true
	} else {
		// The same second-factor enrolment as a first login: the secret is
		// shown, and the first accepted code signs the admin in.
		secret, uri, _, err := d.totpSecret(r, u)
		if err != nil {
			d.fail(w, err)
			return
		}
		v.TOTPSecret, v.TOTPURI = secret, uri
		d.issueTicket(w, u.ID, false)
	}
	d.render(w, http.StatusOK, "dash_setup_done", v)
}

type setupKey struct{ Mode, Token string }

// The setup form lives outside /dashboard, where the other anonymous forms'
// seed cookie (Path=/dashboard) is not sent, so it has a seed cookie of its own.
const cookieSetup = "pm_setup"

func (d *Dashboard) setupPreToken(w http.ResponseWriter, r *http.Request) string {
	seed := ""
	if c, err := r.Cookie(cookieSetup); err == nil && c.Value != "" {
		seed = c.Value
	} else {
		b := make([]byte, 18)
		_, _ = rand.Read(b)
		seed = base64.RawURLEncoding.EncodeToString(b)
		http.SetCookie(w, &http.Cookie{Name: cookieSetup, Value: seed, Path: "/setup", HttpOnly: true, Secure: d.secure, SameSite: http.SameSiteLaxMode})
	}
	return auth.CSRFToken(d.setupKey, seed)
}

func (d *Dashboard) validSetupPre(r *http.Request) bool {
	c, err := r.Cookie(cookieSetup)
	if err != nil || c.Value == "" {
		return false
	}
	return auth.ValidCSRF(d.setupKey, c.Value, formCSRF(r))
}
