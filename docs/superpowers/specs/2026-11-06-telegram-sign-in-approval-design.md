# Telegram sign-in approval

Status: approved by the owner on 2026-11-07. Date: 2026-11-06.

## Why

A stolen password and authenticator code should not be enough to open the
production dashboard. Users who opt in must also approve each sign-in from
Telegram by replying with a personal PIN. The sign-in page must not reveal what
the extra step is or where it happens.

## Decided by the owner

- A user turns this on for themselves in the dashboard and sets their own PIN.
  Users who do not turn it on sign in as they do today.
- Each user has their own PIN, stored encrypted in the database.
- The PIN can be removed from the CLI.
- Password sign-in on production order: password, authenticator code, Telegram
  notification, PIN reply, signed in.
- The notification goes to the Telegram chat already configured for
  notifications. There is no per-user chat.
- The user approves by replying to the notification with their PIN.
- Paymux deletes the PIN reply as soon as it receives it.
- The sign-in page shows only "Waiting for external auth" and never mentions
  Telegram. It signs the user in once the PIN is received.

## Goals

- A sign-in by an opted-in user cannot create a session without the correct PIN.
- A waiting page signs in on its own, with no reload or further input.
- Being locked out is recoverable from the CLI.

## Non-goals

- Per-user Telegram chats or linking Telegram accounts.
- Admins setting, viewing or requiring other users' PINs from the dashboard.
- Asking for the PIN at the "recent sign-in" check on sensitive pages.
- Approving with a button instead of a PIN.

## Anti-goals

Treat any of these as a failure, even if sign-in works:

- The PIN appears in logs, `jobs.last_error`, job payloads, audit data or error
  messages. Reply text must never be stored.
- Any sign-in response (page, error, status text, page source) mentions Telegram.
- A Telegram failure lets a sign-in through. It must fail closed.
- Turning off the Telegram channel, or SSO, becomes a way around the PIN.

## Approach

### Current code to build on

- Password then TOTP flow: `loginSubmit` and `totpSubmit` in
  `internal/dashboard/login.go`. Both end in `createSession`.
- SSO replaces both steps and calls `createSession` directly:
  `internal/dashboard/sso.go:138`.
- Signed ticket cookie for the pending sign-in: `issueTicket` and `ticketUser` in
  `internal/dashboard/dashboard.go`. `totpSubmit` kills the ticket on success.
- Telegram channel: `store.NotificationConfig`, per org and mode.
  `telegramCall` in `internal/outbound/notifications.go` keeps the token out of
  errors. The bot only sends today and nothing reads updates.
- Encrypted columns are listed in `internal/store/sealed.go`, with AAD helpers in
  `internal/secrets/aad.go`. The new column must be added to the list, or key
  rotation will miss it.
- CLI: `runUser` in `internal/cli/user.go`, next to `reset-2fa`.
- Account page and routes: `internal/dashboard/account.go`. `d.recent` guards
  sensitive changes.
- Migrations exist for SQLite and Postgres with the same number. The next free
  number is 0053 at the time of writing.

### Sign-in flow

1. Password and TOTP run unchanged. With SSO, the identity provider step runs.
2. If the user has no PIN, create the session as today.
3. If the user has a PIN, create an approval request instead of a session, and
   redirect to a waiting page.
4. Send the notification to the configured chat. It states the email, IP address
   and time, and asks for a reply with the PIN.
5. The waiting page polls until the request is approved, expired or cancelled.
6. On a correct reply, delete the reply, delete the notification and mark the
   request approved. The next poll from the same browser creates the session,
   keeping the "remember" choice made at the first step.
7. On expiry or cancellation, delete the notification. The page says the request
   ended and sends the user back to sign in.

### Approval request

Keep it in the database, so it survives a restart and works across job workers.
It holds the user, the browser binding, the Telegram message ID, status, wrong
attempt count and expiry. Only the browser that started the sign-in can collect
the session, and it can do so once.

### Reading replies

Call `getUpdates` only while at least one request is pending. This needs no public
URL. A reply counts only when it comes from the configured chat and is a reply to
that request's notification, which identifies the request when several are waiting.

