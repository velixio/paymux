# Payment Tries and Timeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep every provider try with its error and 3D Secure result, expose tries on the merchant API, and replace the payment page's tabs with one timeline page.

**Architecture:** `provider_payment_attempts` becomes the permanent try history, keyed per try and written best-effort from `core.ApplyAttemptSnapshot`, so every path records. The API and dashboard read that table. The page stays a Go template rendered through the existing frontend adapter, built from a new timeline view model.

**Tech Stack:** Go, SQLite and Postgres migrations, Go `html/template`, React dashboard adapter, Playwright.

**Spec:** `docs/superpowers/specs/2026-11-06-payment-tries-and-timeline-design.md`. Mockups: `docs/design/paymux/payment-timeline.png`, `payment-timeline-sparse.png`.

## Global Constraints

- Build on a new branch and worktree from `main`, never in the current checkout. It holds someone's uncommitted Telegram work. Copy the spec, plan and two mockup PNGs into the worktree and commit them first.
- Never add AI attribution or `Co-authored-by` trailers (`AGENTS.md`). Use the configured git identity.
- API changes are additive. Existing fields keep their meaning; `failure_code` and `failure_message` on an attempt still clear on capture.
- Missing values are left out, never empty strings. A ✕ appears only when a provider reported a failure.
- Store outcomes only: no authentication data, full card numbers, expiry or CVV.
- A history write failure must never block applying a snapshot.
- `three_d_secure.status` values are exactly `passed`, `failed`, `not_verified`.
- Migration number: the next free number at build time, in both `internal/migrations/sqlite` and `postgres`. `0049` is in use by the Telegram work, so check again before merging.
- Provider fields marked "verify" come from memory. Confirm against a real response or sandbox before coding the mapping; if a field doesn't exist, leave it out and say so in the task's report.
- All Go tests touching the database use `storetest.EachMigrated`, so they run on both databases where available.

## Review Focus

1. A provider sends the same failed try repeatedly (webhook retry, then sync): expect one row, not two. Task 4.
2. Success arrives before the failure webhook: expect the payment to stay succeeded and the failed try still recorded. Task 2.
3. An operator double-clicks Retry or Redeliver: expect one send per endpoint, and retries still work if it fails again. Task 6.
4. A provider's error text or cardholder name contains HTML: expect it shown as plain text. Task 8.
5. Stripe's open-session failures now add rows that analytics counts: expect them counted as `failed`, not an unknown status. Task 2.

---

### Task 1: Try storage

**Files:**
- Create: `internal/migrations/sqlite/NNNN_provider_tries.sql`, `internal/migrations/postgres/NNNN_provider_tries.sql`
- Modify: `pkg/connector/types.go`, `internal/store/attempts.go`, `internal/core/state/state.go`, `internal/reconcile/reconcile.go`
- Test: `internal/store/attempts_try_test.go`

**Interfaces:**
- Produces:
  - `connector.ThreeDSPassed = "passed"`, `ThreeDSFailed = "failed"`, `ThreeDSNotVerified = "not_verified"`
  - `PaymentSnapshot.TryID string` (`json:"try_id,omitempty"`; empty means `ProviderPaymentID` identifies the try)
  - `PaymentSnapshot.ThreeDS string` (`json:"three_d_secure_status,omitempty"`)
  - `CardDetails.Name string` (`json:"name,omitempty"`)
  - `store.ProviderPaymentAttempt` gains `TryID, MethodDetails, RiskDetails, ThreeDS string`
  - `state.ProviderAttemptStatus(raw string) AttemptStatus`, moved from `reconcile.providerAttemptStatus`, which now calls it

- [ ] **Step 1: Write failing tests**

`TestUpsertTryKeepsTriesOfOnePaymentApart`: two upserts with the same `ProviderPaymentID` and different `TryID` give two rows from `ListProviderPaymentAttempts`, oldest first. `TestUpsertTryKeepsFailureOnLaterUpdate`: upserting a captured status with no failure leaves `FailureCode` and `FailureMessage` set. `TestUpsertTryRoundTripsDetails`: `MethodDetails`, `RiskDetails`, `ThreeDS` come back unchanged. `TestFindAttemptByPaymentIDWithSeveralTries`: `FindAttempt` by the shared payment ID returns the attempt. `TestMigrationKeepsExistingTries`: a row inserted before the migration survives with `try_id` equal to its `provider_payment_id`, if `storetest` can stop at a version; otherwise check by hand and note it.

