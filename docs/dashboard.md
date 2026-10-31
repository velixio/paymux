# Dashboard operations

The dashboard operates the hub's existing records. It is not the public website or its sample payment demo. Sign in on your own hub under `/dashboard/`, check the selected mode, and confirm the application and provider account before changing anything.

## Navigation and roles

The navigation groups payment operations, billing, integration configuration, and administration. Available pages and actions depend on your role.

| Built-in role | Intended access |
|---|---|
| Viewer | Read payment and refund records and settings. |
| Operator | Viewer access plus operational actions such as payment cancellation, refunds, dispute handling, replay, and invoice reissue. |
| Admin | Configuration, app/key/provider management, payouts, users, roles, and audit access. |

Custom roles can receive explicit actions. User and role management remain administrator-only. A hidden action is not an invitation to call its URL directly: the server checks permissions on writes as well. API key scopes are a separate control from dashboard roles.

## Signing in, "Remember me" and your sessions

The login page has a **Remember me for 30 days** checkbox, off by default (the number is the setting `session_remember_days`; 0 hides the checkbox). Without it a sign-in ends after 12 hours idle or 7 days at most (`session_idle_hours`, `session_max_days`). With it the sign-in lasts up to the chosen number of days from the moment you signed in, as long as you use it at least once in that many days; both limits are that number, and using the dashboard does not extend it. You still enter the password and the authenticator code every time you sign in. The three settings are on *Settings*, under *Sign-in sessions*, with the value in effect and where it comes from.

**Your sessions** (the link in the header) lists where you are signed in, with *Sign out* for each, *Sign out all other sessions* and *Change password* (which ends every session, this one too). Administrators can end all sessions of any user from the Users page (*Sign out everywhere*). Creating keys, changing roles and similar actions ask for your password and code again when the last full sign-in is more than 12 hours old.

## Applications and keys

An application owns its API integration and webhook endpoints. Its detail tabs include keys, webhooks, branding, providers, domains, tax, and settings. **Branding → Accepted currencies** is an optional allow-list: blank stays dynamic and permits any currency supported by the selected provider route; for example `USD, INR, EUR` rejects every other currency before checkout.

- Create keys for the correct mode and minimal required scopes. Copy new secret material when displayed; it is not recoverable from a later page visit.
- Revoke unused keys after migrating callers. Do not rotate a key without updating its consumers.
- Link eligible provider accounts to the app. A provider account existing in the organization does not by itself make it available to every app.
- Keep app return URLs, domain settings, and webhook receiver destinations distinct.

## Provider accounts

The list shows account, connector, mode, status, currencies, and purposes. Open an account for credentials, app links, provider-specific configuration, and the provider webhook URL. The detail page also retains inbound payload/header history, processing errors, matched Paymux transactions, and trace-log links. Webhook URLs are deliberately absent from the list.

Register that exact account URL in the provider dashboard. A provider account's webhook secret is not your app endpoint's `whsec_` signing secret. For Zoho connection and callback registration, follow the [runbook](runbook.md).

## Trace a payment

Start with the payment ID or your order reference in **Payments**. Inspect:

1. Requested amount/currency, received/refunded totals, status, and flags.
2. Provider attempts and any failure codes.
3. Ledger records and events.
4. Application delivery attempts and response outcomes.
5. Related refunds, invoices, and disputes.

A refund request or replay action is different from provider reconciliation. Use the detail page to confirm the target, review the action, and inspect the final server response. Payment and attempt status are explained in [payments](payments.md).

## Settings and inheritance

Organization settings are grouped by purpose. App settings show effective values and sources and allow supported overrides. Switching groups retains edits; a navigation warning lets you keep editing or discard unsaved changes. A review dialog precedes configured changes.

Save and confirm the effective value. A validation error leaves the submitted value available to correct; it does not mean the new policy became active. Resetting an app override restores inheritance from the organization.

Routing settings include a route tester. Verify the intended application, currency, amount, country, and purpose before applying new traffic rules.

Routing rules can also cap successful sales or captured amount per UTC day, week, or month. When a rule's limit is exhausted, evaluation continues to the next matching rule or catch-all. Amount limits use minor units. These counters are routing controls, not an accounting limit; the ledger and provider remain authoritative.

**Organization settings → Notifications** sends selected mode-specific events to an email address, a Telegram chat, or both. Payment messages include the amount and, when collected, the customer's email and country. Telegram bot tokens are encrypted and shown only while entered. Telegram messages can be kept or deleted automatically after 1–47 hours; 47 hours is the default because the Bot API cannot delete messages once they are 48 hours old. The deletion is a durable job, so it survives a Paymux restart. Recommended events cover payment creation/success/failure, refunds, and disputes; customize the comma-separated list when an operator should receive fewer messages.

## Subscriptions: changing the amount

