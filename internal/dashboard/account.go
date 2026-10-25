package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/auth"
	"github.com/paymux/paymux/internal/authz"
)

// The account pages belong to whoever is signed in: their sessions, their
// password, and the confirmation that sensitive actions ask for. They need a
// session and a CSRF token but no permission: everyone may manage their own.
const selfOnly authz.Action = ""

// recentAuthWindow is how long after the last full sign-in (password and second
// factor) the sensitive actions below are allowed without confirming again.
const recentAuthWindow = 12 * time.Hour

type sessionRow struct {
	ID, Created, LastSeen, Expires, Network, Device string
	Remembered, Current                             bool
}

// recent wraps a handler of a sensitive action. It runs only when the user
// proved who they are within recentAuthWindow; otherwise the user is sent to
// confirm with the password and the authenticator code and comes back. The
// closed list of such actions: creating and rotating API keys; creating users,
// changing roles, issuing enrolment codes and allowing single sign-on for an
// account; creating, changing and connecting provider accounts; adding webhook
// endpoints and domains; changing org, app, appearance, branding, tax and
// exchange-rate settings.
func (d *Dashboard) recent(h authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, s *session) {
		if d.o.Now().Sub(s.AuthenticatedAt) <= recentAuthWindow {
			h(w, r, s)
			return
		}
		if s.User.PasswordHash == ssoPasswordHash {
			// No password here to confirm with: the identity provider has to say it again.
			_ = d.o.DB.DeleteSession(r.Context(), s.ID)
			d.clearCookie(w, cookieSession)
			http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
			return
		}
		back := r.URL.RequestURI()
		if r.Method != http.MethodGet {
			back = prefix + "/overview" // the form that was posted is not replayed
			if ref, err := url.Parse(r.Referer()); err == nil && ref.Path != "" {
				back = ref.Path
			}
		}
		http.Redirect(w, r, prefix+"/reauth?next="+url.QueryEscape(safeNext(back, prefix+"/overview")), http.StatusSeeOther)
	}
}

func (d *Dashboard) reauthPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderReauth(w, r, s, http.StatusOK, "", r.URL.Query().Get("next"))
}

func (d *Dashboard) renderReauth(w http.ResponseWriter, r *http.Request, s *session, status int, msg, next string) {
	v := d.shell(r, s, "", "Confirm it is you")
	v.Error, v.Next = msg, safeNext(next, prefix+"/overview")
	v.NeedCode = !d.o.DevSkip2FA && s.User.TOTPLastStep >= 0
	d.render(w, status, "dash_reauth", v)
}

// reauthSubmit checks the password and the code again, with the same limits as
// signing in, and marks the session as freshly authenticated.
func (d *Dashboard) reauthSubmit(w http.ResponseWriter, r *http.Request, s *session) {
	next := r.PostForm.Get("next")
	u := s.User
	email := strings.ToLower(strings.TrimSpace(u.Email))
	key := d.limitKey(email, r)
	now := d.o.Now()
	if d.lim.locked(key, now) || d.acctPw.locked(email, now) || d.acctTOTP.locked(email, now) {
		d.renderReauth(w, r, s, http.StatusTooManyRequests, msgLockedOut, next)
		return
	}
	fail := func(msg string, code bool) {
		d.lim.fail(key, now)
		if code {
			d.acctTOTP.fail(email, now)
		} else {
			d.acctPw.fail(email, now)
		}
		d.renderReauth(w, r, s, http.StatusOK, msg, next)
	}
	ok, err := auth.VerifyPassword(u.PasswordHash, r.PostForm.Get("password"))
	if errors.Is(err, auth.ErrBusy) {
		d.renderReauth(w, r, s, http.StatusServiceUnavailable, msgBusy, next)
		return
	}
	if err != nil || !ok {
		fail(msgBadLogin, false)
		return
	}
	if !d.o.DevSkip2FA && u.TOTPLastStep >= 0 {
		secret, _, _, err := d.totpSecret(r, u)
		if err != nil {
			d.fail(w, err)
			return
		}
		code := strings.Join(strings.Fields(r.PostForm.Get("code")), "")
		step, valid := auth.ValidateTOTP(secret, code, now, u.TOTPLastStep)
		if valid {
			if valid, err = d.o.DB.AdvanceTOTPStep(r.Context(), u.ID, step); err != nil {
				d.fail(w, err)
				return
			}
		}
		if !valid {
			fail(msgBadCode, true)
			return
		}
	}
	d.lim.reset(key)
	if err := d.o.DB.SetSessionAuthenticated(r.Context(), s.ID, now); err != nil {
		d.fail(w, err)
		return
	}
	if err := d.record(r.Context(), r, s, "session.reauth", s.ID, nil, nil); err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, safeNext(next, prefix+"/overview"), http.StatusSeeOther)
}

// ---- your sessions ----

func (d *Dashboard) accountPage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderAccount(w, r, s, http.StatusOK, "")
}

func (d *Dashboard) renderAccount(w http.ResponseWriter, r *http.Request, s *session, status int, errMsg string) {
	rows, err := d.o.DB.ListUserSessions(r.Context(), s.User.ID)
	if err != nil {
		d.fail(w, err)
		return
	}
	v := d.shell(r, s, "", "Your sessions")
	v.Error, v.Notice = errMsg, accountNotices[r.URL.Query().Get("n")]
	for _, x := range rows {
		ua := x.UserAgent
		if len(ua) > 80 {
			ua = ua[:80] + "..."
		}
		v.Sessions = append(v.Sessions, sessionRow{ID: x.ID, Created: d.fmtTime(x.CreatedAt), LastSeen: d.fmtTime(x.LastSeenAt), Expires: d.fmtTime(x.ExpiresAt),
			Network: x.IPHint, Device: ua, Remembered: x.Remember, Current: x.ID == s.ID})
	}
	d.render(w, status, "dash_account", v)
}

