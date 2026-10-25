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
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paymux/paymux/internal/alertcenter"
	"github.com/paymux/paymux/internal/auth"
	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/clientip"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/disputes"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/look"
	"github.com/paymux/paymux/internal/oidc"
	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/paylinks"
	"github.com/paymux/paymux/internal/reconcile"
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
	// accountPasswordFailures and accountTOTPFailures are the per-account
	// ceilings, whatever the address the guesses come from.
	accountPasswordFailures = 20
	accountTOTPFailures     = 10
	// loginAttemptsPerMinute is how many password attempts one address (an IPv6
	// /64 counts as one) may make a minute.
	loginAttemptsPerMinute = 30
	// ticketGuesses is how many wrong codes one login ticket allows.
	ticketGuesses = 5
	loginWindow   = 15 * time.Minute
	issuer        = "Paymux"
	maxFormBytes  = 64 << 10
)

// Options configures a Dashboard.
type Options struct {
	DB       *store.DB
	Box      *secrets.Box // encrypts TOTP secrets
	Renderer *web.Renderer
	// Themes lists the installed themes for the Appearance page (nil: none).
	Themes     *web.Themes
	MasterKey  []byte // derives the CSRF and login-ticket keys
	BaseURL    *url.URL
	DevSkip2FA bool
	Logger     *slog.Logger
	Now        func() time.Time

	// TrustedProxies are the reverse proxies whose X-Forwarded-For is
	// believed when working out the client address.
	TrustedProxies []netip.Prefix

	Core       *core.Service
	Settings   *settings.Service
	Outbound   *outbound.Service
	Invoice    *invoice.Service
	Connectors *connectors.Service
	Disputes   *disputes.Service
	Recon      *reconcile.Service
	// Jobs takes the "run reconciliation now" job (*jobs.Queue).
	Jobs interface {
		Enqueue(ctx context.Context, tx store.Tx, j jobs.Job) error
	}
	// Subscriptions serves the subscription pages; nil answers 404 there.
	Subscriptions *subscriptions.Service
	// OIDC enables single sign-on (nil: password and TOTP only). People whose
	// verified e-mail is in OIDCAllowedDomains are created at first sign-in with
	// OIDCDefaultRole (default viewer).
	OIDC               *oidc.Client
	OIDCAllowedDomains []string
	OIDCDefaultRole    string
	// PaymentLinks serves the payment links page.
	PaymentLinks *paylinks.Service
	// Alerts keeps the ops alerts shown on the Alerts page and in the nav badge.
	Alerts *alertcenter.Service
	// Location is the time zone of "today" and of date filters (default UTC).
	Location *time.Location
	// PageSize is the number of rows per list page (default 25).
	PageSize int
	// Updater serves the Update page, which installs a newer release (nil: the
	// page is not there).
	Updater Updater
	// LogDB is the database that holds the logs table (store.DB.OpenLogs); nil
	// disables the Logs page. LogRedact, when set, redacts every stored text once
	// more on its way to the page or the export.
	LogDB     *store.DB
	LogRedact func(string) string
}

// Dashboard is the http.Handler for everything under /dashboard/.
type Dashboard struct {
	o     Options
	mux   *http.ServeMux
	users Users
	lim   *limiter
	// Guessing limits that do not depend on the client address, which an
	// attacker behind a trusted proxy chooses (X-Forwarded-For): failures per
	// account, and a budget per login ticket.
	acctPw, acctTOTP *limiter
	attempts         *limiter // login attempts per address and minute, counted before any hashing
	budgets          *ticketBudgets

	preKey, ticketKey, modeKey, oidcKey []byte
	secure                              bool

	orgMu sync.Mutex
	org   string

	// roles resolves custom roles from the roles table (short TTL cache).
	roles *authz.Cache

	dummyMu   sync.Mutex
	dummyHash string

	// first-run setup: the token that opens /setup (see setup.go)
	setupMu  sync.Mutex
	setupTok string
	setupKey []byte
}

