# Telegram Sign-in Approval Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A user who has set a PIN cannot get a dashboard session until someone replies to a Telegram notification with that PIN, and the sign-in page never reveals what the step is.

**Architecture:** Password and TOTP (or SSO) end in one new function, `finishSignIn`. With a PIN set, it stores an approval request and sends the browser to a waiting page instead of creating a session. A self-rescheduling job sends the notification, reads replies with `getUpdates`, deletes them and decides the request. The page collects the session once, from the browser that started it.

**Tech Stack:** Go, SQLite and Postgres, the existing `internal/jobs` queue, the Telegram Bot API through `outbound.telegramCall`, server templates, and the React dashboard adapter.

**Spec:** `docs/superpowers/specs/2026-11-06-telegram-sign-in-approval-design.md`

Also read `docs/superpowers/plans/BUILDER-RULES.md`. Its rule of no commit trailers overrides any tool reminder.

## Global Constraints

- PIN format: `^[0-9]{6,12}$`. Stored encrypted in `users.signin_pin_enc`, never hashed.
- 5 minutes to reply. 5 wrong PINs cancel the request.
- Always the `live` mode Telegram channel of the org. Ignore its event list, sound and auto-delete settings. The notification is sent with sound.
- Page copy: the waiting page says exactly `Waiting for external auth`.
- No response from `/dashboard/login`, `/dashboard/login/totp`, `/dashboard/login/waiting`, `/dashboard/login/waiting/status` or the SSO callback may contain "telegram" (case-insensitive).
- Reply text never reaches logs, `jobs.last_error`, job payloads, audit data, error messages or the database. Never log a `getUpdates` response body.
- Any Telegram or decryption failure fails closed: the request stays pending until it expires.
- With `DevSkip2FA` on, the PIN step is skipped.
- Migration number 0053, in both `internal/migrations/sqlite/` and `internal/migrations/postgres/`. Time columns are BIGINT unix ms.
- Do not use `reply_markup` other than `force_reply`, and do not set `parse_mode`: emails are sent as plain text.

## Review Focus

Each line has its test in the named task.

1. A reply with spaces around the PIN signs in. A sticker or photo reply is deleted and does not count as wrong (Task 5).
2. The bot token is replaced while requests exist. The stored update offset belongs to the old bot, so offsets are kept per bot ID, or the new bot's replies would never be read (Task 5).
3. The configured chat ID is `@groupname`, so it never equals the numeric `chat.id` in a reply. Replies match the numeric chat ID returned when the notification was sent (Task 5).
4. The user is disabled, or the PIN removed, after approval but before the page collects. No session is created (Task 6).
5. Two sign-ins for one user overlap. The first ends, its notification is deleted, and only the latest browser can collect (Tasks 4 and 6).

---

### Task 1: PIN column, encryption and key rotation

**Files:**
- Create: `internal/migrations/sqlite/0053_signin_approvals.sql`, `internal/migrations/postgres/0053_signin_approvals.sql`
- Modify: `internal/store/auth.go` (`UserRow`, `userCols`, `scanUser`, `InsertUser`, the explicit column list at line 249), `internal/store/sealed.go`, `internal/secrets/aad.go`, `internal/audit/audit.go:62`
- Test: `internal/reseal/reseal_test.go`, `internal/audit/audit_test.go`

**Interfaces:**
- Produces: `store.UserRow.SigninPINEnc []byte` (nil when no PIN); `secrets.AADSigninPIN(userID string) []byte`, returning `"users.signin_pin:" + userID`.

- [ ] **Step 1: Write the failing tests.** In `seedSealed`, set `SigninPINEnc: seal(t, box, "123456", secrets.AADSigninPIN("usr_1"))` on the seeded user and change `const kinds = 8`. In the audit test:

```go
func TestRedactHidesSigninPINButNotLookalikes(t *testing.T) {
	got := Redact(map[string]string{"signin_pin": "123456", "pin": "123456", "shipping": "kept", "mapping": "kept"})
	if strings.Contains(got, "123456") || !strings.Contains(got, `"shipping":"kept"`) || !strings.Contains(got, `"mapping":"kept"`) {
		t.Fatal(got)
	}
}
```

- [ ] **Step 2: Run to verify they fail.** Run: `go test ./internal/reseal ./internal/audit`. Expected: FAIL, undefined `AADSigninPIN` and the redaction test.

