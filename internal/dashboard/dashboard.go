// Package dashboard is the operator web UI (spec §12): login with password and
// TOTP, sessions, CSRF, user management and the navigation shell the other
// pages plug into.
package dashboard

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paymux/paymux/internal/auth"
	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/oidc"
	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/subscriptions"
	"github.com/paymux/paymux/internal/web"
)

const (
	prefix = "/dashboard"

	cookieSession = "pm_session"
	cookiePre     = "pm_pre"   // anonymous CSRF seed for the login forms
	cookieTicket  = "pm_login" // password step passed, TOTP pending

	// IdleTimeout ends a session that saw no request for this long.
	IdleTimeout = 12 * time.Hour
	// MaxSessionAge ends a session regardless of activity.
	MaxSessionAge = 7 * 24 * time.Hour

	rolesTTL      = 5 * time.Second // how long a role change may take to reach other processes
	ticketTTL     = 5 * time.Minute
	touchEvery    = time.Minute
	loginFailures = 5
	loginWindow   = 15 * time.Minute
	issuer        = "Paymux"
	maxFormBytes  = 64 << 10
)

// Options configures a Dashboard.
type Options struct {
	DB         *store.DB
	Box        *secrets.Box // encrypts TOTP secrets
	Renderer   *web.Renderer
	MasterKey  []byte // derives the CSRF and login-ticket keys
	BaseURL    *url.URL
	DevSkip2FA bool
	Logger     *slog.Logger
	Now        func() time.Time

	Core     *core.Service
	Settings *settings.Service
	Outbound *outbound.Service
	// Subscriptions serves the subscription pages; nil answers 404 there.
	Subscriptions *subscriptions.Service
	// OIDC enables single sign-on (nil: password and TOTP only). People whose
	// verified e-mail is in OIDCAllowedDomains are created at first sign-in with
	// OIDCDefaultRole (default viewer).
	OIDC               *oidc.Client
	OIDCAllowedDomains []string
	OIDCDefaultRole    string
	// Location is the time zone of "today" and of date filters (default UTC).
	Location *time.Location
	// PageSize is the number of rows per list page (default 25).
	PageSize int
}

// Dashboard is the http.Handler for everything under /dashboard/.
type Dashboard struct {
	o     Options
	mux   *http.ServeMux
	users Users
	lim   *limiter

	preKey, ticketKey, modeKey, oidcKey []byte
	secure                              bool

	orgMu sync.Mutex
	org   string

	// roles resolves custom roles from the roles table (short TTL cache).
	roles *authz.Cache

	dummyOnce sync.Once
	dummyHash string
}

// New builds a Dashboard.
func New(o Options) (*Dashboard, error) {
	if o.DB == nil || o.Box == nil || o.Renderer == nil || o.BaseURL == nil || len(o.MasterKey) == 0 ||
		o.Core == nil || o.Settings == nil || o.Outbound == nil {
		return nil, errors.New("dashboard: DB, Box, Renderer, BaseURL, MasterKey, Core, Settings and Outbound are required")
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.PageSize <= 0 {
		o.PageSize = 25
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	d := &Dashboard{o: o, mux: http.NewServeMux(), lim: newLimiter(loginFailures, loginWindow),
		users:     Users{DB: o.DB, Now: o.Now},
		preKey:    derive(o.MasterKey, "dashboard-csrf-pre"),
		ticketKey: derive(o.MasterKey, "dashboard-login-ticket"),
		modeKey:   derive(o.MasterKey, "dashboard-mode"),
		oidcKey:   derive(o.MasterKey, "dashboard-oidc"),
		// Cookies are Secure everywhere except plain-http local development.
		secure: o.BaseURL.Scheme == "https"}
	d.roles = authz.NewCache(d.loadRoles, rolesTTL, o.Now)
	d.routes()
	return d, nil
}

func derive(master []byte, label string) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte(label))
	return m.Sum(nil)
}

