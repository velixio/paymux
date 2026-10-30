package dashboard

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var (
	enrolSecretRE = regexp.MustCompile(`<code id="totp-secret">([A-Z2-7]+)</code>`)
	enrolCodeRE   = regexp.MustCompile(`<code id="enrolment-code">([a-z]{6}-[a-z]{6}-[a-z]{6}-[a-z]{6})</code>`)
)

func (e *env) users() Users { return Users{DB: e.db, Now: e.clk.Now} }

// Whoever first signs in with the password of a user who has not set up
// two-factor authentication used to be shown the secret and could enrol their
// own authenticator. Now an admin-issued one-time code is needed first.
func TestEnrolmentNeedsTheAdminIssuedCode(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, _ := e.seedUser("newbie@example.com", "viewer", false)
		if r, _ := e.passwordStep(e.c, "newbie@example.com", goodPW); r.StatusCode != 303 {
			t.Fatalf("password step: %d", r.StatusCode)
		}
		_, page := e.get(e.c, "/dashboard/login/totp")
		if enrolSecretRE.MatchString(page) || strings.Contains(page, "otpauth://") {
			t.Fatal("the secret is shown to someone who only knows the password")
		}
		// A code guessed at the first step: nothing is revealed, nobody is signed in.
		r, body := e.post(e.c, "/dashboard/login/totp", url.Values{"enrol_code": {"aaaaaa-bbbbbb-cccccc-dddddd"}, "csrf": {csrfOf(t, page)}})
		if sessionCookie(r) != nil || enrolSecretRE.MatchString(body) {
			t.Fatalf("a wrong enrolment code was accepted: %d", r.StatusCode)
		}

		token, err := e.users().IssueEnrolmentToken(ctx, id)
		if err != nil || !regexp.MustCompile(`^[a-z]{6}(-[a-z]{6}){3}$`).MatchString(token) {
			t.Fatalf("token %q: %v", token, err)
		}
		_, page = e.get(e.c, "/dashboard/login/totp")
		_, body = e.post(e.c, "/dashboard/login/totp", url.Values{"enrol_code": {token}, "csrf": {csrfOf(t, page)}})
		m := enrolSecretRE.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("the secret is not shown after the right code:\n%s", body)
		}
		r, _ = e.post(e.c, "/dashboard/login/totp", url.Values{"code": {totpFor(m[1], e.clk.Now(), 0)}, "csrf": {csrfOf(t, body)}})
		if r.StatusCode != 303 || sessionCookie(r) == nil {
			t.Fatalf("confirm: %d", r.StatusCode)
		}
		// The code is spent: a second sign-in cannot enrol again.
		if n := countEnrolmentRows(t, db, id); n != 0 {
			t.Errorf("%d enrolment rows left after enrolling", n)
		}
	})
}

func TestEnrolmentCodeExpiresAndAResetNeedsANewOne(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, _ := e.seedUser("newbie@example.com", "viewer", false)
		token, _ := e.users().IssueEnrolmentToken(ctx, id)
		e.clk.Add(8 * 24 * time.Hour)
		e.passwordStep(e.c, "newbie@example.com", goodPW)
		_, page := e.get(e.c, "/dashboard/login/totp")
		_, body := e.post(e.c, "/dashboard/login/totp", url.Values{"enrol_code": {token}, "csrf": {csrfOf(t, page)}})
		if enrolSecretRE.MatchString(body) {
			t.Error("an expired enrolment code worked")
		}
		// Resetting a user's two-factor sign-in invalidates earlier codes.
		fresh, _ := e.users().IssueEnrolmentToken(ctx, id)
		if err := e.users().ResetTOTP(ctx, "newbie@example.com"); err != nil {
			t.Fatal(err)
		}
		c2 := e.client()
		e.passwordStep(c2, "newbie@example.com", goodPW)
		_, page = e.get(c2, "/dashboard/login/totp")
		_, body = e.post(c2, "/dashboard/login/totp", url.Values{"enrol_code": {fresh}, "csrf": {csrfOf(t, page)}})
		if enrolSecretRE.MatchString(body) {
			t.Error("a code issued before the reset worked")
		}
	})
}

// An admin creating a user is shown the code once; only users.manage may issue one.
func TestAdminsGetTheEnrolmentCodeWhenCreatingOrResetting(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/users"))
		r, body := e.post(admin, "/dashboard/users", url.Values{"email": {"new@example.com"}, "role": {"viewer"}, "password": {goodPW}, "csrf": {tok}})
		m := enrolCodeRE.FindStringSubmatch(body)
		if r.StatusCode != 200 || m == nil {
			t.Fatalf("create: %d, no enrolment code on the page:\n%s", r.StatusCode, body)
		}
		if again := e.pageOK(admin, "/dashboard/users"); strings.Contains(again, m[1]) {
			t.Error("the code is shown again")
		}
		u, _ := db.UserByEmail(ctx, "new@example.com")
		r, body = e.post(admin, "/dashboard/users/"+u.ID+"/enrolment", url.Values{"csrf": {tok}})
		m2 := enrolCodeRE.FindStringSubmatch(body)
		if r.StatusCode != 200 || m2 == nil || m2[1] == m[1] {
			t.Fatalf("new code: %d %v", r.StatusCode, m2)
		}
		if auditCount(t, db, "user.enrolment_code") < 1 {
			t.Error("issuing a code is not audited")
		}
		viewer := e.roleClient("viewer")
		vt := csrfOf(t, e.pageOK(viewer, "/dashboard/payments"))
		if r, _ := e.post(viewer, "/dashboard/users/"+u.ID+"/enrolment", url.Values{"csrf": {vt}}); r.StatusCode != 403 {
			t.Errorf("a viewer issued a code: %d", r.StatusCode)
		}
	})
}

func countEnrolmentRows(t *testing.T, db *store.DB, userID string) int {
	t.Helper()
	ok, err := db.HasEnrolmentToken(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		return 1
	}
	return 0
}