- [ ] **Step 3: Implement.** The migration adds the column: `ALTER TABLE users ADD COLUMN signin_pin_enc BLOB;` (BYTEA on Postgres). Add to `sealedSpecs`: `{"users.signin_pin_enc", "users", "signin_pin_enc", []string{"id"}, func(k []string) []byte { return secrets.AADSigninPIN(k[0]) }}`. Add `(^|_)pin($|_)` to the `secretKey` regexp. Plain `pin` would also match `shipping`.

- [ ] **Step 4: Run to verify they pass.** Run: `go test ./internal/reseal ./internal/audit ./internal/store`. Expected: PASS, including `TestSchemaParity`.

- [ ] **Step 5: Commit.** `git add` the listed paths, then `git commit -m "feat(store): add an encrypted sign-in PIN to users"`.

### Task 2: Approval request store

**Files:**
- Modify: both `0053_signin_approvals.sql` files
- Create: `internal/store/signin_approvals.go`, `internal/store/signin_approvals_test.go`

**Interfaces:**
- Consumes: `UserRow.SigninPINEnc`.
- Produces, all in package `store`:

```go
const (SigninPending = "pending"; SigninApproved = "approved"; SigninCollected = "collected"; SigninEnded = "ended")

type SigninApproval struct {
	ID, OrgID, UserID, BrowserHash, IP, Status string
	Remember          bool
	ChatID, MessageID int64 // 0 until the notification is sent
	WrongAttempts     int
	Cleared           bool  // the notification has been deleted
	CreatedAt, ExpiresAt, DecidedAt time.Time
}

func (db *DB) SetUserSigninPIN(ctx context.Context, userID string, enc []byte, now time.Time) error // also ends the user's pending requests
func (db *DB) ClearUserSigninPIN(ctx context.Context, userID string, now time.Time) error            // same, and sets NULL
func InsertSigninApproval(ctx context.Context, tx Tx, a SigninApproval) error                        // first ends the user's earlier pending requests
func (db *DB) SigninApprovalByBrowser(ctx context.Context, browserHash string) (SigninApproval, error) // ErrNotFound
func (db *DB) SigninApprovalByNotification(ctx context.Context, chatID, messageID int64) (SigninApproval, error) // ErrNotFound
func (db *DB) CollectSigninApproval(ctx context.Context, id string, decidedAfter time.Time) (bool, error) // approved -> collected, once
func (db *DB) WrongSigninPINsSince(ctx context.Context, userID string, since time.Time) (int, error)     // sum over requests created since
func (db *DB) ExpireSigninApprovals(ctx context.Context, now time.Time) error                            // pending and past expires_at -> ended
func (db *DB) SigninApprovalsToSend(ctx context.Context, now time.Time) ([]SigninApproval, error)        // pending, unexpired, message_id = 0
func (db *DB) SetSigninNotification(ctx context.Context, id string, chatID, messageID int64) error
func (db *DB) SigninApprovalsToClear(ctx context.Context) ([]SigninApproval, error)                      // not pending, message_id <> 0, not cleared
func (db *DB) MarkSigninCleared(ctx context.Context, id string) error
func (db *DB) RecordSigninReply(ctx context.Context, id string, correct bool, maxWrong int, now time.Time) (SigninApproval, error) // applies only while pending and unexpired
func (db *DB) ClaimSigninPoll(ctx context.Context, botID string, now, until time.Time) (offset int64, ok bool, err error)
func (db *DB) ReleaseSigninPoll(ctx context.Context, botID string, offset int64) error
func (db *DB) HasOpenSigninApprovals(ctx context.Context, now time.Time) (bool, error)                   // something to send, read or clear
func (db *DB) PurgeSigninApprovals(ctx context.Context, before time.Time) error                          // not pending and created before
```

