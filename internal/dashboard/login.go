package dashboard

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/paymux/paymux/internal/auth"
	"github.com/paymux/paymux/internal/store"
)

const (
	msgBadLogin  = "Invalid email or password."
	msgBadCode   = "Invalid code. Check your authenticator app and try again."
	msgLockedOut = "Too many failed attempts. Try again in 15 minutes."
)

func totpAAD(userID string) []byte { return []byte("users.totp_secret:" + userID) }

func limitKey(email string, r *http.Request) string {
	return strings.ToLower(strings.TrimSpace(email)) + "|" + clientIP(r)
}

// dummyPasswordHash lets an unknown or disabled account cost the same time as
// a real one, so response timing does not reveal which emails exist.
func (d *Dashboard) dummyPasswordHash() string {
	d.dummyOnce.Do(func() { d.dummyHash, _ = auth.HashPassword("dummy password for timing") })
	return d.dummyHash
}

func (d *Dashboard) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.currentSession(r); ok {
		http.Redirect(w, r, prefix+"/", http.StatusSeeOther)
		return
	}
	d.render(w, http.StatusOK, "dash_login", view{Title: "Sign in", CSRF: d.preToken(w, r)})
}

func (d *Dashboard) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !d.validPre(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostForm.Get("email")))
	password := r.PostForm.Get("password")
	again := func(status int, msg string) {
		d.render(w, status, "dash_login", view{Title: "Sign in", CSRF: d.preToken(w, r), Email: email, Error: msg})
	}
	key := limitKey(email, r)
	now := d.o.Now()
	if d.lim.locked(key, now) {
		again(http.StatusTooManyRequests, msgLockedOut)
		return
	}

	u, err := d.o.DB.UserByEmail(r.Context(), email)
	hash := d.dummyPasswordHash()
	found := err == nil
	if found {
		hash = u.PasswordHash
	} else if err != store.ErrNotFound {
		d.fail(w, err)
		return
	}
	ok, _ := auth.VerifyPassword(hash, password)
	if !found || !ok || u.Disabled {
		d.lim.fail(key, now)
		again(http.StatusOK, msgBadLogin)
		return
	}

	// A password success does not clear the failure count: otherwise someone
	// who knows the password could guess TOTP codes without limit.
	if d.o.DevSkip2FA {
		d.lim.reset(key)
		if err := d.createSession(r.Context(), w, r, u); err != nil {
			d.fail(w, err)
			return
		}
		http.Redirect(w, r, prefix+"/", http.StatusSeeOther)
		return
	}
	d.issueTicket(w, u.ID)
	http.Redirect(w, r, prefix+"/login/totp", http.StatusSeeOther)
}

// totpSecret returns the user's TOTP secret, generating and storing one on
// first use. enrol is true until the first code has been accepted, so the
// secret is shown again if the user reloads before confirming.
func (d *Dashboard) totpSecret(r *http.Request, u store.UserRow) (secret, uri string, enrol bool, err error) {
	enrol = u.TOTPLastStep < 0
	if u.TOTPSecretEnc != nil {
		pt, err := d.o.Box.Open(u.TOTPSecretEnc, totpAAD(u.ID))
		if err != nil {
			return "", "", false, err
		}
		secret = string(pt)
		if enrol {
			uri = provisioningURI(secret, u.Email)
		}
		return secret, uri, enrol, nil
	}
	secret, uri, err = auth.NewTOTPSecret(issuer, u.Email)
	if err != nil {
		return "", "", false, err
	}
	enc, err := d.o.Box.Seal([]byte(secret), totpAAD(u.ID))
	if err != nil {
		return "", "", false, err
	}
	if err := d.o.DB.SetUserTOTP(r.Context(), u.ID, enc); err != nil {
		return "", "", false, err
	}
	return secret, uri, true, nil
}

// provisioningURI builds the otpauth:// URI for an existing secret, in the
// same format as auth.NewTOTPSecret.
func provisioningURI(secret, email string) string {
	esc := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + esc(issuer) + ":" + esc(email) + "?" + q.Encode()
}

func (d *Dashboard) pendingUser(w http.ResponseWriter, r *http.Request) (store.UserRow, bool) {
	id, ok := d.ticketUser(r)
	if ok {
		u, err := d.o.DB.UserByID(r.Context(), id)
		if err == nil && !u.Disabled {
			return u, true
		}
	}
	http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
	return store.UserRow{}, false
}

func (d *Dashboard) totpView(w http.ResponseWriter, r *http.Request, u store.UserRow, status int, msg string) {
	secret, uri, enrol, err := d.totpSecret(r, u)
	if err != nil {
		d.fail(w, err)
		return
	}
	v := view{Title: "Two-factor authentication", CSRF: d.preToken(w, r), Error: msg, Enrol: enrol, Email: u.Email}
	if enrol {
		v.TOTPSecret, v.TOTPURI = secret, uri
	}
	d.render(w, status, "dash_totp", v)
}

func (d *Dashboard) totpPage(w http.ResponseWriter, r *http.Request) {
	u, ok := d.pendingUser(w, r)
	if !ok {
		return
	}
	d.totpView(w, r, u, http.StatusOK, "")
}

func (d *Dashboard) totpSubmit(w http.ResponseWriter, r *http.Request) {
	if !d.validPre(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	u, ok := d.pendingUser(w, r)
	if !ok {
		return
	}
	key := limitKey(u.Email, r)
	now := d.o.Now()
	if d.lim.locked(key, now) {
		d.totpView(w, r, u, http.StatusTooManyRequests, msgLockedOut)
		return
	}
	secret, _, _, err := d.totpSecret(r, u)
	if err != nil {
		d.fail(w, err)
		return
	}
	code := strings.Join(strings.Fields(r.PostForm.Get("code")), "")
	step, valid := auth.ValidateTOTP(secret, code, now, u.TOTPLastStep)
	if valid {
		// Compare-and-set: of two concurrent requests with the same code only
		// one advances the step.
		valid, err = d.o.DB.AdvanceTOTPStep(r.Context(), u.ID, step)
		if err != nil {
			d.fail(w, err)
			return
		}
	}
	if !valid {
		d.lim.fail(key, now)
		d.totpView(w, r, u, http.StatusOK, msgBadCode)
		return
	}
	d.lim.reset(key)
	if err := d.createSession(r.Context(), w, r, u); err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/", http.StatusSeeOther)
}

func (d *Dashboard) logout(w http.ResponseWriter, r *http.Request, s *session) {
	if err := d.o.DB.DeleteSession(r.Context(), s.ID); err != nil {
		d.fail(w, err)
		return
	}
	d.clearCookie(w, cookieSession)
	http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
}