- [ ] **Step 2: Run and confirm they fail**

Run: `go test ./internal/store/ -run 'Try|FindAttemptByPaymentIDWithSeveralTries'`
Expected: FAIL, unknown fields.

- [ ] **Step 3: Add the migration**

Rebuild the table: create a new table with the existing columns plus `try_id TEXT NOT NULL`, `method_details TEXT`, `risk_details TEXT`, `three_d_secure TEXT`, primary key `(provider_account_id, try_id)`. Copy rows with `try_id = provider_payment_id`, drop the old table, rename, recreate `provider_payment_attempts_attempt`, and add a non-unique index on `(provider_account_id, provider_payment_id)`.

- [ ] **Step 4: Update the store and types**

`UpsertProviderPaymentAttempt` sets `try_id` to `TryID`, or `ProviderPaymentID` when empty, and conflicts on the new key. New detail columns use `COALESCE(excluded.x, existing)`. Move the status function. The lookups in `connectors.go:210` and `recon.go:238` need no change; the test proves it.

- [ ] **Step 5: Run and commit**

Run: `go test ./internal/store/ ./internal/reconcile/ ./pkg/connector/...`
Expected: PASS.

```bash
git add -A internal/migrations internal/store pkg/connector internal/core/state internal/reconcile
git commit -m "feat: store each provider try with its details"
```

### Task 2: Record every try

**Files:**
- Modify: `internal/core/attempts.go`, `internal/checkout/checkout.go`, `internal/checkout/jobs.go`, `internal/subscriptions/*` (`toSnapshot`)
- Test: `internal/core/tries_test.go`, `internal/checkout/tries_test.go`

**Interfaces:**
- Consumes: Task 1 fields.
- Produces: `core.Snapshot` gains `TryID, RawStatus, ThreeDS string; ProviderCreatedAt time.Time`. `checkout.ToSnapshot` fills them. `func (s *Service) recordTry(ctx, attemptID string, snap Snapshot)` in core.

- [ ] **Step 1: Write failing tests**

Core: `TestApplyRecordsTryOnEveryPath` applies a failed snapshot through `ApplyAttemptSnapshot` and expects one try row with failure, method details, risk and 3D Secure. `TestFailedTrySurvivesLaterCapture`: fail with ID `a`, capture with ID `b`: both rows exist and the attempt's own failure is cleared. `TestLateFailureAfterCapture`: capture, then apply a failed snapshot with another ID: payment stays succeeded, failed try recorded. `TestOpenFailureStoredAsFailed`: a `requires_action` snapshot with `FailureCode` stores status `failed`. `TestTryWriteFailureDoesNotBlockApply`: with history writes failing (rename the table in the test), the attempt still applies.

Checkout: `TestWebhookRecordsNotifiedFailedTry`: a `NotifyPayment` whose snapshot has its own `TryID` and an error records that try even when `FetchPayment` returns a later one. `TestWebhookFetchesTryHistoryOnFailure`: with `fakeconn`, a failure notification records all `FetchPaymentAttempts` results.

- [ ] **Step 2: Run and confirm they fail**

Run: `go test ./internal/core/ ./internal/checkout/ -run 'Try|Late|OpenFailure|TryHistory'`
Expected: FAIL.

- [ ] **Step 3: Implement**

`recordTry` runs at the start of `ApplyAttemptSnapshot`, after validation, in its own `db.Write`. It skips empty `ProviderPaymentID`, and on error logs with `slog.Warn` and returns. If the snapshot has a failure and status isn't captured, it stores `failed`; otherwise it stores `RawStatus`, falling back to the snapshot status. Remove the now-redundant `recordProviderPaymentAttempt` calls before apply in `syncAttempt` and `SyncAttemptLatest`, and keep `recordProviderPaymentAttempts` for history, including the new detail fields.

In `handleNotification`, before `syncAttempt`: if `snapshotIsOurs(a, *ps)` and it has `TryID` or an error, record it. If `ps` has an error or a failed status, call `recordProviderPaymentAttempts` with a ref built from `a`, using `live` from `s.o.Conns.Get`.

- [ ] **Step 4: Run and commit**