- [ ] **Step 1: Write the failing tests** in `signin_approvals_test.go`, each under `storetest.EachMigrated`:
  - `TestInsertEndsTheUsersEarlierPendingRequest`: after a second insert for one user, the first is `ended` and the second `pending`.
  - `TestCollectWorksOnce`: first `CollectSigninApproval` returns true, second false, and pending or too-old rows return false.
  - `TestRecordSigninReply`: `correct=true` gives `approved` with `DecidedAt` set. 5 calls with `correct=false, maxWrong=5` give `WrongAttempts == 5, Status == "ended"`. A call after expiry or after a decision changes nothing.
  - `TestClaimSigninPollIsExclusivePerBot`: a second claim before `until` returns `ok=false`. A claim after `until` returns `ok=true`. A claim for another `botID` returns `ok=true, offset=0`. `ReleaseSigninPoll(ctx, "1", 42)` makes the next claim for bot `"1"` return 42.
  - `TestSettingOrClearingThePINEndsPendingRequests`: both leave the request `ended`, and `UserByID` shows the new `SigninPINEnc`.

- [ ] **Step 2: Run to verify they fail.** Run: `go test ./internal/store -run 'Signin|Collect|Claim'`. Expected: FAIL to compile.

- [ ] **Step 3: Implement.** Add to both migrations:

```sql
CREATE TABLE signin_approvals (
    id TEXT PRIMARY KEY, org_id TEXT NOT NULL,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    browser_hash TEXT NOT NULL UNIQUE, ip TEXT NOT NULL, status TEXT NOT NULL,
    remember BIGINT NOT NULL DEFAULT 0, chat_id BIGINT NOT NULL DEFAULT 0, message_id BIGINT NOT NULL DEFAULT 0,
    wrong_attempts BIGINT NOT NULL DEFAULT 0, cleared BIGINT NOT NULL DEFAULT 0,
    created_at BIGINT NOT NULL, expires_at BIGINT NOT NULL, decided_at BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX signin_approvals_status ON signin_approvals (status, expires_at);
CREATE INDEX signin_approvals_notification ON signin_approvals (chat_id, message_id);
CREATE INDEX signin_approvals_user ON signin_approvals (user_id, created_at);
CREATE TABLE signin_polls (bot_id TEXT PRIMARY KEY, update_offset BIGINT NOT NULL DEFAULT 0, lease_until BIGINT NOT NULL DEFAULT 0);
```

Use conditional `UPDATE ... WHERE status = ?` for every state change. Reply text is not a parameter of any function here.

- [ ] **Step 4: Run to verify they pass.** Run: `go test ./internal/store`. Expected: PASS on both databases (set `PAYMUX_TEST_POSTGRES_URL` or use `make test-pg PKGS=./internal/store`).

- [ ] **Step 5: Commit.** `git commit -m "feat(store): add sign-in approval requests"` with explicit paths.

### Task 3: Fake Telegram and the setup check

**Files:**
- Create: `internal/outbound/telegramtest/telegramtest.go`, `internal/outbound/signin_approval.go`, `internal/outbound/signin_approval_test.go`

**Interfaces:**
- Produces:

```go
// package telegramtest: an http.RoundTripper standing in for the Bot API.
type Fake struct{ /* mutex-protected state */ }
func New() *Fake
func (f *Fake) RoundTrip(*http.Request) (*http.Response, error)
func (f *Fake) SetChat(chatID int64, chatType string) // default: -100123, "supergroup"
func (f *Fake) SetBotCanDelete(can bool)              // default: true
func (f *Fake) SetWebhook(url string)
func (f *Fake) Fail(method string, status int)        // that method answers status until cleared with status 0
func (f *Fake) Reply(toMessageID int64, text string) int64 // queues an update; "" means a non-text message; returns the reply's message id
func (f *Fake) ReplyFromChat(chatID, toMessageID int64, text string) int64
func (f *Fake) Sent() []SentMessage                   // {ChatID, MessageID, Text, DisableNotification, ForceReply}
func (f *Fake) Deleted() []int64
func (f *Fake) Calls(method string) int
func (f *Fake) LastOffset() int64

// package outbound
func (s *Service) CheckSigninApprovalChannel(ctx context.Context, orgID string) error
```

- [ ] **Step 1: Write the failing test** `TestCheckSigninApprovalChannel`, table-driven under `storetest.EachMigrated`. Expected error text:

| Case | Error (exact) |
|---|---|
| Channel off, or no token or chat | `Turn on the live Telegram channel in Settings first.` |
| Webhook set | `This bot has a webhook set. Remove it so Paymux can read replies.` |
| Chat type `channel` | `Channels can't be used, because they have no normal replies. Use a private chat or a group.` |
| Group, bot lacks delete rights | `Make the bot an admin of this group with permission to delete messages.` |
| `getChat` answers 401 | `Paymux could not reach the Telegram chat. Check the bot token and chat id in Settings.` |
| Private chat, or group with delete rights | no error |