// sections are the placeholder pages of the nav shell.
var sections = []navItem{
	{"subscriptions", "Subscriptions", prefix + "/subscriptions"},
	{"disputes", "Disputes", prefix + "/disputes"}, {"invoices", "Invoices", prefix + "/invoices"},
	{"reports", "Reports", prefix + "/reports"}, {"providers", "Providers", prefix + "/providers"},
}

// navItems is the full menu; the first entries are built pages, the rest of
// the placeholders come from sections.
var navItems = func() []navItem {
	items := []navItem{{"overview", "Overview", prefix + "/overview"}, {"payments", "Payments", prefix + "/payments"}}
	items = append(items, sections...)
	return append(items, navItem{"apps", "Apps", prefix + "/apps"}, navItem{"settings", "Settings", prefix + "/settings"},
		navItem{"audit", "Audit", prefix + "/audit"}, navItem{"users", "Users", prefix + "/users"},
		navItem{"roles", "Roles", prefix + "/roles"})
}()

type navItem struct{ Key, Label, Href string }

func (d *Dashboard) routes() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/{$}", d.authed(authz.PaymentRead, d.root))
	m.HandleFunc("GET "+prefix+"/login", d.loginPage)
	m.HandleFunc("POST "+prefix+"/login", d.loginSubmit)
	m.HandleFunc("GET "+prefix+"/login/oidc", d.ssoStart)
	m.HandleFunc("GET "+prefix+"/login/oidc/callback", d.ssoCallback)
	m.HandleFunc("GET "+prefix+"/login/totp", d.totpPage)
	m.HandleFunc("POST "+prefix+"/login/totp", d.totpSubmit)
	m.HandleFunc("POST "+prefix+"/logout", d.authed(authz.PaymentRead, d.logout))
	m.HandleFunc("POST "+prefix+"/mode", d.authed(authz.PaymentRead, d.setMode))

	// Users. The list is visible to everyone who can read settings.
	m.HandleFunc("GET "+prefix+"/users", d.authed(authz.SettingsRead, d.usersPage))
	m.HandleFunc("POST "+prefix+"/users", d.authed(authz.UserManage, d.userCreate))
	m.HandleFunc("POST "+prefix+"/users/{id}/role", d.authed(authz.UserManage, d.userRole))
	m.HandleFunc("POST "+prefix+"/users/{id}/disable", d.authed(authz.UserManage, d.userDisable))

	// Roles: admin only (role.manage is never grantable to a custom role).
	m.HandleFunc("GET "+prefix+"/roles", d.authed(authz.RoleManage, d.rolesPage))
	m.HandleFunc("POST "+prefix+"/roles", d.authed(authz.RoleManage, d.roleCreate))
	m.HandleFunc("POST "+prefix+"/roles/{name}", d.authed(authz.RoleManage, d.roleUpdate))
	m.HandleFunc("POST "+prefix+"/roles/{name}/delete", d.authed(authz.RoleManage, d.roleDelete))

	d.routesPages()
	d.routesSubscriptions()
	m.HandleFunc("GET "+prefix+"/{section}", d.authed(authz.PaymentRead, d.section))
}

// ServeHTTP implements http.Handler.
func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
	limit := int64(maxFormBytes)
	if strings.HasSuffix(r.URL.Path, "/branding") {
		limit = maxUploadBytes + 64<<10
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d.mux.ServeHTTP(w, r)
}

// ---- view model and rendering ----

// view is the data for every dash_* template. AppName, LogoURL and Accent are
// required by the shared layout.
type view struct {
	AppName, LogoURL, Accent string

	Title     string
	DevBanner bool
	Nav       []navItem
	Active    string
	User      *store.UserRow
	CSRF      string
	Error     string
	Notice    string

	// login
	Email string
	SSO   bool // show the single sign-on button
	// totp
	Enrol               bool
	TOTPSecret, TOTPURI string
	// users
	Users     []userRow
	Roles     []string
	RoleRows  []roleRow
	NewBoxes  []actionBox
	CanManage bool
	Self      string
	// section placeholder
	Section string

	// header: test/live toggle and where to come back to
	Mode, Path string

	Page // page-specific data of the part-2 pages
}