Delete every reply to a pending notification, right or wrong, because a wrong
reply may be a mistyped PIN. Ignore everything else in the chat. Advance the
update offset only after handling, so a restart does not lose a reply.

Replies to an old notification, after its request has ended, are still deleted
when seen.

### Setting up the chat

Telegram sets limits the design depends on:

- In groups, a bot can delete other people's messages only if it is an admin with
  delete rights. In a private chat it can delete incoming messages.
- A channel has no normal replies, so it cannot be used.
- In groups with privacy mode on, bots still receive replies to their own messages.
- `getUpdates` fails while a webhook is set on the bot.

So turning on the PIN checks that the live channel is enabled and working, and that
the chat is private, or a group where the bot may delete messages. If not, the
account page says what to fix and the PIN is not turned on.

### Account page and CLI

- Account page: turn on, change and turn off the PIN, behind `d.recent`, using
  the review-dialog rules of the page it sits on.
- CLI: `paymux user remove-pin --email <email>`. It clears the PIN and cancels the
  user's pending requests. It must not touch TOTP or sessions.

### Failure behaviour

| Case | Behaviour |
|---|---|
| Notification cannot be sent | Retry until expiry, then the request ends. The page stays generic. |
| Channel turned off or token broken while a PIN is set | No sign-in; recover with the CLI or fix the channel |
| Bot cannot delete the reply | Approval still succeeds. Log a warning without the reply text. |
| Webhook set on the bot | Requests expire. Log a warning. |
| Server restarts during a request | The request and offset carry on |

## My calls, for the owner to check

Proposed in conversation or settled while writing this. The owner did not object,
but did not explicitly decide them either.

1. **Off by default:** no PIN means sign-in as today.
2. **SSO needs the PIN too**, after the identity provider step.
3. **Live channel:** the live-mode Telegram channel is always the one used.
4. **Limits:** 5 minutes to reply. 5 wrong PINs cancel the request, and they also
   count toward the existing account sign-in lockout.
5. **PIN format:** 6 to 12 digits.
6. **Turning it off:** users can also turn it off from the account page, not only
   from the CLI.
7. **"On production":** the step runs whenever a user has a PIN, except where
   `PAYMUX_DEV_SKIP_2FA` already skips 2FA in development.
8. **Bad delete:** if the reply cannot be deleted, sign-in still succeeds, because
   the setup check should already prevent it.
9. **Encrypted, as decided:** the PIN is encrypted rather than hashed, so anyone
   holding the app's encryption key could read it.
10. **Always delivered, with sound:** the notification ignores the channel's event
    list and sound settings, and its auto-delete setting.

## Known risks

The owner chose the existing chat after hearing the first risk below. The others
follow from the same choice and have not been separately discussed.

- Everyone in a group chat can see a PIN until it is deleted, and it may stay in
  their phone notifications. A private chat avoids this.
- Everyone in the chat also sees sign-in notifications, including email and IP.
- Anyone in the chat who knows a user's PIN can approve that user's sign-in.
- If the bot stops working, users with a PIN are locked out until the bot is fixed
  or the PIN is removed from the CLI.

## Left to the builder

- Table and column names, polling interval and page wording beyond the required
  phrase.
- How the waiting page polls, and where the `getUpdates` loop runs, within the
  existing process and job model.
- How the ticket cookie carries over to the waiting step.
- Which audit events to record, without the PIN. Check that the audit redaction
  in `internal/audit/audit.go` also catches any new PIN field name.

## Testing

Use the failing-test-first approach, on both SQLite and Postgres where the store
is involved. Must be covered:

- No session before a correct PIN, on both the password and SSO paths.
- Wrong PIN, expiry, 5-wrong cancellation, and a reply from another chat or to
  another message.
- The reply is deleted in every case, and its text appears nowhere in stored data.
- Sign-in responses never contain "telegram" in any state.
- Another browser cannot collect an approved session, and collecting works once.
- `remove-pin` restores normal sign-in and cancels pending requests.
- The new column is covered by key rotation.