Add one assertion: no returned error contains the token.

- [ ] **Step 2: Run to verify it fails.** Run: `go test ./internal/outbound -run CheckSigninApprovalChannel`. Expected: FAIL to compile.

- [ ] **Step 3: Implement** using `telegramCall`, in this order: `getWebhookInfo`, `getChat`, then for `group`/`supergroup` `getMe` and `getChatMember`. A member passes when `status == "administrator" && can_delete_messages`. Open the token with `secrets.AADNotificationTelegram(orgID, "live")`. Add a private helper for "load the live channel and open its token", which Tasks 4 and 5 reuse.

- [ ] **Step 4: Run to verify it passes.** Same command. Expected: PASS.

- [ ] **Step 5: Commit.** `git commit -m "feat(outbound): check a Telegram chat can approve sign-ins"` with explicit paths.

### Task 4: Starting a request and the tick

**Files:**
- Modify: `internal/outbound/signin_approval.go`, `internal/outbound/signin_approval_test.go`, `internal/outbound/outbound.go` (`Register`)

**Interfaces:**
- Consumes: Task 2's store functions and Task 3's helper.
- Produces:

```go
const (
	JobSigninApprovalTick = "signin_approval.tick"
	SigninApprovalTTL     = 5 * time.Minute
	SigninMaxWrongPINs    = 5
	signinTickEvery       = 2 * time.Second
	signinPurgeAfter      = 48 * time.Hour
)
type SigninApprovalInput struct{ OrgID, UserID, BrowserHash, IP string; Remember bool }
func (s *Service) BeginSigninApproval(ctx context.Context, in SigninApprovalInput) error
func (s *Service) handleSigninApprovalTick(ctx context.Context, raw json.RawMessage) error
```

- [ ] **Step 1: Write the failing tests.**
  - `TestSigninNotificationIsSentWithSoundAndAsksForAReply`: with the clock at 2027-01-15 08:00 UTC, after Begin and `q.RunOnce`, the one sent message has `ChatID == -100123`, `DisableNotification == false`, `ForceReply == true` and the exact text `"Sign-in request\nbuyer@example.com\nIP: 203.0.113.9\nTime: 2027-01-15T08:00:00Z\nReply to this message with your PIN."`. Use a seeded channel whose event list and sound list are empty and whose auto-delete is 1 hour, then assert no `JobDeleteTelegram` job exists.
  - `TestSendingRetriesUntilExpiryThenEnds`: with `Fail("sendMessage", 500)`, the request stays pending across ticks. It sends on the first tick after the fault clears. If never cleared, the request is `ended` after 5 minutes and no further tick is queued.
  - `TestExpiryAndReplacementDeleteTheNotification` (Review Focus 5): both cases put the message ID in `Deleted()`, once.
  - `TestAFailedSendLeavesNoSecretInTheJob`: the payload is `{}`, and `last_error` and captured logs contain no token.
  - `TestTickStopsWhenNothingIsOpen`: with nothing open, a tick enqueues no follow-up and calls no Telegram method.
  - `TestChannelOffFailsClosed`: the channel off leaves the request pending until expiry, logs a warning, and sends nothing.

- [ ] **Step 2: Run to verify they fail.** Run: `go test ./internal/outbound -run 'Signin|Sending|Expiry|Tick|ChannelOff'`. Expected: FAIL.

- [ ] **Step 3: Implement.** `BeginSigninApproval` inserts the row (`ids.New("sia")`, `ExpiresAt = now + SigninApprovalTTL`) and enqueues a tick in one transaction, with dedupe key `signin_tick:req:<id>`. The tick runs these steps in order:
  1. `ExpireSigninApprovals`.
  2. Send for each `SigninApprovalsToSend`, storing the numeric `result.chat.id` and `result.message_id` from the response.
  3. Read replies (Task 5; a stub here).
  4. Delete each `SigninApprovalsToClear` and mark it cleared. A Telegram "message not found" also counts as cleared. On other failures retry on later ticks, and mark cleared anyway once `DecidedAt` or `ExpiresAt` is over 47 hours old, Telegram's deletion limit. Running this after reading removes a decided notification in the same tick.
  5. `PurgeSigninApprovals(now - signinPurgeAfter)`.
  6. If `HasOpenSigninApprovals`, enqueue the next tick with `RunAt = now + signinTickEvery` and dedupe key `signin_tick:<RunAt.UnixMilli()/2000>`.

  Telegram failures log a warning with the status code only and do not fail the job. Only database errors return an error. A job's own key stays taken while it runs, which is why the next tick's key must differ. Do not use one fixed key.