var accountNotices = map[string]string{
	"revoked": "That session was signed out.",
	"others":  "Every other session was signed out.",
}

func (d *Dashboard) sessionRevoke(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.PathValue("id")
	ok, err := d.o.DB.DeleteUserSession(r.Context(), s.User.ID, id) // scoped to the signed-in user
	if err != nil {
		d.fail(w, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := d.record(r.Context(), r, s, "session.revoke", id, nil, nil); err != nil {
		d.fail(w, err)
		return
	}
	if id == s.ID {
		d.clearCookie(w, cookieSession)
		http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, prefix+"/account?n=revoked", http.StatusSeeOther)
}

func (d *Dashboard) sessionsRevokeOthers(w http.ResponseWriter, r *http.Request, s *session) {
	n, err := d.o.DB.DeleteUserSessionsExcept(r.Context(), s.User.ID, s.ID)
	if err != nil {
		d.fail(w, err)
		return
	}
	if err := d.record(r.Context(), r, s, "session.revoke_others", s.User.ID, nil, map[string]int{"revoked": n}); err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/account?n=others", http.StatusSeeOther)
}

// passwordChange replaces the password and ends every session of the user, this
// one included: whoever had the old password, or a stolen cookie, is out.
func (d *Dashboard) passwordChange(w http.ResponseWriter, r *http.Request, s *session) {
	u := s.User
	email := strings.ToLower(strings.TrimSpace(u.Email))
	key := d.limitKey(email, r)
	now := d.o.Now()
	if u.PasswordHash == ssoPasswordHash {
		d.renderAccount(w, r, s, http.StatusBadRequest, "This account signs in through single sign-on and has no password here.")
		return
	}
	if d.lim.locked(key, now) || d.acctPw.locked(email, now) {
		d.renderAccount(w, r, s, http.StatusTooManyRequests, msgLockedOut)
		return
	}
	ok, err := auth.VerifyPassword(u.PasswordHash, r.PostForm.Get("current_password"))
	if errors.Is(err, auth.ErrBusy) {
		d.renderAccount(w, r, s, http.StatusServiceUnavailable, msgBusy)
		return
	}
	if err != nil || !ok {
		d.lim.fail(key, now)
		d.acctPw.fail(email, now)
		d.renderAccount(w, r, s, http.StatusBadRequest, "The current password is not right.")
		return
	}
	next := r.PostForm.Get("new_password")
	if next != r.PostForm.Get("confirm_password") {
		d.renderAccount(w, r, s, http.StatusBadRequest, "The two new passwords differ.")
		return
	}
	hash, err := auth.HashPassword(next)
	switch {
	case errors.Is(err, auth.ErrWeakPassword):
		d.renderAccount(w, r, s, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, auth.ErrBusy):
		d.renderAccount(w, r, s, http.StatusServiceUnavailable, msgBusy)
		return
	case err != nil:
		d.fail(w, err)
		return
	}
	if err := d.o.DB.SetUserPassword(r.Context(), u.ID, hash); err != nil {
		d.fail(w, err)
		return
	}
	if err := d.o.DB.DeleteUserSessions(r.Context(), u.ID); err != nil {
		d.fail(w, err)
		return
	}
	if err := d.record(r.Context(), r, s, "user.password_change", u.ID, nil, nil); err != nil {
		d.o.Logger.Warn("dashboard: audit of password change", "err", err)
	}
	d.lim.reset(key)
	d.clearCookie(w, cookieSession)
	http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
}

// userSessionsRevoke is the admin's "sign out everywhere" for any user.
func (d *Dashboard) userSessionsRevoke(w http.ResponseWriter, r *http.Request, s *session) {
	u, ok := d.target(w, r)
	if !ok {
		return
	}
	if err := d.o.DB.DeleteUserSessions(r.Context(), u.ID); err != nil {
		d.fail(w, err)
		return
	}
	if err := d.record(r.Context(), r, s, "user.sessions_revoke", u.ID, nil, nil); err != nil {
		d.fail(w, err)
		return
	}
	if u.ID == s.User.ID {
		d.clearCookie(w, cookieSession)
		http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, prefix+"/users?n=signedout", http.StatusSeeOther)
}

func (d *Dashboard) accountRoutes() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/account", d.authed(selfOnly, d.accountPage))
	m.HandleFunc("POST "+prefix+"/account/password", d.authed(selfOnly, d.passwordChange))
	m.HandleFunc("POST "+prefix+"/account/sessions/revoke-others", d.authed(selfOnly, d.sessionsRevokeOthers))
	m.HandleFunc("POST "+prefix+"/account/sessions/{id}/revoke", d.authed(selfOnly, d.sessionRevoke))
	m.HandleFunc("GET "+prefix+"/reauth", d.authed(selfOnly, d.reauthPage))
	m.HandleFunc("POST "+prefix+"/reauth", d.authed(selfOnly, d.reauthSubmit))
	m.HandleFunc("POST "+prefix+"/users/{id}/sessions/revoke", d.authed(authz.UserManage, d.recent(d.userSessionsRevoke)))
}