Run: `go test ./internal/core/... ./internal/checkout/... ./internal/subscriptions/... ./internal/reconcile/... ./internal/store/...`
Expected: PASS. Check analytics tests still pass.

```bash
git add internal/core internal/checkout internal/subscriptions
git commit -m "feat: record every provider try from all snapshot paths"
```

### Task 3: Tries on the API and the failure webhook

**Files:**
- Modify: `internal/core/views.go`, `internal/core/payments.go`, `internal/core/attempts.go`, `docs/openapi.yaml`, `sdk/go/types.go`, `sdk/js/src/types.ts`, `docs/payments.md`, `docs/webhooks.md`, and any generated copy such as `llms-full.txt`
- Test: `internal/core/tries_view_test.go`, `internal/httpapi/payments_tries_test.go`

**Interfaces:**
- Consumes: `state.ProviderAttemptStatus`, Task 1 store fields.
- Produces: `core.TryView{ProviderPaymentID, Status, Method string; MethodDetails *connector.PaymentMethodDetails; ThreeDSecure *ThreeDSecureView; FailureCode, FailureMessage string; CreatedAt time.Time}` with the JSON names in the spec. `core.ThreeDSecureView{Status string}`. `AttemptView.Tries []TryView` (`json:"tries"`, never null).

- [ ] **Step 1: Write failing tests**

`TestPaymentAttemptsIncludeTries`: oldest first, grouped under the right attempt, normalized status. `TestTryJSONLeavesOutMissingFields`: marshal a bare try and assert no `three_d_secure`, `method_details`, `failure_code` keys. `TestAttemptWithoutTriesHasEmptyList`: JSON has `"tries":[]`. `TestCheckoutErrorKeepsAttemptFailure`. `TestPaymentFailedEventCarriesFailure`: `payment.failed` data has both fields from the failing attempt. `TestOtherEventsLeaveFailureOut`: `payment.succeeded` has neither. `TestTriesScopedToAppAndMode` over HTTP: another app's key and the other mode get 404.

- [ ] **Step 2: Run and confirm they fail**

Run: `go test ./internal/core/ ./internal/httpapi/ -run 'Tries|TryJSON|PaymentFailedEvent|OtherEvents|CheckoutErrorKeeps'`
Expected: FAIL.

- [ ] **Step 3: Implement**

Add failure fields with `omitempty` to `paymentEventData`, passing them at `attempts.go:429` only when the payment moved to `failed` and the attempt has a failure. Update SDK types and OpenAPI, including the name in `CardDetails` and a sentence that fields vary by provider.

- [ ] **Step 4: Run and commit**

Run: `go test ./internal/core/... ./internal/httpapi/... ./sdk/go/...` and the repo's JS SDK and OpenAPI checks (see `.github/workflows/ci.yml`).
Expected: PASS.

```bash
git add internal/core internal/httpapi docs sdk llms-full.txt llms.txt
git commit -m "feat: show every try on the payment API and failure webhook"
```

### Task 4: Stripe tries

**Files:**
- Modify: `connectors/stripe/payments.go`, `connectors/stripe/webhook.go`, `connectors/stripe/webhook_config.go`
- Create: `connectors/stripe/testdata/event_payment_intent_failed.json`, `connectors/stripe/testdata/charges_of_intent.json`
- Test: `connectors/stripe/tries_test.go`

**Interfaces:**
- Consumes: Task 1 snapshot fields.
- Produces: `FetchPaymentAttempts(ctx, connector.PaymentRef) ([]connector.PaymentSnapshot, error)` on `*stripe`.

Verify in the sandbox tests (`connectors/stripe/sandbox_*`) before finishing: the shape of `last_payment_error.payment_method` and `.charge`, whether an authentication failure creates a charge, and `card.three_d_secure.result` values. If no sandbox key is available, build from Stripe's API reference and report that it's unverified live.

- [ ] **Step 1: Write failing tests**