- [ ] **Step 4: Run to verify they pass.** Same command. Expected: PASS.

- [ ] **Step 5: Commit.** `git commit -m "feat(outbound): send sign-in approval notifications"` with explicit paths.

### Task 5: Reading and deciding replies

**Files:**
- Modify: `internal/outbound/signin_approval.go`, `internal/outbound/signin_approval_test.go`

**Interfaces:**
- Consumes: `ClaimSigninPoll`, `ReleaseSigninPoll`, `SigninApprovalByNotification`, `RecordSigninReply`.
- Produces: no new exports. The request's `Status` is what Task 6 reads.

- [ ] **Step 1: Write the failing tests.** Seed a user with PIN `482913` sealed under `secrets.AADSigninPIN`.
  - `TestCorrectReplyApprovesAndDeletesBothMessages`: status `approved`, and `Deleted()` holds the reply and the notification.
  - `TestReplyWithSpacesCounts`: `"  482913\n"` approves (Review Focus 1).
  - `TestNonTextReplyIsDeletedAndNotCounted`: `Reply(msg, "")` is deleted, and `WrongAttempts == 0` (Review Focus 1).
  - `TestWrongRepliesAreDeletedAndFiveEndTheRequest`: after 5, status is `ended`, all 5 are deleted, and the notification is deleted.
  - `TestRepliesElsewhereAreIgnored`: a reply in another chat, a reply to another message and an ordinary message are not deleted, don't count, and don't stop the offset moving.
  - `TestReplyToAnEndedRequestIsStillDeleted`: with another request pending to keep polling on, it is deleted and changes nothing.
  - `TestUsernameChatIDStillMatches`: configured chat `@groupname`, with the fake answering numeric `-100123` (Review Focus 3).
  - `TestOffsetIsKeptPerBotAndAdvancesAfterHandling`: `LastOffset()` is the last update ID plus 1 after a tick, and a request after a token swap reads the new bot from offset 0 (Review Focus 2).
  - `TestOverlappingPollsDoNotDoubleCount`: with the lease held, a tick makes no `getUpdates` call and still queues the next tick.
  - `TestWebhookSetLogsAWarningAndRequestExpires`: with `getUpdates` answering 409, the request ends at expiry.
  - `TestFailedDeleteStillApproves`: with `Fail("deleteMessage", 400)`, the status is `approved` and a warning is logged.
  - `TestReplyTextIsStoredNowhere`: after correct and wrong replies, assert a captured log has no `482913` or wrong-PIN text, then scan every `TEXT` and `BLOB` value of `jobs`, `signin_approvals` and `audit_log` for them.

- [ ] **Step 2: Run to verify they fail.** Run: `go test ./internal/outbound -run 'Reply|Replies|Username|Offset|Overlapping|Webhook|FailedDelete'`. Expected: FAIL.

- [ ] **Step 3: Implement.** Call `getUpdates` only when some request is pending. Use `offset` from the claim, `timeout: 0`, `limit: 100`, `allowed_updates: ["message"]`. `botID` is the token text before `:`. The lease lasts 30 seconds. For each update:
  1. Look up `(message.chat.id, message.reply_to_message.message_id)`. Not found or no `reply_to_message`: skip it.
  2. Call `deleteMessage` on the reply. On failure, log a warning with the IDs only.
  3. If the request is pending and unexpired and the message has text: load the user, open the PIN, compare `strings.TrimSpace(text)` with `subtle.ConstantTimeCompare`, and call `RecordSigninReply(..., SigninMaxWrongPINs, now)`. If the PIN can't be opened, log and do not count it.

  Release the poll with the new offset after the batch, using a `context.WithoutCancel` copy so a shutdown doesn't keep the lease.

- [ ] **Step 4: Run to verify they pass.** Run: `go test ./internal/outbound`. Expected: PASS.