On a subscription's page, *Change the amount* sets a new amount per period (from the next period; for a subscription Paymux bills itself the difference can also be charged for the current one). The control follows what the provider can do (`connector.Info.CanChangeSubscriptionAmount`): where it keeps the plan of a subscription paid by a given method (Razorpay does for cards and UPI), the form is not shown and the page says *the provider cannot change the plan of a card subscription; cancel it and start a new one*. If a change is attempted anyway, or the provider refuses it, the page shows the same words or the provider's own message (with secrets removed) instead of a generic error. The change is audited as `subscription.update`. The other controls (cancel, a new checkout link for a customer who has to authorise a mandate, the simulator's renewal) appear only where they apply to the subscription's billing model.

## Disputes, reconciliation, and alerts

Use **Disputes** to track due dates, inspect provider capabilities, upload evidence, and submit or accept supported actions. Some providers require manual dispute entry. An evidence file being uploaded locally is not equivalent to a successful provider submission.

Use **Reconciliation** to compare hub records with a provider and inspect unresolved items. After an outage or restore, run it for affected accounts before assuming your records are current.

Use **Logs** (permission `logs.read`: admins, or a custom role you give it) to investigate a failure: the errors and warnings kept in the database, failed provider calls with their redacted request and response, rejected API and webhook requests and failed jobs. Filter by time, level, kind, connector, account, request id, payment id, any other id and text; follow an id to the rows that share it; export the filtered rows as JSON lines. Payments, attempts, refunds and subscriptions link to their own logs. See the runbook's *Logs* section.

Use **Update** (admins only) to install a newer release: it shows the running and the latest version with a link to the release notes, then downloads, verifies (checksum and cosign signature, always) and installs it and restarts Paymux onto the new binary. The button is disabled, with the reason, when `cosign` is missing or the binary cannot be replaced (then update with your package manager or image). An older or the same release needs the *Force* box. Take a backup first; the migrations run on the restart. Details and limits are in [Upgrades](upgrades.md#updating-from-the-dashboard).

Use **Alerts** to triage retained operational alerts. Acknowledgement records that someone reviewed the alert; it does not repair the underlying condition. Review the audit log for administrative changes.

## Overview and payment analytics

**Overview** combines the native-currency revenue summary with a compact payment-health chart and the top customer countries. Use **Analytics** for the detailed view: created, paid, failed, waiting/review, and expired/canceled payment cohorts; country rankings and CSV export; provider and payment-method attempt performance; and the most frequent provider failure reasons.

Analytics can be filtered by date range, app, native currency, customer country, and time bucket. Counts are grouped by the payment creation time, while each payment is classified by its current status. Provider and method tables are attempt-level, so a failed retry followed by a successful retry remains visible. Country uses the payment routing country when present and otherwise the stored customer billing country. **Payments** links in the country table open the matching transactions.

Live and test data are always isolated by the dashboard mode switch. Amounts are never combined across currencies with a static FX rate; choose one native currency at a time. Provider settlement data remains authoritative.

Operational pages update automatically while the dashboard is open. A single authenticated server-sent events stream carries lightweight change notices scoped to the signed-in organisation and selected mode; it never sends payment or webhook payloads. The browser coalesces bursts and reloads clean Overview, Analytics, Payments, Subscriptions, Invoices, Disputes, Reconciliation, Reports, and Alerts data. Pages with unsaved edits are left alone. The status bar shows **Live** when the stream is connected and **Reconnecting** while the browser resumes it using the last event ID.

## Tabs, revenue chart and shortcuts

The dashboard keeps pages open as tabs. Overview is pinned. Alt-click a link (or open a page from the pinned Overview) to open it in a new tab; reload the browser and your tabs come back. A tab with unsaved edits shows a dot and asks before it closes. A page that shows a secret only once, such as a new API key, closes its content when you leave the tab, so copy the secret before switching. Shortcuts: `[` and `]` switch tabs, `g` then `1` to `9` jumps to a tab, `g w` closes the current tab, `g t` reopens the last closed one, `Ctrl+K` (or `/`) opens the command bar and `?` lists all shortcuts. In tables, `j` and `k` move between rows, `Enter` opens one and `Alt+Enter` opens it in a new tab.

The revenue section supports Today, 7D, 30D, 3M, 6M, 12M, YTD, All time, and custom dates; hour, day, week, month, and quarter buckets; and app and currency filters. Gross, refunds and net include change against the previous period of the same length, and **Download CSV** exports the buckets. USD is preferred when it has activity; otherwise the chart uses the active native currency with the highest activity measured in minor units. The currency picker and native-currency breakdown show each currency without estimated conversion.

The command bar finds pages, open tabs and actions, opens a payment, subscription, invoice or dispute by its id, and searches payments, subscriptions and invoices for an email address or any text. On list pages, **Save filter** keeps the current filters as a chip in this browser; the invoice list also searches by invoice number or buyer.

## Themes and responsive behavior

The React dashboard uses a compact application shell with a fixed header and independently scrolling content. Collapse the desktop sidebar to an icon rail using **Collapse sidebar**; **Expand sidebar** restores labels. The browser remembers the choice. Changing routes starts the content at the top, and changing sidebar size keeps current form edits.

The dashboard has a personal light/dark control and responsive navigation. The **Appearance** page chooses the organisation's dashboard theme (Stripe, Paddle, Midnight or your own); each person can pick their own on the **Account** page. Making, editing, exporting and importing themes is covered in [Dashboard themes](themes.md). Per-app customer-page themes are a separate setting on the same page. The marketing site's saved theme does not configure your hub. On narrow screens, tables can scroll inside their containers and navigation opens in a sheet.

## Browser errors and uncertain writes

If a write fails at the network layer, inspect the authoritative detail page before repeating it. A closed dialog or interrupted browser connection does not prove a server operation was canceled. For API-backed automation use stable idempotency keys; for dashboard operations verify the resulting resource and audit entry.