`TestParsePaymentIntentFailed`: gives `NotifyPayment`, ref from metadata, `ErrorCode` `insufficient_funds`, card last4 and a `TryID`. `TestPaymentIntentFailedWithoutRefIsIgnored`: no `paymux_ref` metadata (invoice payment) returns nil. `TestRepeatedFailureSameTryID`: the same event parsed twice, and the charge listing for it, all give one `TryID`. `TestAuthFailureWithoutChargeTryID`: gives `<pi>:<payment_method>:<code>` and `ThreeDSFailed`. `TestFetchPaymentAttemptsListsCharges`: session lookup, then `GET /v1/charges?payment_intent=`, each with `TryID` = charge ID. `TestChargeMapsChecks`: `address_line1_check`, `address_postal_code_check`, `cvc_check` map to `CheckMatch`/`CheckNoMatch`; `three_d_secure.result` `authenticated` gives `passed`, `failed` gives `failed`, absent gives empty; `outcome.risk_score` gives `Risk.Score`; `unchecked` is left out. `TestNoStripeNameOnCard`: billing name doesn't set `CardDetails.Name`. `TestWebhookEventsIncludePaymentFailed`.

- [ ] **Step 2: Run and confirm they fail**

Run: `go test ./connectors/stripe/ -run 'PaymentIntentFailed|TryID|FetchPaymentAttempts|ChargeMaps|NoStripeName|WebhookEventsInclude'`
Expected: FAIL.

- [ ] **Step 3: Implement**

`TryID` is `last_payment_error.charge`, else latest charge ID, else `<pi>:<payment_method>:<code>`. On `payment_intent.payment_failed`, return the snapshot with status from `mapIntent`, so it doesn't mark the attempt failed while the session is open. Add the event to `stripeWebhookEvents`, and confirm the existing webhook configuration path updates existing endpoints; if it needs a re-save, document it in `docs/` and the final report.

- [ ] **Step 4: Run and commit**

Run: `go test ./connectors/stripe/ ./internal/checkout/...`
Expected: PASS, including conformance and fuzz seed tests.

```bash
git add connectors/stripe
git commit -m "feat: record each Stripe try with its checks and 3D Secure result"
```

### Task 5: Other connectors

**Files:**
- Modify: `connectors/razorpay/webhook.go`, `connectors/whop/risk.go`, `connectors/whop/whop.go`, `connectors/payu/api.go`; `connectors/cashfree/payments.go` and `connectors/zoho/*` only if real responses justify it
- Test: `connectors/razorpay/tries_test.go`, `connectors/whop/risk_test.go`, `connectors/payu/tries_test.go`

**Interfaces:**
- Consumes: Task 1 snapshot fields.

- [ ] **Step 1: Check real responses**

Read each connector's `testdata` and any provider docs. For Razorpay, confirm `error_source`, `error_step`, `error_reason` (verify). List what each provider offers for 3D Secure, risk checks, bank and name, and put that table in the commit message. Don't add a mapping without evidence.

- [ ] **Step 2: Write failing tests**

Whop: `TestThreeDSTrueIsPassed`, `TestThreeDSFalseIsNotVerified` (never `failed`), `TestThreeDSAbsentIsEmpty`. PayU: `TestNameOnCardCaptured` from `verify_success_card.json`, `TestPlaceholderNameIgnored` for an empty or placeholder value, using whatever placeholder the fixtures show. Razorpay, if confirmed: `TestAuthenticationStepFailureIsThreeDSFailed`, `TestOtherStepHasNoThreeDS`.

- [ ] **Step 3: Run and confirm they fail, implement, run again**

Run: `go test ./connectors/...`
Expected: first FAIL on the new tests, then all PASS.

- [ ] **Step 4: Test the redaction, then commit**

Add `TestCardNameRedacted` in `internal/redact` with a snapshot-style JSON line holding `"name":"S CUSTOMER"` under `card`. If it fails, extend the rules.

```bash
git add connectors internal/redact
git commit -m "feat: map 3D Secure and name on card from other providers"
```

### Task 6: Retry a delivery that is waiting

**Files:**
- Modify: `internal/outbound/outbound.go`, `internal/dashboard/payments.go` (`resendNotice`), `docs/webhooks.md`
- Test: `internal/outbound/outbound_test.go`

**Interfaces:**
- Produces: `outbound.ErrDeliveryInFlight` (sentinel). `outbound.ResendTarget{EndpointID, URL, DeliveryID, Status string; LastStatus int; LastError, NextAttemptAt string; New bool}`. `func (s *Service) ResendTargets(ctx context.Context, eventID string) ([]ResendTarget, error)` returning exactly the endpoints `ResendEvent` would consider.

- [ ] **Step 1: Write failing tests**