- [ ] **Step 5: Commit.** `git commit -m "feat(outbound): approve sign-ins from PIN replies"` with explicit paths.

### Task 6: The sign-in flow and waiting page

**Files:**
- Create: `internal/dashboard/signin_approval.go`, `internal/dashboard/signin_approval_test.go`, `web/templates/dash_signin_wait.html`
- Modify: `internal/dashboard/login.go:301`, `internal/dashboard/sso.go:138`, `internal/dashboard/dashboard.go` (routes, cookie const, view fields), `internal/dashboard/dashboard_test.go` (`telegramOK` replaced by `telegramtest.Fake` as `e.tg`, default behaviour unchanged)

**Interfaces:**
- Consumes: `BeginSigninApproval`, `SigninApprovalByBrowser`, `CollectSigninApproval`, `WrongSigninPINsSince`.
- Produces:

```go
const cookieSigninWait = "pm_signin_wait"
const signinCollectWindow = 2 * time.Minute
func (d *Dashboard) finishSignIn(w http.ResponseWriter, r *http.Request, u store.UserRow, remember bool)
func (d *Dashboard) signinWaitPage(w http.ResponseWriter, r *http.Request)   // GET /dashboard/login/waiting
func (d *Dashboard) signinWaitStatus(w http.ResponseWriter, r *http.Request) // GET /dashboard/login/waiting/status
```

- [ ] **Step 1: Write the failing tests**, each under `storetest.EachMigrated`:
  - `TestNoSessionBeforeTheCorrectPIN`: password and TOTP with a PIN set give a 303 to `/dashboard/login/waiting`, with no session cookie and no row in `sessions`. After `e.tg.Reply(msg, pin)`, `e.q.RunOnce`, and a GET of the waiting page: 303 to `/dashboard/`, a session cookie, and `remember` honoured.
  - `TestSSONeedsThePINToo`: same assertions through the SSO callback, built on `sso_test.go`'s helpers.
  - `TestNoPINSignsInAsBefore`, plus `TestDevSkipSkipsThePIN`.
  - `TestWaitingPageStatesWording`: pending shows exactly `Waiting for external auth`. Ended (wrong PINs, expiry) shows `This sign-in request ended.` and a link to `/dashboard/login`, and clears the cookie.
  - `TestSignInResponsesNeverMentionTheChannel`: for every response in every state above and in Global Constraints, including headers and the status endpoint, the lowercased body doesn't contain `telegram`.
  - `TestStatusEndpoint`: `pending` while pending, `ready` for anything else including no cookie, `text/plain`, `no-store`, and no state change: it never creates a session.
  - `TestAnotherBrowserCannotCollect`: a different client gets a redirect to login and no session. `TestCollectingWorksOnce`: replaying the old cookie after success gets no second session.
  - `TestDisabledUserOrRemovedPINCannotCollect` (Review Focus 4). Disable the user after approval: no session. Removing the PIN after approval: no session.
  - `TestSecondSignInReplacesTheFirst` (Review Focus 5): the first browser ends up on the ended page.
  - `TestApprovalTooLateToCollect`: approved, then clock +3 minutes: ended page, no session.
  - `TestWrongPINsLockTheAccount`: 2 cancelled requests (10 wrong PINs) inside 15 minutes, then a third full sign-in renders `msgLockedOut` with 429 and queues no request. At +16 minutes it works again.
  - `TestTOTPTicketIsSpentOnceWaiting`: re-POSTing `/login/totp` with the old ticket does not start another request.

- [ ] **Step 2: Run to verify they fail.** Run: `go test ./internal/dashboard -run 'PIN|SSONeeds|Waiting|SignInResponses|StatusEndpoint|Collect|SecondSignIn|TOTPTicket'`. Expected: FAIL.

- [ ] **Step 3: Implement `finishSignIn`** and call it in place of `createSession` plus its redirect at `login.go:301` and `sso.go:138`. Leave the `DevSkip2FA` branch at `login.go:113` and `setup.go:242` as they are. Logic:
  1. `d.o.DevSkip2FA` or no `u.SigninPINEnc`: `createSession`, redirect to `prefix+"/"`.
  2. `WrongSigninPINsSince(u.ID, now-loginWindow) >= accountTOTPFailures`: clear the ticket cookie and render `dash_login` with `msgLockedOut` and 429.
  3. Otherwise make a random 32-byte token, store its SHA-256 hex as `BrowserHash`, call `BeginSigninApproval` with `d.orgID(ctx)` and `d.clientIP(r)`, set `cookieSigninWait` (360 seconds), clear the ticket cookie, and redirect to `/dashboard/login/waiting`. Any error goes to `d.fail`, so no session.

