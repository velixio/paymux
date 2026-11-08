package dashboard

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/velixio/paymux/internal/auth"
	"github.com/velixio/paymux/internal/outbound"
	"github.com/velixio/paymux/internal/secrets"
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
// and code, or the identity provider). A user with sign-in approval on gets a
// request to approve instead of a session. Nothing the browser sees may name where the
// approval happens.
func (d *Dashboard) finishSignIn(w http.ResponseWriter, r *http.Request, u store.UserRow, remember bool) {
	ctx := r.Context()
	if d.o.DevSkip2FA || !u.SigninApprovalOn() {
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
		IP: d.clientIP(r), Method: u.SigninMethod, Remember: remember}); err != nil {
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
		if !u.Disabled && u.SigninApprovalOn() {
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

// telegramLink makes a one-time link that connects the user's Telegram account,
// and shows it.
func (d *Dashboard) telegramLink(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	url, err := d.o.Outbound.BeginTelegramLink(ctx, org, s.User.ID)
	if err != nil {
		if outbound.IsSigninProblem(err) {
			d.renderAccount(w, r, s, http.StatusBadRequest, err.Error())
			return
		}
		d.fail(w, err)
		return
	}
	d.renderAccountLink(w, r, s, http.StatusOK, "", url)
}

// telegramUnlink forgets the user's Telegram account; approval goes with it.
func (d *Dashboard) telegramUnlink(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	if err := d.o.DB.UnlinkUserTelegram(ctx, s.User.ID, d.o.Now()); err != nil {
		d.fail(w, err)
		return
	}
	if err := d.record(ctx, r, s, "user.telegram_unlinked", s.User.ID, nil, nil); err != nil {
		d.o.Logger.Warn("dashboard: audit of sign-in approval", "err", err)
	}
	http.Redirect(w, r, prefix+"/account?n=unlinked", http.StatusSeeOther)
}

var signinPINPattern = regexp.MustCompile(`^[0-9]{6,12}$`)

// signinApprovalSet turns approval on with one method, tap (the default) or PIN,
// or changes the PIN. The bot is checked first: an approval it cannot serve
// would lock the user out at next sign-in.
func (d *Dashboard) signinApprovalSet(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	if s.User.TelegramUserID == 0 {
		d.renderAccount(w, r, s, http.StatusBadRequest, "Connect your Telegram account first.")
		return
	}
	method := r.PostForm.Get("method")
	if method == "" {
		method = store.SigninMethodTap
	}
	if method != store.SigninMethodTap && method != store.SigninMethodPIN {
		d.renderAccount(w, r, s, http.StatusBadRequest, "Choose tap or PIN.")
		return
	}
	pin := r.PostForm.Get("pin")
	if method == store.SigninMethodPIN {
		if !signinPINPattern.MatchString(pin) {
			d.renderAccount(w, r, s, http.StatusBadRequest, "The PIN must be 6 to 12 digits.")
			return
		}
		if pin != r.PostForm.Get("confirm_pin") {
			d.renderAccount(w, r, s, http.StatusBadRequest, "The two PINs differ.")
			return
		}
	}
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	if err := d.o.Outbound.CheckSigninApprovalChannel(ctx, org); err != nil {
		d.renderAccount(w, r, s, http.StatusBadRequest, err.Error())
		return
	}
	if method == store.SigninMethodPIN {
		enc, sealErr := d.o.Box.Seal([]byte(pin), secrets.AADSigninPIN(s.User.ID))
		if sealErr != nil {
			d.fail(w, sealErr)
			return
		}
		err = d.o.DB.SetUserSigninPIN(ctx, s.User.ID, enc, d.o.Now())
	} else {
		err = d.o.DB.SetUserSigninTap(ctx, s.User.ID, d.o.Now())
	}
	if errors.Is(err, store.ErrNotFound) {
		// Telegram was disconnected meanwhile, from another session.
		d.renderAccount(w, r, s, http.StatusBadRequest, "Connect your Telegram account first.")
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	if err := d.record(ctx, r, s, "user.signin_approval_set", s.User.ID, nil, map[string]string{"method": method}); err != nil {
		d.o.Logger.Warn("dashboard: audit of sign-in approval", "err", err)
	}
	http.Redirect(w, r, prefix+"/account?n=approval", http.StatusSeeOther)
}

// signinApprovalOff turns the approval step off. Telegram stays linked.
func (d *Dashboard) signinApprovalOff(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	if err := d.o.DB.ClearUserSigninApproval(ctx, s.User.ID, d.o.Now()); err != nil {
		d.fail(w, err)
		return
	}
	if err := d.record(ctx, r, s, "user.signin_approval_removed", s.User.ID, nil, nil); err != nil {
		d.o.Logger.Warn("dashboard: audit of sign-in approval", "err", err)
	}
	http.Redirect(w, r, prefix+"/account?n=approval_off", http.StatusSeeOther)
}