Replace `TestResendEventSkipsPendingDelivery`, whose behavior the spec changes, with `TestResendEventSendsWaitingDeliveryNow`: `Sent` 1, `Skipped` 0, a second try reaches the sink immediately, and no leftover job fires when time advances past the old retry. `TestReplayWaitingDeliveryNow`: the same for `ReplayDelivery`. `TestDoubleRetryOneSend`: two replays in a row reach the sink once. `TestRetryAfterFailureStillSchedules`: a replay that fails again schedules the next retry. `TestReplayInFlightRefused`: a claimed job returns `ErrDeliveryInFlight`, and `ResendEvent` counts it in `Skipped`. `TestResendTargetsMatchResend`: disabled, other-mode and unsubscribed endpoints are excluded, a new endpoint has `New: true`, and the count equals `ResendEvent`'s `Sent`.

- [ ] **Step 2: Run and confirm they fail**

Run: `go test ./internal/outbound/ -run 'Waiting|DoubleRetry|RetryAfterFailure|InFlight|ResendTargets'`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `replayTx`, for a `pending` delivery, call `store.CancelUnclaimedJob` for dedupe keys `JobDeliver:<id>:<AttemptCount>` and `JobDeliver:<id>:0`. If neither cancels, return `ErrDeliveryInFlight`. Otherwise enqueue with `AttemptCount` as the attempt, to run now. `ResendEvent` treats `ErrDeliveryInFlight` as `Skipped`.

For `TestDoubleRetryOneSend`: the second replay cancels the first replay's unclaimed job and enqueues one job, so only one send occurs. If the first has been claimed, it's refused as in flight.

- [ ] **Step 4: Run and commit**

Run: `go test -race ./internal/outbound/ ./internal/jobs/ ./internal/dashboard/`
Expected: PASS.

```bash
git add internal/outbound internal/dashboard/payments.go docs/webhooks.md
git commit -m "fix: let operators resend a webhook that is waiting to retry"
```

### Task 7: Timeline view model

**Files:**
- Create: `internal/dashboard/payment_timeline.go`
- Modify: `internal/dashboard/payments.go`, `internal/dashboard/risk.go`
- Test: `internal/dashboard/payment_timeline_test.go`

**Interfaces:**
- Consumes: `store.ListProviderPaymentAttempts`, `outbound.ResendTargets`, `paymentRiskView`.
- Produces, on `payDetail`:
  - `Glance glanceView{LastFailure, Tries, TriesTone, RiskText, RiskTone, Webhooks, WebhooksTone, Received string}`
  - `Timeline []timelineItem{Kind, At, Title, Tone string; System bool; Try *tryRow; Refund *refundRow; Webhook *webhookGroup}`
  - `tryRow{Card, Bank, Name, ThreeDS, ThreeDSTone, Declined bool, FailureCode, Provider, OrderID, PaymentID, AttemptID string; Risk *riskView}`
  - `webhookGroup{EventID, EventType string; Targets []webhookTarget; SendCount int}`
  - `webhookTarget{DeliveryID, Endpoint, Result, Tone, NextTry string; New, CanRetry bool}`
- `paymentRiskView` splits so a try's stored risk JSON can use the same mapping: `riskViewFromJSON(raw, failed string)`.

- [ ] **Step 1: Write failing tests** (table-driven, no HTTP)

`TestGlanceMissingData`: exact values from the spec table: no risk gives `Not provided` with tone `none`; a failed try with risk but no score gives `Not scored`; `FailureCode` `failed` with no message gives `No reason given`. `TestTimelineOrderAndKinds`: time order; ledger and system events have `System: true`. `TestFailedAttemptWithoutTryStillShows`: a `checkout_error` shows its message. `TestNoTriesHasOnlyCreated`. `TestSayCheckoutStarted`: the title is exactly `Checkout started`. `TestThreeDSNotVerifiedIsGrey`: `not_verified` has tone `none`, with no ✕. `TestRetryOnlyOnFailedTargets`: delivered and new targets have `CanRetry` false. `TestSendCountMatchesTargets`: in-flight targets aren't counted. `TestRiskUsesLatestTryWithRisk`: older try has risk, newer doesn't: panel uses the older; score isn't in the panel view.

- [ ] **Step 2: Run and confirm they fail**