- [ ] **Step 4: Implement the page handlers.** Wait page: no cookie or unknown goes to `/dashboard/login`. `pending` and unexpired renders the page. `approved` with `DecidedAt` inside `signinCollectWindow`: reload the user, refuse if disabled or the PIN is gone, then `CollectSigninApproval`, `createSession` with the stored `Remember`, clear the cookie, 303 to `prefix+"/"`. Anything else clears the cookie and renders the ended state. The template, in the `dash_open`/`dash_close` frame like `dash_totp.html`: `<section data-signin-wait="/dashboard/login/waiting/status"><h3>Waiting for external auth</h3></section>`, with `<noscript><meta http-equiv="refresh" content="3"></noscript>` in the `head` block for browsers without JavaScript.

- [ ] **Step 5: Run to verify they pass.** Run: `go test ./internal/dashboard`. Expected: PASS, with existing sign-in tests unchanged.

- [ ] **Step 6: Commit.** `git commit -m "feat(dashboard): wait for approval before signing in"` with explicit paths.

### Task 7: The waiting page signs in by itself

The dashboard adapter loads form responses with `fetch`, so a `<meta>` refresh in the fetched page's head never applies. The React side must do the polling.

**Files:**
- Create: `frontend/dashboard/src/signin-wait.ts`, `frontend/dashboard/src/signin-wait.test.ts`
- Modify: `frontend/dashboard/src/adapter.ts` (the `DashboardPage` type and `parseDashboard`), `frontend/dashboard/src/shell.tsx` (`AuthLayout`), `frontend/dashboard/src/adapter.test.ts`

**Interfaces:**
- Consumes: the `data-signin-wait` attribute and the status endpoint from Task 6.
- Produces: `DashboardPage.signinWait: string | null` and:

```ts
export function startSigninWait(statusUrl: string, done: () => void, fetcher: typeof fetch = fetch, everyMs = 2000): () => void
```

- [ ] **Step 1: Write the failing tests** with Vitest fake timers:
  - `done` is not called while the response body is `pending`.
  - `done` is called once when it becomes `ready`, and polling stops.
  - A network error or a 500 keeps polling.
  - The returned stop function ends polling.
  - In `adapter.test.ts`, parsing a page holding `data-signin-wait="/x"` gives `auth === true` and `signinWait === "/x"`, and a normal login page gives `signinWait === null`.

- [ ] **Step 2: Run to verify they fail.** Run: `cd frontend && bun run test:frontend signin-wait adapter`. Expected: FAIL.

- [ ] **Step 3: Implement.** In `AuthLayout`, a `useEffect` keyed on `page.signinWait` calls `startSigninWait(page.signinWait, () => location.assign("/dashboard/login/waiting"))` and returns its stop function. A full navigation is needed so the server's redirect loads the signed-in app from scratch.

- [ ] **Step 4: Run to verify they pass, then check types.** Run: `cd frontend && bun run test:frontend && bun run typecheck`. Expected: PASS.

- [ ] **Step 5: Commit.** `git commit -m "feat(dashboard): sign in as soon as approval arrives"` with explicit paths.

### Task 8: PIN controls on the account page

**Files:**
- Modify: `internal/dashboard/account.go`, `internal/dashboard/dashboard.go` (`view`), `web/templates/dash_account.html`, `internal/dashboard/signin_approval.go`, `internal/dashboard/signin_approval_test.go`

**Interfaces:**
- Consumes: `CheckSigninApprovalChannel`, `SetUserSigninPIN`, `ClearUserSigninPIN`, `secrets.AADSigninPIN`.
- Produces: `POST /dashboard/account/signin-pin` and `POST /dashboard/account/signin-pin/remove`, both `d.authed(selfOnly, d.recent(...))`, and `view.HasSigninPIN bool`.

