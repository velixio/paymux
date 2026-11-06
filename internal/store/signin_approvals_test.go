package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var signinT0 = time.UnixMilli(1_800_000_000_000).UTC()

func signinUser(t *testing.T, db *store.DB, id string) {
	t.Helper()
	if err := db.InsertUser(ctx, store.UserRow{ID: id, Email: id + "@example.com", PasswordHash: "h", Role: "admin", CreatedAt: signinT0}); err != nil {
		t.Fatal(err)
	}
}

func insertSignin(t *testing.T, db *store.DB, id, userID string, at time.Time) store.SigninApproval {
	t.Helper()
	a := store.SigninApproval{ID: id, OrgID: "org_1", UserID: userID, BrowserHash: "hash_" + id, IP: "203.0.113.9",
		Status: store.SigninPending, Remember: true, CreatedAt: at, ExpiresAt: at.Add(5 * time.Minute)}
	if err := db.Write(ctx, func(tx store.Tx) error { return store.InsertSigninApproval(ctx, tx, a) }); err != nil {
		t.Fatal(err)
	}
	return a
}

func signinByHash(t *testing.T, db *store.DB, id string) store.SigninApproval {
	t.Helper()
	a, err := db.SigninApprovalByBrowser(ctx, "hash_"+id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestInsertEndsTheUsersEarlierPendingRequest(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		signinUser(t, db, "usr_1")
		signinUser(t, db, "usr_2")
		insertSignin(t, db, "a", "usr_1", signinT0)
		insertSignin(t, db, "other", "usr_2", signinT0)
		insertSignin(t, db, "b", "usr_1", signinT0.Add(time.Second))
		if got := signinByHash(t, db, "a"); got.Status != store.SigninEnded || got.DecidedAt.IsZero() {
			t.Fatalf("first request: %+v", got)
		}
		if got := signinByHash(t, db, "b"); got.Status != store.SigninPending || !got.Remember || got.IP != "203.0.113.9" || !got.ExpiresAt.Equal(signinT0.Add(5*time.Minute+time.Second)) {
			t.Fatalf("second request: %+v", got)
		}
		if got := signinByHash(t, db, "other"); got.Status != store.SigninPending {
			t.Fatalf("another user's request: %+v", got)
		}
		if _, err := db.SigninApprovalByBrowser(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown browser: %v", err)
		}
	})
}

func TestCollectWorksOnce(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		signinUser(t, db, "usr_1")
		insertSignin(t, db, "a", "usr_1", signinT0)
		if ok, err := db.CollectSigninApproval(ctx, "a", signinT0); err != nil || ok {
			t.Fatalf("collected while pending: %v %v", ok, err)
		}
		decided := signinT0.Add(time.Minute)
		if _, err := db.RecordSigninReply(ctx, "a", true, 5, decided); err != nil {
			t.Fatal(err)
		}
		if ok, err := db.CollectSigninApproval(ctx, "a", decided.Add(time.Millisecond)); err != nil || ok {
			t.Fatalf("collected an approval that is too old: %v %v", ok, err)
		}
		if ok, err := db.CollectSigninApproval(ctx, "a", decided); err != nil || !ok {
			t.Fatalf("first collect: %v %v", ok, err)
		}
		if ok, err := db.CollectSigninApproval(ctx, "a", decided); err != nil || ok {
			t.Fatalf("second collect: %v %v", ok, err)
		}
		if got := signinByHash(t, db, "a"); got.Status != store.SigninCollected {
			t.Fatalf("status %q", got.Status)
		}
	})
}

func TestRecordSigninReply(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		signinUser(t, db, "usr_1")
		at := signinT0.Add(time.Minute)

		insertSignin(t, db, "ok", "usr_1", signinT0)
		got, err := db.RecordSigninReply(ctx, "ok", true, 5, at)
		if err != nil || got.Status != store.SigninApproved || !got.DecidedAt.Equal(at) {
			t.Fatalf("correct reply: %+v %v", got, err)
		}
		// Decided already: nothing changes.
		if got, err = db.RecordSigninReply(ctx, "ok", false, 5, at); err != nil || got.Status != store.SigninApproved || got.WrongAttempts != 0 {
			t.Fatalf("reply after approval: %+v %v", got, err)
		}

		insertSignin(t, db, "bad", "usr_1", signinT0)
		for i := 1; i <= 5; i++ {
			got, err = db.RecordSigninReply(ctx, "bad", false, 5, at)
			if err != nil || got.WrongAttempts != i {
				t.Fatalf("wrong %d: %+v %v", i, got, err)
			}
			if want := i == 5; (got.Status == store.SigninEnded) != want {
				t.Fatalf("wrong %d: status %q", i, got.Status)
			}
		}
		if n, err := db.WrongSigninPINsSince(ctx, "usr_1", signinT0); err != nil || n != 5 {
			t.Fatalf("wrong count %d: %v", n, err)
		}
		if n, err := db.WrongSigninPINsSince(ctx, "usr_1", signinT0.Add(time.Second)); err != nil || n != 0 {
			t.Fatalf("wrong count outside the window %d: %v", n, err)
		}

		insertSignin(t, db, "late", "usr_1", signinT0)
		if got, err = db.RecordSigninReply(ctx, "late", true, 5, signinT0.Add(5*time.Minute)); err != nil || got.Status != store.SigninPending {
			t.Fatalf("reply at expiry: %+v %v", got, err)
		}
	})
}

