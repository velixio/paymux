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

Use **Alerts** to triage retained operational alerts. Acknowledgement records that someone reviewed the alert; it does not repair the underlying condition. Review the audit log for administrative changes.

## Themes and responsive behavior

The React dashboard has a personal light/dark control and responsive navigation. Organization appearance and per-app customer-page themes are separate settings. The marketing site's saved theme does not configure your hub. On narrow screens, tables can scroll inside their containers and navigation opens in a sheet.

## Browser errors and uncertain writes

If a write fails at the network layer, inspect the authoritative detail page before repeating it. A closed dialog or interrupted browser connection does not prove a server operation was canceled. For API-backed automation use stable idempotency keys; for dashboard operations verify the resulting resource and audit entry.
