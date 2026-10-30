package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/velixio/paymux/internal/auth"
	"github.com/velixio/paymux/internal/clientip"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
)

const (
	msgBadLogin = "Invalid email or password."
	msgBadCode  = "Invalid code. Check your authenticator app and try again."

	msgNeedEnrolCode = "Enter the enrolment code an admin gave you. Without it, two-factor sign-in cannot be set up."
	msgBadEnrolCode  = "That enrolment code is not valid or has expired. Ask an admin for a new one."
	msgLockedOut     = "Too many failed attempts. Try again in 15 minutes."
)

func totpAAD(userID string) []byte { return secrets.AADTOTP(userID) }

// limitKey is the per-address sign-in key. IPv6 clients are grouped by /64: a
// single customer has billions of addresses in theirs.
func (d *Dashboard) limitKey(email string, r *http.Request) string {
	return strings.ToLower(strings.TrimSpace(email)) + "|" + clientip.Group(d.clientIP(r))
}

// dummyPasswordHash lets an unknown or disabled account cost the same time as
// a real one, so response timing does not reveal which emails exist. It is made
// on first use (a hash is a 64 MiB affair); when the server is too busy to make
// it, "" is returned and the caller answers 503.
func (d *Dashboard) dummyPasswordHash() string {
	d.dummyMu.Lock()
	defer d.dummyMu.Unlock()
	if d.dummyHash == "" {
		d.dummyHash, _ = auth.HashPassword("dummy password for timing")
	}
	return d.dummyHash
}

const msgBusy = "The server is busy. Try again in a moment."

func (d *Dashboard) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.currentSession(r); ok {
		http.Redirect(w, r, prefix+"/", http.StatusSeeOther)
		return
	}
	d.render(w, http.StatusOK, "dash_login", view{Title: "Sign in", CSRF: d.preToken(w, r), NeedsSetup: d.SetupToken() != "", SSO: d.ssoEnabled(),
		RememberDays: d.rememberDays(r.Context())})
}

func (d *Dashboard) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !d.validPre(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostForm.Get("email")))
	password := r.PostForm.Get("password")
	again := func(status int, msg string) {
		d.render(w, status, "dash_login", view{Title: "Sign in", CSRF: d.preToken(w, r), Email: email, Error: msg, SSO: d.ssoEnabled(),
			RememberDays: d.rememberDays(r.Context())})
	}
	key := d.limitKey(email, r)
	now := d.o.Now()
	// Before any hashing: one address may only try so often, success or not.
	ipKey := clientip.Group(d.clientIP(r))
	if d.attempts.locked(ipKey, now) {
		again(http.StatusTooManyRequests, msgLockedOut)
		return
	}
	d.attempts.fail(ipKey, now)
	// The per-address key is not enough: the address behind a proxy is the
	// client's to choose. The account has its own ceilings.
	if d.lim.locked(key, now) || d.acctPw.locked(email, now) || d.acctTOTP.locked(email, now) {
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
	if hash == "" {
		again(http.StatusServiceUnavailable, msgBusy)
		return
	}
	ok, verr := auth.VerifyPassword(hash, password)
	if errors.Is(verr, auth.ErrBusy) {
		again(http.StatusServiceUnavailable, msgBusy)
		return
	}
	if !found || !ok || u.Disabled {
		d.lim.fail(key, now)
		d.acctPw.fail(email, now)
		again(http.StatusOK, msgBadLogin)
		return
	}

	// A password success does not clear the failure count: otherwise someone
	// who knows the password could guess TOTP codes without limit.
	remember := r.PostForm.Get("remember") != "" && d.rememberDays(r.Context()) > 0
	if d.o.DevSkip2FA {
		d.lim.reset(key)
		if err := d.createSession(r.Context(), w, r, u, remember); err != nil {
			d.fail(w, err)
			return
		}
		http.Redirect(w, r, prefix+"/", http.StatusSeeOther)
		return
	}
	d.issueTicket(w, u.ID, remember)
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
	id, _, _, ok := d.ticketUser(r)
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
	if u.TOTPLastStep < 0 {
		// Setting up the second factor needs the code an admin issued; knowing the
		// password is not enough to be shown the secret.
		open, err := d.o.DB.EnrolmentUnlocked(r.Context(), u.ID, d.o.Now())
		if err != nil {
			d.fail(w, err)
			return
		}
		if !open {
			d.render(w, status, "dash_totp", view{Title: "Two-factor authentication", CSRF: d.preToken(w, r), Error: msg, Email: u.Email, NeedEnrolCode: true})
			return
		}
	}
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
	_, ticket, remember, _ := d.ticketUser(r) // the choice made at the password step, signed into the ticket
	key := d.limitKey(u.Email, r)
	now := d.o.Now()
	email := strings.ToLower(strings.TrimSpace(u.Email))
	if d.acctTOTP.locked(email, now) {
		// Too many wrong codes for this account, from anywhere: the ticket is
		// dead and the password step has to be repeated (once the window passes).
		d.budgets.kill(ticket, now)
		d.clearCookie(w, cookieTicket)
		http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
		return
	}
	if d.lim.locked(key, now) {
		d.totpView(w, r, u, http.StatusTooManyRequests, msgLockedOut)
		return
	}
	if u.TOTPLastStep < 0 {
		open, err := d.o.DB.EnrolmentUnlocked(r.Context(), u.ID, now)
		if err != nil {
			d.fail(w, err)
			return
		}
		if !open {
			entered := r.PostForm.Get("enrol_code")
			good := false
			if strings.TrimSpace(entered) != "" {
				if good, err = d.o.DB.UnlockEnrolment(r.Context(), u.ID, hashEnrolmentCode(entered), now, enrolmentWindow); err != nil {
					d.fail(w, err)
					return
				}
				if !good {
					d.lim.fail(key, now)
					d.acctTOTP.fail(email, now)
					if d.budgets.fail(ticket, now) {
						d.clearCookie(w, cookieTicket)
						http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
						return
					}
				}
			}
			switch {
			case good:
				d.totpView(w, r, u, http.StatusOK, "")
			case strings.TrimSpace(entered) == "":
				d.totpView(w, r, u, http.StatusOK, msgNeedEnrolCode)
			default:
				d.totpView(w, r, u, http.StatusOK, msgBadEnrolCode)
			}
			return
		}
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
		d.acctTOTP.fail(email, now)
		if d.budgets.fail(ticket, now) {
			d.clearCookie(w, cookieTicket)
			http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
			return
		}
		d.totpView(w, r, u, http.StatusOK, msgBadCode)
		return
	}
	d.lim.reset(key)
	d.budgets.kill(ticket, now)
	if err := d.o.DB.ClearEnrolmentToken(r.Context(), u.ID); err != nil {
		d.fail(w, err)
		return
	}
	if err := d.createSession(r.Context(), w, r, u, remember); err != nil {
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