// New builds a Dashboard.
func New(o Options) (*Dashboard, error) {
	if o.DB == nil || o.Box == nil || o.Renderer == nil || o.BaseURL == nil || len(o.MasterKey) == 0 ||
		o.Core == nil || o.Settings == nil || o.Outbound == nil || o.Invoice == nil || o.Connectors == nil || o.Disputes == nil || o.Recon == nil || o.Jobs == nil || o.PaymentLinks == nil || o.Alerts == nil {
		return nil, errors.New("dashboard: DB, Box, Renderer, BaseURL, MasterKey, Core, Settings, Outbound, Invoice, Connectors, Disputes, Recon, Jobs, PaymentLinks and Alerts are required")
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
		attempts: newLimiter(loginAttemptsPerMinute, time.Minute), acctPw: newLimiter(accountPasswordFailures, loginWindow), acctTOTP: newLimiter(accountTOTPFailures, loginWindow), budgets: newTicketBudgets(),
		users:     Users{DB: o.DB, Now: o.Now},
		preKey:    derive(o.MasterKey, "dashboard-csrf-pre"),
		ticketKey: derive(o.MasterKey, "dashboard-login-ticket"),
		modeKey:   derive(o.MasterKey, "dashboard-mode"),
		oidcKey:   derive(o.MasterKey, "dashboard-oidc"),
		setupKey:  derive(o.MasterKey, "dashboard-csrf-setup"),
		// Cookies are Secure everywhere except plain-http local development.
		secure: o.BaseURL.Scheme == "https"}
	d.roles = authz.NewCache(d.loadRoles, rolesTTL, o.Now)
	d.routes()
	d.SetupToken() // on a hub without users this logs the token that opens /setup
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
}

// navItems is the full menu; built pages and placeholders interleaved.
var navItems = []navItem{
	{"overview", "Overview", prefix + "/overview"}, {"payments", "Payments", prefix + "/payments"}, {"payment-links", "Payment links", prefix + "/payment-links"},
	{"subscriptions", "Subscriptions", prefix + "/subscriptions"}, {"invoices", "Invoices", prefix + "/invoices"},
	{"disputes", "Disputes", prefix + "/disputes"}, {"reconciliation", "Reconciliation", prefix + "/reconciliation"}, {"reports", "Reports", prefix + "/reports"}, {"alerts", "Alerts", prefix + "/alerts"}, {"audit", "Audit", prefix + "/audit"}, {"logs", "Logs", prefix + "/logs"},
	{"apps", "Apps", prefix + "/apps"}, {"providers", "Providers", prefix + "/providers"},
	{"settings", "Settings", prefix + "/settings"}, {"tax-entities", "Tax entities", prefix + "/tax-entities"}, {"fx", "FX rates", prefix + "/fx"}, {"appearance", "Appearance", prefix + "/appearance"}, {"users", "Users", prefix + "/users"}, {"roles", "Roles", prefix + "/roles"}, {"update", "Update", prefix + "/update"},
}

type navItem struct{ Key, Label, Href string }

func (d *Dashboard) routes() {
	d.accountRoutes()
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
	m.HandleFunc("POST "+prefix+"/users", d.authed(authz.UserManage, d.recent(d.userCreate)))
	m.HandleFunc("POST "+prefix+"/users/{id}/role", d.authed(authz.UserManage, d.recent(d.userRole)))
	m.HandleFunc("POST "+prefix+"/users/{id}/enrolment", d.authed(authz.UserManage, d.recent(d.userEnrolment)))
	m.HandleFunc("POST "+prefix+"/users/{id}/sso-link", d.authed(authz.UserManage, d.recent(d.userSSOLink)))
	m.HandleFunc("POST "+prefix+"/users/{id}/disable", d.authed(authz.UserManage, d.recent(d.userDisable)))

	// Roles: admin only (role.manage is never grantable to a custom role).
	m.HandleFunc("GET "+prefix+"/roles", d.authed(authz.RoleManage, d.rolesPage))
	m.HandleFunc("POST "+prefix+"/roles", d.authed(authz.RoleManage, d.recent(d.roleCreate)))
	m.HandleFunc("POST "+prefix+"/roles/{name}", d.authed(authz.RoleManage, d.recent(d.roleUpdate)))
	m.HandleFunc("POST "+prefix+"/roles/{name}/delete", d.authed(authz.RoleManage, d.recent(d.roleDelete)))

	d.routesPages()
	d.updateRoutes()
	d.routesSubscriptions()
	m.HandleFunc("GET "+prefix+"/{section}", d.authed(authz.PaymentRead, d.section))
}

// setSecurityHeaders sets the headers of every dashboard page (and of /setup).
func setSecurityHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	// Radix dialogs insert scrollbar-isolation CSS. Authorize only style tags
	// carrying this response's nonce, while retaining the existing script policy.
	nonceBytes := make([]byte, 24)
	_, _ = rand.Read(nonceBytes)
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'nonce-"+nonce+"'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
}