func TestClaimSigninPollIsExclusivePerBot(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		until := signinT0.Add(30 * time.Second)
		if off, ok, err := db.ClaimSigninPoll(ctx, "1", signinT0, until); err != nil || !ok || off != 0 {
			t.Fatalf("first claim: %d %v %v", off, ok, err)
		}
		if _, ok, err := db.ClaimSigninPoll(ctx, "1", signinT0.Add(time.Second), until); err != nil || ok {
			t.Fatalf("claim during the lease: %v %v", ok, err)
		}
		if off, ok, err := db.ClaimSigninPoll(ctx, "2", signinT0, until); err != nil || !ok || off != 0 {
			t.Fatalf("another bot: %d %v %v", off, ok, err)
		}
		if _, ok, err := db.ClaimSigninPoll(ctx, "1", until, until.Add(time.Second)); err != nil || !ok {
			t.Fatalf("claim after the lease: %v %v", ok, err)
		}
		if err := db.ReleaseSigninPoll(ctx, "1", until.Add(time.Second), 42); err != nil {
			t.Fatal(err)
		}
		if off, ok, err := db.ClaimSigninPoll(ctx, "1", until, until.Add(time.Second)); err != nil || !ok || off != 42 {
			t.Fatalf("claim after release: %d %v %v", off, ok, err)
		}
	})
}

func TestSettingOrClearingThePINEndsPendingRequests(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		signinUser(t, db, "usr_1")
		insertSignin(t, db, "a", "usr_1", signinT0)
		if err := db.SetUserSigninPIN(ctx, "usr_1", []byte("sealed"), signinT0); err != nil {
			t.Fatal(err)
		}
		u, err := db.UserByID(ctx, "usr_1")
		if err != nil || string(u.SigninPINEnc) != "sealed" {
			t.Fatalf("after set: %q %v", u.SigninPINEnc, err)
		}
		if got := signinByHash(t, db, "a"); got.Status != store.SigninEnded {
			t.Fatalf("after set: %q", got.Status)
		}
		insertSignin(t, db, "b", "usr_1", signinT0)
		if err := db.ClearUserSigninPIN(ctx, "usr_1", signinT0); err != nil {
			t.Fatal(err)
		}
		u, err = db.UserByID(ctx, "usr_1")
		if err != nil || u.SigninPINEnc != nil {
			t.Fatalf("after clear: %q %v", u.SigninPINEnc, err)
		}
		if got := signinByHash(t, db, "b"); got.Status != store.SigninEnded {
			t.Fatalf("after clear: %q", got.Status)
		}
		if err := db.ClearUserSigninPIN(ctx, "nobody", signinT0); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown user: %v", err)
		}
	})
}

