package dashboard

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/velixio/paymux/internal/auth"
	"github.com/velixio/paymux/internal/outbound"
	"github.com/velixio/paymux/internal/store"
)

const (
	// cookieSigninWait holds the secret that lets this browser, and only this one,
	// collect the session once the sign-in is approved.
	cookieSigninWait = "pm_signin_wait"
	// signinCollectWindow is how long an approval waits for the browser to collect it.
	signinCollectWindow = 2 * time.Minute
	// signinWaitCookieAge outlives the request and the collect window's start.
	signinWaitCookieAge = 8 * 60

	signinWaitPath = prefix + "/login/waiting"
)

// finishSignIn ends every sign-in that has proved who the person is (password
// and code, or the identity provider). A user with a PIN gets a request to
// approve instead of a session. Nothing the browser sees may name where the
// approval happens.
func (d *Dashboard) finishSignIn(w http.ResponseWriter, r *http.Request, u store.UserRow, remember bool) {
	ctx := r.Context()
	if d.o.DevSkip2FA || u.SigninPINEnc == nil {
		if err := d.createSession(ctx, w, r, u, remember); err != nil {
			d.fail(w, err)
			return
		}
		http.Redirect(w, r, prefix+"/", http.StatusSeeOther)
		return
	}
	now := d.o.Now()
	// Counted in the database, unlike the code limiter: a PIN reply is read by
	// whichever instance's worker gets the job, not the one serving this browser.
	wrong, err := d.o.DB.WrongSigninPINsSince(ctx, u.ID, now.Add(-loginWindow))
	if err != nil {
		d.fail(w, err)
		return
	}
	if wrong >= accountTOTPFailures {
		d.clearCookie(w, cookieTicket)
		d.render(w, http.StatusTooManyRequests, "dash_login", view{Title: "Sign in", CSRF: d.preToken(w, r), Email: u.Email, Error: msgLockedOut, SSO: d.ssoEnabled(),
			RememberDays: d.rememberDays(ctx)})
		return
	}
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	token, hash := auth.NewSessionToken()
	if err := d.o.Outbound.BeginSigninApproval(ctx, outbound.SigninApprovalInput{OrgID: org, UserID: u.ID, BrowserHash: hash,
		IP: d.clientIP(r), Remember: remember}); err != nil {
		d.fail(w, err)
		return
	}
	d.setCookie(w, cookieSigninWait, token, signinWaitCookieAge)
	d.clearCookie(w, cookieTicket)
	http.Redirect(w, r, signinWaitPath, http.StatusSeeOther)
}

// signinRequest finds the request this browser's cookie stands for.
func (d *Dashboard) signinRequest(r *http.Request) (store.SigninApproval, error) {
	c, err := r.Cookie(cookieSigninWait)
	if err != nil {
		return store.SigninApproval{}, store.ErrNotFound
	}
	return d.o.DB.SigninApprovalByBrowser(r.Context(), auth.HashToken(c.Value))
}

func signinStillWaiting(a store.SigninApproval, now time.Time) bool {
	return a.Status == store.SigninPending && a.ExpiresAt.After(now)
}

// signinWaitPage shows the wait, ends it, or creates the session.
func (d *Dashboard) signinWaitPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := d.signinRequest(r)
	if errors.Is(err, store.ErrNotFound) {
		d.clearCookie(w, cookieSigninWait)
		http.Redirect(w, r, prefix+"/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	now := d.o.Now()
	if signinStillWaiting(a, now) {
		d.render(w, http.StatusOK, "dash_signin_wait", view{Title: "Waiting for external auth"})
		return
	}
	if a.Status == store.SigninApproved {
		u, err := d.o.DB.UserByID(ctx, a.UserID)
		if err != nil {
			d.fail(w, err)
			return
		}
		if !u.Disabled && u.SigninPINEnc != nil {
			ok, err := d.o.DB.CollectSigninApproval(ctx, a.ID, now.Add(-signinCollectWindow))
			if err != nil {
				d.fail(w, err)
				return
			}
			if ok {
				if err := d.createSession(ctx, w, r, u, a.Remember); err != nil {
					d.fail(w, err)
					return
				}
				d.clearCookie(w, cookieSigninWait)
				http.Redirect(w, r, prefix+"/", http.StatusSeeOther)
				return
			}
		}
	}
	d.clearCookie(w, cookieSigninWait)
	d.render(w, http.StatusOK, "dash_signin_wait", view{Title: "Sign in", SigninEnded: true})
}

// signinWaitStatus tells the waiting page when to load itself again. It changes
// nothing: the session is created by the page, from a full navigation.
func (d *Dashboard) signinWaitStatus(w http.ResponseWriter, r *http.Request) {
	state := "ready"
	a, err := d.signinRequest(r)
	switch {
	case err == nil:
		if signinStillWaiting(a, d.o.Now()) {
			state = "pending"
		}
	case !errors.Is(err, store.ErrNotFound):
		d.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, state)
}