type userRow struct {
	ID, Email, Role string
	Disabled        bool
	Enrolled        bool
	Created         string
	IsSelf          bool
}

func (d *Dashboard) render(w http.ResponseWriter, status int, page string, v view) {
	v.DevBanner = d.o.DevSkip2FA
	var buf bytes.Buffer
	if err := d.o.Renderer.Render(&buf, page, v); err != nil {
		d.o.Logger.Error("dashboard: render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (d *Dashboard) fail(w http.ResponseWriter, err error) {
	d.o.Logger.Error("dashboard: internal error", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// ---- cookies ----

func (d *Dashboard) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: prefix, MaxAge: maxAge, HttpOnly: true,
		Secure: d.secure, SameSite: http.SameSiteLaxMode}) // no Domain: host-only
}

func (d *Dashboard) clearCookie(w http.ResponseWriter, name string) { d.setCookie(w, name, "", -1) }

func clientIP(r *http.Request) string {
	// TODO(spec §12): honour a trusted proxy header once deployment config has one.
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// ---- anonymous CSRF (login forms) ----

// preToken returns the CSRF token for the anonymous forms, creating the seed
// cookie if the browser has none.
func (d *Dashboard) preToken(w http.ResponseWriter, r *http.Request) string {
	seed := ""
	if c, err := r.Cookie(cookiePre); err == nil && c.Value != "" {
		seed = c.Value
	} else {
		b := make([]byte, 18)
		_, _ = rand.Read(b)
		seed = base64.RawURLEncoding.EncodeToString(b)
		d.setCookie(w, cookiePre, seed, 0)
	}
	return auth.CSRFToken(d.preKey, seed)
}

// validPre checks the CSRF token of an anonymous form post.
func (d *Dashboard) validPre(r *http.Request) bool {
	c, err := r.Cookie(cookiePre)
	if err != nil || c.Value == "" {
		return false
	}
	return auth.ValidCSRF(d.preKey, c.Value, formCSRF(r))
}

// maxUploadBytes bounds the multipart branding form (a logo of up to 512 KB
// plus overhead, with room to report an oversize logo instead of failing).
const maxUploadBytes = 3 << 20

// parseForm parses urlencoded and multipart bodies alike into r.PostForm.
func parseForm(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.ParseMultipartForm(maxUploadBytes)
	}
	return r.ParseForm()
}

func formCSRF(r *http.Request) string {
	if t := r.Header.Get("X-CSRF-Token"); t != "" {
		return t
	}
	if err := parseForm(r); err != nil {
		return ""
	}
	return r.PostForm.Get("csrf")
}

// ---- login tickets (password verified, TOTP pending) ----

func (d *Dashboard) issueTicket(w http.ResponseWriter, userID string) {
	payload := userID + "|" + strconv.FormatInt(d.o.Now().Add(ticketTTL).Unix(), 10)
	m := hmac.New(sha256.New, d.ticketKey)
	m.Write([]byte(payload))
	v := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	d.setCookie(w, cookieTicket, v, int(ticketTTL.Seconds()))
}

// ticketUser returns the user id of a valid, unexpired ticket.
func (d *Dashboard) ticketUser(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieTicket)
	if err != nil {
		return "", false
	}
	p, s, ok := strings.Cut(c.Value, ".")
	if !ok {
		return "", false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	sig, err2 := base64.RawURLEncoding.DecodeString(s)
	if err1 != nil || err2 != nil {
		return "", false
	}
	m := hmac.New(sha256.New, d.ticketKey)
	m.Write(payload)
	if !hmac.Equal(sig, m.Sum(nil)) {
		return "", false
	}
	id, exp, ok := strings.Cut(string(payload), "|")
	n, err := strconv.ParseInt(exp, 10, 64)
	if !ok || err != nil || d.o.Now().Unix() > n {
		return "", false
	}
	return id, true
}

// ---- sessions ----

type session struct {
	ID      string
	User    store.UserRow
	CSRFKey []byte
	roles   authz.Resolver
}

func (s *session) csrf() string { return auth.CSRFToken(s.CSRFKey, s.ID) }

func (d *Dashboard) createSession(ctx context.Context, w http.ResponseWriter, r *http.Request, u store.UserRow) error {
	token, hash := auth.NewSessionToken()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	now := d.o.Now()
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	row := store.SessionRow{ID: sessionID(), UserID: u.ID, TokenHash: hash, CSRFKey: key, CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(MaxSessionAge), IP: clientIP(r), UserAgent: ua}
	if err := d.o.DB.InsertSession(ctx, row); err != nil {
		return err
	}
	if _, err := d.o.DB.DeleteExpiredSessions(ctx, now); err != nil {
		d.o.Logger.Warn("dashboard: session cleanup", "err", err)
	}
	d.setCookie(w, cookieSession, token, 0)
	d.clearCookie(w, cookieTicket)
	return nil
}

func sessionID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "ses_" + base64.RawURLEncoding.EncodeToString(b)
}

