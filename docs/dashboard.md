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

An application owns its API integration and webhook endpoints. Its detail tabs include keys, webhooks, branding, providers, domains, tax, and settings.

- Create keys for the correct mode and minimal required scopes. Copy new secret material when displayed; it is not recoverable from a later page visit.
- Revoke unused keys after migrating callers. Do not rotate a key without updating its consumers.
- Link eligible provider accounts to the app. A provider account existing in the organization does not by itself make it available to every app.
- Keep app return URLs, domain settings, and webhook receiver destinations distinct.

## Provider accounts

The list shows account, connector, mode, status, currencies, and purposes. Open an account for credentials, app links, provider-specific configuration, and the provider webhook URL. Webhook URLs are deliberately absent from the list.

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

## Disputes, reconciliation, and alerts

Use **Disputes** to track due dates, inspect provider capabilities, upload evidence, and submit or accept supported actions. Some providers require manual dispute entry. An evidence file being uploaded locally is not equivalent to a successful provider submission.

Use **Reconciliation** to compare hub records with a provider and inspect unresolved items. After an outage or restore, run it for affected accounts before assuming your records are current.

Use **Logs** (permission `logs.read`: admins, or a custom role you give it) to investigate a failure: the errors and warnings kept in the database, failed provider calls with their redacted request and response, rejected API and webhook requests and failed jobs. Filter by time, level, kind, connector, account, request id, payment id, any other id and text; follow an id to the rows that share it; export the filtered rows as JSON lines. Payments, attempts, refunds and subscriptions link to their own logs. See the runbook's *Logs* section.

Use **Alerts** to triage retained operational alerts. Acknowledgement records that someone reviewed the alert; it does not repair the underlying condition. Review the audit log for administrative changes.

## Tabs, revenue chart and shortcuts

The dashboard keeps pages open as tabs. Overview is pinned. Alt-click a link (or open a page from the pinned Overview) to open it in a new tab; reload the browser and your tabs come back. A tab with unsaved edits shows a dot and asks before it closes. A page that shows a secret only once, such as a new API key, closes its content when you leave the tab, so copy the secret before switching. Shortcuts: `[` and `]` switch tabs, `g` then `1` to `9` jumps to a tab, `g w` closes the current tab, `g t` reopens the last closed one, `Ctrl+K` (or `/`) opens the command bar and `?` lists all shortcuts. In tables, `j` and `k` move between rows, `Enter` opens one and `Alt+Enter` opens it in a new tab.

**Overview** starts with a revenue chart. Pick a range (Today, 7D, 30D, 3M, 6M, 12M, YTD, All time, or custom dates), a bucket (hour, day, week, month, quarter), an app and a currency. Gross, refunds and net come with the change against the previous period of the same length, and **Download CSV** exports the buckets. Amounts are shown in rupees by default: other currencies are converted at the exchange rate stored for each day. If a day has no rate the nearest stored one is used and the chart says it includes estimated conversions; a currency with no rate at all is listed as not included until you add rates on the **FX rates** page. Choose a currency in the picker to see its own amounts with no conversion.

The command bar finds pages, open tabs and actions, opens a payment, subscription, invoice or dispute by its id, and searches payments, subscriptions and invoices for an email address or any text. On list pages, **Save filter** keeps the current filters as a chip in this browser; the invoice list also searches by invoice number or buyer.

## Themes and responsive behavior

The React dashboard uses a compact application shell with a fixed header and independently scrolling content. Collapse the desktop sidebar to an icon rail using **Collapse sidebar**; **Expand sidebar** restores labels. The browser remembers the choice. Changing routes starts the content at the top, and changing sidebar size keeps current form edits.

The dashboard has a personal light/dark control and responsive navigation. Organization appearance and per-app customer-page themes are separate settings. The marketing site's saved theme does not configure your hub. On narrow screens, tables can scroll inside their containers and navigation opens in a sheet.

## Browser errors and uncertain writes

If a write fails at the network layer, inspect the authoritative detail page before repeating it. A closed dialog or interrupted browser connection does not prove a server operation was canceled. For API-backed automation use stable idempotency keys; for dashboard operations verify the resulting resource and audit entry.