// ServeHTTP implements http.Handler.
func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	limit := int64(maxFormBytes)
	switch {
	case strings.HasPrefix(r.URL.Path, prefix+"/disputes/") && strings.HasSuffix(r.URL.Path, "/evidence"):
		limit = 3*disputes.MaxEvidenceBytes + 1<<20 // up to three files of 10 MiB
	case r.URL.Path == prefix+"/appearance/themes/import":
		limit = themeImportMax + 16<<10 // the file plus the form's other fields
	case isUploadPath(r.URL.Path):
		limit = maxUploadBytes + 64<<10
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d.mux.ServeHTTP(w, r)
}

// ---- view model and rendering ----

// view is the data for every dash_* template. AppName, LogoURL and Accent are
// required by the shared layout.
type view struct {
	RememberDays int // the login page offers "Remember me for N days"; 0 hides it
	Sessions     []sessionRow
	Next         string // where the confirmation page returns to
	NeedCode     bool   // the confirmation page asks for an authenticator code too
	StyleNonce   string
	// DashboardTheme names the theme the React dashboard wears; see effectiveDashboardTheme.
	DashboardTheme string
	// DashboardThemeVersion changes whenever the theme's files do, so a browser
	// already wearing the theme refetches its colours after an edit.
	DashboardThemeVersion string
	ThemeForm             *themeFormView // the theme editor
	ImportForm            *importFormView
	// The account page's theme picker.
	OrgThemeName string
	ThemeChoices []themeChoice
	UserHasTheme bool // the user's own choice is set and still usable

	AppName, LogoURL, Accent string

	Title     string
	DevBanner bool
	// OAuthContinue is the provider authorization URL the OAuth continue page
	// links to (form-action 'self' forbids redirecting a form post off-site).
	OAuthContinue string
	Nav           []navItem
	Active        string
	User          *store.UserRow
	CSRF          string
	Error         string
	Notice        string

	// login
	Email      string
	SSO        bool // show the single sign-on button
	NeedsSetup bool
	// first-run setup
	SetupOrg, SetupApp, SetupSlug string
	SetupKeys                     []setupKey
	SetupSignedIn                 bool
	// totp
	Enrol               bool
	NeedEnrolCode       bool   // the enrolment page asks for the admin-issued code first
	EnrolCode           string // a code just issued, shown once
	EnrolFor            string
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

	// header: the count of open alerts in the nav badge
	AlertsOpen, AlertsCritical int
	DisputesOpen               int // disputes waiting for a response (nav count)

	// CanLogs says the user may read the logs: pages show "logs for this ..." links.
	CanLogs bool

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

// dashboardTheme is the org's theme, which the dashboard wears.
func (d *Dashboard) dashboardTheme(ctx context.Context) string {
	return look.Dashboard(ctx, d.o.Settings)
}

// defaultDashboardTheme is the dashboard theme when nothing usable is chosen.
const defaultDashboardTheme = "stripe"

// dashboardThemeUsable reports whether name loads and styles the dashboard
// (customer-page-only themes such as "default" have no dashboard block).
func (d *Dashboard) dashboardThemeUsable(name string) bool {
	if d.o.Themes == nil || !web.ValidThemeName(name) {
		return false
	}
	th, err := d.o.Themes.Load(name)
	return err == nil && th.Dashboard != nil
}

// effectiveDashboardTheme is the theme the dashboard wears for userID: the
// first usable of the user's own choice, the organisation default and stripe.
// A name that no longer loads (deleted on disk, broken) is skipped.
func (d *Dashboard) effectiveDashboardTheme(ctx context.Context, userID string) string {
	if own := d.userTheme(ctx, userID); own != "" && d.dashboardThemeUsable(own) {
		return own
	}
	return d.orgDashboardTheme(ctx)
}

// orgDashboardTheme is the organisation default, or stripe when that is not a
// usable dashboard theme.
func (d *Dashboard) orgDashboardTheme(ctx context.Context) string {
	if org := d.dashboardTheme(ctx); d.dashboardThemeUsable(org) {
		return org
	}
	return defaultDashboardTheme
}

// userTheme is the theme a user chose for themselves, "" for none (or unknown).
func (d *Dashboard) userTheme(ctx context.Context, userID string) string {
	if userID == "" || d.o.DB == nil {
		return ""
	}
	var name string
	if err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		name, err = store.UserTheme(ctx, q, userID)
		return
	}); err != nil {
		return ""
	}
	return name
}