func TestSigninApprovalWorkLists(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		signinUser(t, db, "usr_1")
		if open, err := db.HasOpenSigninApprovals(ctx); err != nil || open {
			t.Fatalf("open with no rows: %v %v", open, err)
		}
		insertSignin(t, db, "a", "usr_1", signinT0)
		if open, err := db.HasOpenSigninApprovals(ctx); err != nil || !open {
			t.Fatalf("open with a pending row: %v %v", open, err)
		}
		if orgs, err := db.PendingSigninOrgs(ctx); err != nil || len(orgs) != 1 || orgs[0] != "org_1" {
			t.Fatalf("pending orgs: %v %v", orgs, err)
		}
		send, err := db.SigninApprovalsToSend(ctx, signinT0)
		if err != nil || len(send) != 1 || send[0].ID != "a" {
			t.Fatalf("to send: %+v %v", send, err)
		}
		if won, err := db.SetSigninNotification(ctx, "a", -100123, 77); err != nil || !won {
			t.Fatalf("first notification: %v %v", won, err)
		}
		// A second worker's message must not replace the one replies are matched to.
		if won, err := db.SetSigninNotification(ctx, "a", -100123, 88); err != nil || won {
			t.Fatalf("second notification: %v %v", won, err)
		}
		if send, err = db.SigninApprovalsToSend(ctx, signinT0); err != nil || len(send) != 0 {
			t.Fatalf("sent already: %+v %v", send, err)
		}
		got, err := db.SigninApprovalByNotification(ctx, -100123, 77)
		if err != nil || got.ID != "a" || got.ChatID != -100123 || got.MessageID != 77 {
			t.Fatalf("by notification: %+v %v", got, err)
		}
		if _, err := db.SigninApprovalByNotification(ctx, -1, 77); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("other chat: %v", err)
		}
		if clear, err := db.SigninApprovalsToClear(ctx); err != nil || len(clear) != 0 {
			t.Fatalf("clear while pending: %+v %v", clear, err)
		}

		// Past its time it ends, and its notification waits to be deleted.
		if err := db.ExpireSigninApprovals(ctx, signinT0.Add(5*time.Minute-time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if got := signinByHash(t, db, "a"); got.Status != store.SigninPending {
			t.Fatalf("expired early: %q", got.Status)
		}
		if err := db.ExpireSigninApprovals(ctx, signinT0.Add(5*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if got := signinByHash(t, db, "a"); got.Status != store.SigninEnded {
			t.Fatalf("not expired: %q", got.Status)
		}
		if orgs, err := db.PendingSigninOrgs(ctx); err != nil || len(orgs) != 0 {
			t.Fatalf("pending orgs after expiry: %v %v", orgs, err)
		}
		clear, err := db.SigninApprovalsToClear(ctx)
		if err != nil || len(clear) != 1 || clear[0].ID != "a" {
			t.Fatalf("to clear: %+v %v", clear, err)
		}
		if open, err := db.HasOpenSigninApprovals(ctx); err != nil || !open {
			t.Fatalf("open with a notification to delete: %v %v", open, err)
		}
		if err := db.MarkSigninCleared(ctx, "a"); err != nil {
			t.Fatal(err)
		}
		if clear, err = db.SigninApprovalsToClear(ctx); err != nil || len(clear) != 0 {
			t.Fatalf("cleared already: %+v %v", clear, err)
		}
		if open, err := db.HasOpenSigninApprovals(ctx); err != nil || open {
			t.Fatalf("open when all done: %v %v", open, err)
		}

		// Ended rows stay long enough to recognise late replies, then go.
		if err := db.PurgeSigninApprovals(ctx, signinT0); err != nil {
			t.Fatal(err)
		}
		signinByHash(t, db, "a")
		if err := db.PurgeSigninApprovals(ctx, signinT0.Add(time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SigninApprovalByBrowser(ctx, "hash_a"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("not purged: %v", err)
		}
	})
}

// An approval given before one of these must not be collected after it.
func TestApprovedRequestsEndWithLaterChanges(t *testing.T) {
	for name, change := range map[string]func(db *store.DB) error{
		"a newer sign-in": func(db *store.DB) error {
			return db.Write(ctx, func(tx store.Tx) error {
				return store.InsertSigninApproval(ctx, tx, store.SigninApproval{ID: "newer", OrgID: "org_1", UserID: "usr_1", BrowserHash: "hash_newer", IP: "x",
					Status: store.SigninPending, CreatedAt: signinT0, ExpiresAt: signinT0.Add(time.Minute)})
			})
		},
		"a new PIN":        func(db *store.DB) error { return db.SetUserSigninPIN(ctx, "usr_1", []byte("sealed"), signinT0) },
		"removing the PIN": func(db *store.DB) error { return db.ClearUserSigninPIN(ctx, "usr_1", signinT0) },
		"signing out other sessions": func(db *store.DB) error {
			_, err := db.DeleteUserSessionsExcept(ctx, "usr_1", "ses_none")
			return err
		},
		"signing out everywhere": func(db *store.DB) error { return db.DeleteUserSessions(ctx, "usr_1") },
	} {
		t.Run(name, func(t *testing.T) {
			storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
				signinUser(t, db, "usr_1")
				insertSignin(t, db, "a", "usr_1", signinT0)
				if _, err := db.RecordSigninReply(ctx, "a", true, 5, signinT0); err != nil {
					t.Fatal(err)
				}
				if err := change(db); err != nil {
					t.Fatal(err)
				}
				if ok, err := db.CollectSigninApproval(ctx, "a", signinT0); err != nil || ok {
					t.Fatalf("collected: %v %v", ok, err)
				}
				if got := signinByHash(t, db, "a"); got.Status != store.SigninEnded {
					t.Fatalf("status %q", got.Status)
				}
			})
		})
	}
}

// A holder that outlived its lease cannot free the new holder's lease or move the
// reading position backward.
func TestAnExpiredPollHolderCannotDisturbTheNextOne(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		first, second := signinT0.Add(30*time.Second), signinT0.Add(60*time.Second)
		if _, ok, err := db.ClaimSigninPoll(ctx, "1", signinT0, first); err != nil || !ok {
			t.Fatalf("first claim: %v %v", ok, err)
		}
		if _, ok, err := db.ClaimSigninPoll(ctx, "1", first, second); err != nil || !ok {
			t.Fatalf("second claim: %v %v", ok, err)
		}
		if err := db.ReleaseSigninPoll(ctx, "1", first, 42); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := db.ClaimSigninPoll(ctx, "1", first.Add(time.Second), second.Add(time.Minute)); err != nil || ok {
			t.Fatalf("the old holder freed the new lease: %v %v", ok, err)
		}
		if err := db.ReleaseSigninPoll(ctx, "1", second, 30); err != nil {
			t.Fatal(err)
		}
		off, ok, err := db.ClaimSigninPoll(ctx, "1", second, second.Add(time.Minute))
		if err != nil || !ok || off != 42 {
			t.Fatalf("after both released: offset %d, %v %v", off, ok, err)
		}
	})
}