Run: `go test ./internal/dashboard/ -run 'Glance|Timeline|FailedAttemptWithoutTry|NoTries|SayCheckout|ThreeDSNotVerified|RetryOnly|SendCount|RiskUsesLatest'`
Expected: FAIL.

- [ ] **Step 3: Implement `buildTimeline` and `buildGlance`**

Pure functions over data loaded in the existing page loader, so they stay testable. Keep every existing `payDetail` field the forms use.

- [ ] **Step 4: Run and commit**

Run: `go test ./internal/dashboard/`
Expected: PASS.

```bash
git add internal/dashboard
git commit -m "feat: build the payment timeline view"
```

### Task 8: The timeline page

**Files:**
- Modify: `web/templates/dash_payment.html`, `web/static/css/dash.css`, `frontend/dashboard/src/section-tabs.tsx`, `docs/dashboard.md`, `frontend/dashboard/e2e/risk-review.spec.ts`, any payment-page assertions in `internal/dashboard/payments_test.go`, `frontend/dashboard/e2e/peek.spec.ts`
- Create: `frontend/dashboard/e2e/payment-timeline.spec.ts`
- Test: `frontend/dashboard/src/section-tabs.test.ts` (create if absent), `internal/dashboard/payment_page_test.go`

**Interfaces:**
- Consumes: Task 7 view model; existing POST routes for sync, refund, send status, replay and `webhooks/replay`.
- Produces: root element `<section class="payment-timeline">`. `recordSplit` returns null when the page contains it.

- [ ] **Step 1: Write failing tests**

Go: `TestPaymentPageShowsTimelineNoHeadingsForTabs`. `TestPaymentPageEscapesProviderText`: a failure message and card name of `<img src=x onerror=alert(1)>` render escaped. `TestPaymentPageKeepsActions`: a privileged user sees forms for sync, refund, send status, each retry and redeliver, with the CSRF token. `TestPaymentPagePermissions`: a role without `webhook.replay` sees no webhook buttons; without refund rights no Refund; simulator tools only on simulator test payments. `TestPaymentPageSparse` and `TestPaymentPageNoTries`: expected text from the spec, with no `✕` in the sparse risk area.

TS: `recordSplit returns null for the timeline page`, and other record pages still split.

Playwright `payment-timeline.spec.ts`: no inner tabs; the top tiles are visible; "All events" reveals ledger rows; the Refund pop-up opens and closes; at 390px width the right side sits below the timeline with no horizontal page scroll. Extend `cmd/paymux-e2eseed` if it lacks a failed try.

- [ ] **Step 2: Run and confirm they fail**

Run: `go test ./internal/dashboard/ -run PaymentPage` and `cd frontend/dashboard && npx vitest run src/section-tabs`
Expected: FAIL.

- [ ] **Step 3: Implement**

Match `docs/design/paymux/payment-timeline.png` and `-sparse.png`. Use `<details>` for "Provider IDs and logs", "All events" and "Test tools", and `<dialog>` with `command="show-modal"` for Refund, as `risk_dialog` does. Reuse the dashboard's theme variables rather than the mockup's colors, and check every theme in `themes.spec.ts`. Check that interactive elements the adapter renders survive inside `details`. Check that the peek pane still renders this page.

- [ ] **Step 4: Run all page tests**

Run: `make test`, then `make frontend`, then the Playwright suite as CI runs it (see `.github/workflows/ci.yml`).
Expected: PASS.

- [ ] **Step 5: Look at it running**

Use the `webapp-testing` skill against a seeded dashboard, and screenshot full, sparse and no-tries states at desktop and 390px. Compare with the mockups.

- [ ] **Step 6: Update docs and commit**

Update `docs/dashboard.md` and any docs naming the old tabs, including the Webhook deliveries paths in `docs/webhooks.md`.

```bash
git add web frontend internal/dashboard cmd/paymux-e2eseed docs
git commit -m "feat: replace the payment page tabs with one timeline"
```

### Task 9: Final check

**Files:** none unless something fails.

- [ ] **Step 1: Run everything CI runs**

Run: `make ci` and `make test-pg` if Postgres is available.
Expected: PASS. Report anything skipped, especially the Postgres and provider sandbox tests.

- [ ] **Step 2: Walk the spec**

Check each goal, anti-goal and table row against the running code, and list any shortfalls in the report rather than leaving them silent.