// themedRenderer is the renderer drawing in the org's theme.
func (d *Dashboard) themedRenderer() *web.Renderer {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return d.o.Renderer.With(d.dashboardTheme(ctx), "")
}

func (d *Dashboard) render(w http.ResponseWriter, status int, page string, v view) {
	d.renderWith(w, status, d.themedRenderer(), page, v)
}

func (d *Dashboard) renderWith(w http.ResponseWriter, status int, rend *web.Renderer, page string, v view) {
	v.DevBanner = d.o.DevSkip2FA
	_, noncePart, _ := strings.Cut(w.Header().Get("Content-Security-Policy"), "'nonce-")
	v.StyleNonce, _, _ = strings.Cut(noncePart, "'")
	if v.DashboardTheme == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		v.DashboardTheme = d.effectiveDashboardTheme(ctx, v.Self)
	}
	if v.DashboardTheme != "" && d.o.Themes != nil {
		if th, err := d.o.Themes.Load(v.DashboardTheme); err == nil {
			v.DashboardThemeVersion = th.Fingerprint
		}
	}
	var buf bytes.Buffer
	if err := rend.Render(&buf, page, v); err != nil {
		d.o.Logger.Error("dashboard: render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (d *Dashboard) fail(w http.ResponseWriter, err error) {
	d.o.Logger.Error("dashboard: internal error", "request_id", w.Header().Get("X-Request-Id"), "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// ---- cookies ----

func (d *Dashboard) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: prefix, MaxAge: maxAge, HttpOnly: true,
		Secure: d.secure, SameSite: http.SameSiteLaxMode}) // no Domain: host-only
}

func (d *Dashboard) clearCookie(w http.ResponseWriter, name string) { d.setCookie(w, name, "", -1) }

// clientIP is the address of the browser (see package clientip).
func (d *Dashboard) clientIP(r *http.Request) string { return clientip.From(r, d.o.TrustedProxies) }

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

