package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/oidc"
	"github.com/paymux/paymux/internal/store"
)

// Single sign-on (OpenID Connect). A sign-in through the identity provider
// replaces the password and TOTP steps: the IdP authenticated the person, so
// Paymux asks nothing more. Users are matched by e-mail; someone not yet known
// is created only when the e-mail is verified and in an allowed domain, with
// the default role. Password logins still need TOTP.

const (
	cookieOIDC = "pm_oidc" // state, nonce and PKCE verifier of a sign-in in progress
	oidcTTL    = 10 * time.Minute

	msgSSOFailed = "Single sign-on failed. Try again, or sign in with your password."
	// ssoPasswordHash can never verify (it is not a PHC hash), so a user created
	// by single sign-on has no password until an admin sets one.
	ssoPasswordHash = "!sso"
)

func (d *Dashboard) ssoEnabled() bool { return d.o.OIDC != nil }

// ssoCookie seals the sign-in's secrets into a signed value: nothing is kept
// on the server, and the browser cannot alter state, nonce or verifier.
func (d *Dashboard) ssoCookie(state, nonce, verifier string) string {
	payload := strings.Join([]string{state, nonce, verifier, strconv.FormatInt(d.o.Now().Add(oidcTTL).Unix(), 10)}, "|")
	m := hmac.New(sha256.New, d.oidcKey)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (d *Dashboard) readSSOCookie(r *http.Request) (state, nonce, verifier string, ok bool) {
	c, err := r.Cookie(cookieOIDC)
	if err != nil {
		return
	}
	p, s, found := strings.Cut(c.Value, ".")
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	sig, err2 := base64.RawURLEncoding.DecodeString(s)
	if !found || err1 != nil || err2 != nil {
		return
	}
	m := hmac.New(sha256.New, d.oidcKey)
	m.Write(payload)
	if !hmac.Equal(sig, m.Sum(nil)) {
		return
	}
	f := strings.Split(string(payload), "|")
	if len(f) != 4 {
		return
	}
	exp, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil || d.o.Now().Unix() > exp {
		return
	}
	return f[0], f[1], f[2], true
}

func (d *Dashboard) ssoStart(w http.ResponseWriter, r *http.Request) {
	if !d.ssoEnabled() {
		http.NotFound(w, r)
		return
	}
	state, nonce := oidc.Random(24), oidc.Random(24)
	verifier, challenge := oidc.NewPKCE()
	to, err := d.o.OIDC.AuthURL(r.Context(), state, nonce, challenge)
	if err != nil {
		d.o.Logger.Error("dashboard: sso start", "err", err)
		d.ssoFailed(w, r, http.StatusBadGateway)
		return
	}
	d.setCookie(w, cookieOIDC, d.ssoCookie(state, nonce, verifier), int(oidcTTL.Seconds()))
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (d *Dashboard) ssoFailed(w http.ResponseWriter, r *http.Request, status int) {
	d.clearCookie(w, cookieOIDC)
	d.render(w, status, "dash_login", view{Title: "Sign in", CSRF: d.preToken(w, r), Error: msgSSOFailed, SSO: d.ssoEnabled()})
}

func (d *Dashboard) ssoCallback(w http.ResponseWriter, r *http.Request) {
	if !d.ssoEnabled() {
		http.NotFound(w, r)
		return
	}
	state, nonce, verifier, ok := d.readSSOCookie(r)
	q := r.URL.Query()
	if !ok || state == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
		d.o.Logger.Warn("dashboard: sso callback with a missing or wrong state", "ip", d.clientIP(r))
		d.ssoFailed(w, r, http.StatusBadRequest)
		return
	}
	d.clearCookie(w, cookieOIDC) // a sign-in attempt is single use
	if e := q.Get("error"); e != "" || q.Get("code") == "" {
		d.o.Logger.Warn("dashboard: sso refused by the identity provider", "error", q.Get("error"), "ip", d.clientIP(r))
		d.ssoFailed(w, r, http.StatusUnauthorized)
		return
	}
	raw, err := d.o.OIDC.Exchange(r.Context(), q.Get("code"), verifier)
	var claims oidc.Claims
	if err == nil {
		claims, err = d.o.OIDC.Verify(r.Context(), raw, nonce)
	}
	if err != nil {
		d.o.Logger.Warn("dashboard: sso sign-in rejected", "err", err, "ip", d.clientIP(r))
		d.ssoFailed(w, r, http.StatusUnauthorized)
		return
	}
	u, err := d.ssoUser(r.Context(), r, claims)
	if err != nil {
		d.o.Logger.Warn("dashboard: sso sign-in refused", "email", claims.Email, "reason", err, "ip", d.clientIP(r))
		d.ssoFailed(w, r, http.StatusForbidden)
		return
	}
	if err := d.createSession(r.Context(), w, r, u); err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/", http.StatusSeeOther)
}

var errSSORefused = errors.New("not allowed to sign in")

// ssoUser finds the user for the verified claims, or creates one when the
// e-mail's domain is allowed. Anything else is refused.
func (d *Dashboard) ssoUser(ctx context.Context, r *http.Request, c oidc.Claims) (store.UserRow, error) {
	a, err := mail.ParseAddress(c.Email)
	if err != nil || a.Address != c.Email {
		return store.UserRow{}, errors.New("the id_token has no usable e-mail")
	}
	if !c.EmailVerified { // an unverified address proves nothing about who owns it
		return store.UserRow{}, errors.New("the e-mail is not verified by the identity provider")
	}
	u, err := d.o.DB.UserByEmail(ctx, c.Email)
	switch {
	case err == nil:
		if u.Disabled {
			return store.UserRow{}, errors.New("the account is disabled")
		}
		return u, d.recordSSO(ctx, r, u.ID, "user.sso_login", map[string]string{"email": u.Email, "subject": c.Subject})
	case !errors.Is(err, store.ErrNotFound):
		return store.UserRow{}, err
	}
	domain := c.Email[strings.LastIndex(c.Email, "@")+1:]
	if !slices.Contains(d.o.OIDCAllowedDomains, domain) {
		return store.UserRow{}, errors.New("no such user, and the domain " + domain + " is not allowed to sign up")
	}
	role := d.o.OIDCDefaultRole
	if role == "" {
		role = RoleViewer
	}
	if ok, err := d.o.DB.RoleExists(ctx, role); err != nil {
		return store.UserRow{}, err
	} else if !ok || role == RoleAdmin {
		return store.UserRow{}, errors.New("the default role " + role + " is not usable")
	}
	u = store.UserRow{ID: ids.New("usr"), Email: c.Email, PasswordHash: ssoPasswordHash, TOTPLastStep: -1, Role: role, CreatedAt: d.o.Now()}
	if err := d.o.DB.InsertUser(ctx, u); errors.Is(err, store.ErrDuplicate) { // two first sign-ins at once
		if u, err = d.o.DB.UserByEmail(ctx, c.Email); err != nil || u.Disabled {
			return store.UserRow{}, errSSORefused
		}
		return u, nil
	} else if err != nil {
		return store.UserRow{}, err
	}
	return u, d.recordSSO(ctx, r, u.ID, "user.create", map[string]string{"email": u.Email, "role": role, "source": "sso", "subject": c.Subject})
}

// recordSSO audits a sign-in or a creation with the user as the actor.
func (d *Dashboard) recordSSO(ctx context.Context, r *http.Request, userID, action string, after map[string]string) error {
	org, err := d.orgID(ctx)
	if err != nil {
		return err
	}
	return d.o.DB.Write(ctx, func(tx store.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{OrgID: org, Actor: audit.Actor{Type: audit.ActorUser, ID: userID}, Action: action, Target: userID,
			After: after, IP: d.clientIP(r)})
	})
}