// currentSession resolves the cookie to a live session, deleting it when it
// has idled out or passed its absolute limit.
func (d *Dashboard) currentSession(r *http.Request) (*session, bool) {
	c, err := r.Cookie(cookieSession)
	if err != nil || c.Value == "" {
		return nil, false
	}
	s, u, err := d.o.DB.SessionByTokenHash(r.Context(), auth.HashToken(c.Value))
	if err != nil {
		return nil, false
	}
	now := d.o.Now()
	if !now.Before(s.ExpiresAt) || now.Sub(s.LastSeenAt) > IdleTimeout {
		_ = d.o.DB.DeleteSession(r.Context(), s.ID)
		return nil, false
	}
	if u.Disabled {
		return nil, false
	}
	if now.Sub(s.LastSeenAt) > touchEvery {
		if err := d.o.DB.TouchSession(r.Context(), s.ID, now); err != nil {
			d.o.Logger.Warn("dashboard: touch session", "err", err)
		}
	}
	return &session{ID: s.ID, User: u, CSRFKey: s.CSRFKey, roles: d.roles}, true
}

type authedHandler func(w http.ResponseWriter, r *http.Request, s *session)

// actor is the session's user as an authz actor.
func (s *session) actor() authz.Actor {
	return authz.Actor{Kind: authz.KindUser, ID: s.User.ID, Role: authz.Role(s.User.Role)}
}

// can reports whether the session's user may perform action.
func (s *session) can(action authz.Action) bool {
	return authz.CanWith(s.roles, s.actor(), action, authz.Resource{})
}

// authed wraps h: it needs a live session, checks CSRF on POST, then asks
// authz whether the user may perform action.
func (d *Dashboard) authed(action authz.Action, h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := d.currentSession(r)
		if !ok {
			d.clearCookie(w, cookieSession)
			http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			if err := parseForm(r); err != nil {
				var tooBig *http.MaxBytesError
				if errors.As(err, &tooBig) {
					http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
				} else {
					http.Error(w, "bad request", http.StatusBadRequest)
				}
				return
			}
			if !auth.ValidCSRF(s.CSRFKey, s.ID, formCSRF(r)) {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		if !s.can(action) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r, s)
	}
}

func (d *Dashboard) shell(r *http.Request, s *session, active, title string) view {
	u := s.User
	return view{Title: title, Nav: navItems, Active: active, User: &u, CSRF: s.csrf(), Self: u.ID,
		Mode: d.mode(r, s), Path: r.URL.RequestURI()}
}

func (d *Dashboard) root(w http.ResponseWriter, r *http.Request, s *session) {
	http.Redirect(w, r, prefix+"/overview", http.StatusSeeOther)
}

func (d *Dashboard) section(w http.ResponseWriter, r *http.Request, s *session) {
	key := r.PathValue("section")
	for _, it := range sections {
		if it.Key == key {
			v := d.shell(r, s, key, it.Label)
			v.Section = it.Label
			d.render(w, http.StatusOK, "dash_section", v)
			return
		}
	}
	http.NotFound(w, r)
}