// isUploadPath reports whether the route takes a logo upload.
func isUploadPath(p string) bool {
	return strings.HasSuffix(p, "/branding") || strings.HasSuffix(p, "/tax-entities") || strings.Contains(p, "/tax-entities/")
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

func (d *Dashboard) issueTicket(w http.ResponseWriter, userID string, remember bool) {
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	flag := "0"
	if remember {
		flag = "1" // signed with the rest: the second step cannot change it
	}
	payload := userID + "|" + strconv.FormatInt(d.o.Now().Add(ticketTTL).Unix(), 10) + "|" + base64.RawURLEncoding.EncodeToString(nonce) + "|" + flag
	m := hmac.New(sha256.New, d.ticketKey)
	m.Write([]byte(payload))
	v := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	d.setCookie(w, cookieTicket, v, int(ttlSeconds(ticketTTL)))
}

func ttlSeconds(d time.Duration) int64 { return int64(d.Seconds()) }

// ticketUser returns the user id of a valid, unexpired ticket and the ticket's
// own id (what its guess budget is kept under). A ticket that has used up its
// guesses is no ticket.
func (d *Dashboard) ticketUser(r *http.Request) (userID, ticketID string, remember, ok bool) {
	c, err := r.Cookie(cookieTicket)
	if err != nil {
		return "", "", false, false
	}
	p, s, found := strings.Cut(c.Value, ".")
	if !found {
		return "", "", false, false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	sig, err2 := base64.RawURLEncoding.DecodeString(s)
	if err1 != nil || err2 != nil {
		return "", "", false, false
	}
	m := hmac.New(sha256.New, d.ticketKey)
	m.Write(payload)
	if !hmac.Equal(sig, m.Sum(nil)) {
		return "", "", false, false
	}
	parts := strings.Split(string(payload), "|")
	if len(parts) != 4 {
		return "", "", false, false
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || d.o.Now().Unix() > n {
		return "", "", false, false
	}
	if d.budgets.spent(parts[2], d.o.Now()) {
		return "", "", false, false
	}
	return parts[0], parts[2], parts[3] == "1", true
}

// ticketBudgets counts the wrong codes each login ticket has taken, in memory.
// A ticket with ticketGuesses failures is dead: the password step must be
// repeated.
type ticketBudgets struct {
	mu sync.Mutex
	m  map[string]ticketState
}

type ticketState struct {
	fails int
	until time.Time
}

func newTicketBudgets() *ticketBudgets { return &ticketBudgets{m: map[string]ticketState{}} }

func (b *ticketBudgets) spent(id string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.m[id].fails >= ticketGuesses
}

// fail records a wrong code and says whether the ticket is now dead.
func (b *ticketBudgets) fail(id string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.m) > 10_000 {
		for k, v := range b.m {
			if now.After(v.until) {
				delete(b.m, k)
			}
		}
	}
	st := b.m[id]
	st.fails++
	st.until = now.Add(2 * ticketTTL)
	b.m[id] = st
	return st.fails >= ticketGuesses
}

// kill makes the ticket unusable.
func (b *ticketBudgets) kill(id string, now time.Time) {
	b.mu.Lock()
	b.m[id] = ticketState{fails: ticketGuesses, until: now.Add(2 * ticketTTL)}
	b.mu.Unlock()
}

// ---- sessions ----

type session struct {
	ID      string
	User    store.UserRow
	CSRFKey []byte
	roles   authz.Resolver
	// OrgID is the org the user belongs to: the dashboard's own (users have no org
	// of their own while there is one org).
	OrgID string
	// Remember is true for a sign-in that was asked to last longer; AuthenticatedAt
	// is when the user last gave the password and the second factor.
	Remember        bool
	AuthenticatedAt time.Time
}

func (s *session) csrf() string { return auth.CSRFToken(s.CSRFKey, s.ID) }

// createSession starts a session after a complete sign-in. remember asks for the
// longer life (session_remember_days) and is honoured only while that setting is
// above zero. The session id is new: nothing from before the sign-in is reused.
func (d *Dashboard) createSession(ctx context.Context, w http.ResponseWriter, r *http.Request, u store.UserRow, remember bool) error {
	lim := d.sessionLimits(ctx)
	remember = remember && lim.remember > 0
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
	life, cookieAge := lim.max, 0 // a session cookie, as before, unless remembered
	if remember {
		life, cookieAge = lim.remember, int(lim.remember.Seconds())
	}
	ip := d.clientIP(r)
	row := store.SessionRow{ID: sessionID(), UserID: u.ID, TokenHash: hash, CSRFKey: key, CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(life), IP: ip, UserAgent: ua, Remember: remember, AuthenticatedAt: now, IPHint: ipHint(ip)}
	if err := d.o.DB.InsertSession(ctx, row); err != nil {
		return err
	}
	flag := "false"
	if remember {
		flag = "true"
	}
	if err := d.recordSSO(ctx, r, u.ID, "user.login", map[string]string{"email": u.Email, "remember": flag}); err != nil {
		d.o.Logger.Warn("dashboard: audit of sign-in", "err", err)
	}
	if _, err := d.o.DB.DeleteExpiredSessions(ctx, now); err != nil {
		d.o.Logger.Warn("dashboard: session cleanup", "err", err)
	}
	d.setCookie(w, cookieSession, token, cookieAge)
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
	lim := d.sessionLimits(r.Context())
	idle := lim.idle
	if s.Remember && lim.remember > 0 {
		idle = lim.remember
	}
	if !now.Before(s.ExpiresAt) || now.Sub(s.LastSeenAt) > idle {
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
	return &session{ID: s.ID, User: u, CSRFKey: s.CSRFKey, roles: d.roles, OrgID: d.sessionOrg(r.Context()), Remember: s.Remember, AuthenticatedAt: s.AuthenticatedAt}, true
}

type authedHandler func(w http.ResponseWriter, r *http.Request, s *session)

// actor is the session's user as an authz actor.
func (s *session) actor() authz.Actor {
	return authz.Actor{Kind: authz.KindUser, ID: s.User.ID, Role: authz.Role(s.User.Role), OrgID: s.OrgID}
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
		if action != selfOnly && !s.can(action) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r, s)
	}
}

func (d *Dashboard) shell(r *http.Request, s *session, active, title string) view {
	u := s.User
	v := view{Title: title, Nav: navItems, Active: active, User: &u, CSRF: s.csrf(), Self: u.ID,
		Mode: d.mode(r, s), Path: r.URL.RequestURI(), CanLogs: s.can(authz.LogsRead)}
	v.Nav = slices.DeleteFunc(slices.Clone(navItems), func(n navItem) bool {
		return n.Key == "logs" && !v.CanLogs || n.Key == "update" && (d.o.Updater == nil || !s.can(authz.SystemUpdate))
	})
	v.AlertsOpen, v.AlertsCritical = d.alertCounts(r)
	v.DisputesOpen = d.disputeCount(r)
	return v
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