- [ ] **Step 1: Write the failing tests.**
  - `TestSetPINStoresItEncrypted`: after a POST with `pin=482913&confirm_pin=482913`, the raw column doesn't contain `482913`, and it opens with `AADSigninPIN(id)`. The audit row `user.signin_pin_set` holds no PIN.
  - `TestPINFormatIsChecked`: `12345`, `1234567890123`, `12a456` and differing confirmation are refused with a 400, nothing is stored and the channel check is not called.
  - `TestSetPINRefusedWhenTheChatCannotDoIt`: for a channel-type chat, the page shows the Task 3 message and no PIN is stored.
  - `TestRemovePINFromTheAccountPage`: the column is NULL, pending requests end, and the audit row is `user.signin_pin_removed`.
  - `TestPINChangesNeedARecentSignIn`: past `recentAuthWindow`, a POST redirects to `/dashboard/reauth` and stores nothing.
  - `TestAccountPageNeverEchoesThePIN`: the page after set and after a refused set has no PIN value.

- [ ] **Step 2: Run to verify they fail.** Run: `go test ./internal/dashboard -run 'SetPIN|PINFormat|RemovePIN|PINChanges|AccountPageNever'`. Expected: FAIL.

- [ ] **Step 3: Implement.** Order in the handler: format, confirmation, `CheckSigninApprovalChannel`, seal, store, audit, redirect to `/dashboard/account?n=pin` or `?n=pin_removed`. Notices: `Your sign-in PIN was saved.` and `Your sign-in PIN was removed.`

  Add a template section titled `Sign-in approval`, placed after `Change password` and following that form's markup. Text: `Each sign-in must also be approved by replying to a Telegram message with this PIN. Anyone in that chat can see a PIN until Paymux removes it.` Both inputs use `type="password" inputmode="numeric" autocomplete="new-password" pattern="[0-9]{6,12}" minlength="6" maxlength="12"`. Show `Remove PIN` only when `HasSigninPIN`. The account page's forms have no review dialog today, so add none.

- [ ] **Step 4: Run to verify they pass.** Run: `go test ./internal/dashboard`. Expected: PASS.

- [ ] **Step 5: Commit.** `git commit -m "feat(dashboard): let users set their sign-in PIN"` with explicit paths.

### Task 9: CLI, docs and the full gate

**Files:**
- Modify: `internal/cli/user.go`, `internal/cli/user_test.go`, `internal/dashboard/users.go`, `docs/runbook.md` (the "Lost authenticator" line at 117), `docs/security-review.md`

**Interfaces:**
- Consumes: `ClearUserSigninPIN`.
- Produces: `func (u Users) RemoveSigninPIN(ctx context.Context, email string) error`, and `paymux user remove-pin --email <email>`.

- [ ] **Step 1: Write the failing tests** in `user_test.go`:
  - `TestUserRemovePIN`: with a PIN and a pending request seeded, exit code 0 and stdout `sign-in PIN removed for a@example.com; their pending approval requests were cancelled.` The column is NULL, the request ended, and the TOTP secret and sessions are unchanged.
  - An unknown email gives exit 1 with `no user with email`.
  - `TestUserUsage` includes `remove-pin`.

- [ ] **Step 2: Run to verify they fail.** Run: `go test ./internal/cli -run 'RemovePIN|UserUsage'`. Expected: FAIL.

- [ ] **Step 3: Implement.** Add the `remove-pin` command to `runUser`'s check, `userUsage` and the flow.

- [ ] **Step 4: Run to verify they pass.** Same command. Expected: PASS.

- [ ] **Step 5: Update the docs.** Runbook, next to "Lost authenticator": `Locked out by the sign-in PIN, or the Telegram bot is down: paymux user remove-pin --email you@example.com. With SQLite, stop the hub first.` Add the known risks from the spec to `docs/security-review.md`, and note that the PIN is encrypted rather than hashed.

- [ ] **Step 6: Run the full gate.** Run: `bash scripts/ci.sh`. Expected: exit 0.

- [ ] **Step 7: Commit.** `git commit -m "feat(cli): remove a user's sign-in PIN"` with explicit paths.

- [ ] **Step 8: Ask the owner to try it with the real bot.** The fake can't prove Telegram's real behaviour. On a test deployment: turn on a PIN, sign in, reply with a wrong PIN and then the right one, and confirm the replies vanish and the page signs in by itself. Report the result. Do not claim this step was done without it.
