# Security and trust boundaries

Self-hosting gives you control of provider credentials, payment records, and deployment. It also makes you responsible for access, patching, durable storage, backups, public TLS, and recovery. This guide describes implemented controls; it is not a certification or an assurance that every provider has been tested live.

## Keep the listeners separate

The public listener serves the application API, hosted customer pages, dashboard, and provider webhook endpoints. Put it behind a correctly configured HTTPS proxy.

The internal listener serves metrics, profiling, and custom-domain TLS authorization. Keep it private. If a proxy runs on another machine, restrict network access and note that `PAYMUX_TRUSTED_PROXIES` defaults to the whole local network: on a shared LAN, set it to that proxy's exact address so other hosts cannot set the client address Paymux records.

## Master key and stored credentials

The master key protects encrypted provider credentials, app signing secrets, and dashboard TOTP material. Configure exactly one of its direct-value or file-based sources. Back it up separately from the database and restrict read access.

Do not regenerate it on restart, put it in a container image, or log it. A database restore without its matching key cannot restore the encrypted secrets. See [configuration](configuration.md) and [recovery](runbook.md).

## Dashboard access

The first administrator is created using a one-time setup code in the private server log. After a user exists, setup is closed. Use unique operator accounts, strong passwords, TOTP for password sign-in, and the smallest appropriate role. Disable departed users and inspect the audit log when changing permissions.

User and role management are administrator-only. Operator access allows meaningful payment operations such as refunds; viewer access should be used for read-only work. Custom roles require an explicit action grant. Server-side authorization, session checks, CSRF protection, and login limits remain necessary even when the UI hides unavailable actions.

## OIDC sign-in

Configure issuer, client ID, and secret together and register this exact redirect URI with the identity provider:

```text
https://your-hub.example/dashboard/login/oidc/callback
```

The implementation uses authorization code flow with PKCE, state, nonce, and ID-token verification. It requires a verified email claim. Existing enabled users are matched by verified email; new users are created only when their email domain is allowed. The default provisioning role must exist and cannot be admin. An empty allowed-domain list therefore prevents automatic registration of new users; it is not a blanket block on existing users signing in.

SSO establishes the dashboard session through the identity provider. Apply the provider's MFA and account lifecycle policies; do not assume the local password/TOTP step is also run during SSO. OIDC login callback and Zoho provider OAuth callback are different URLs.

## Application secrets and scopes

API keys identify an application and mode. Keep them on application backends and assign explicit scopes where possible. Sending-money permissions are deliberately different: payout scopes must be named even on otherwise unscoped keys.

Webhook signing secrets belong to app endpoints. Provider webhook credentials belong to provider accounts. Do not reuse one for the other. Verify raw-body signatures and timestamp freshness before accepting events; use durable deduplication before fulfilling orders.

## Outbound connections

App webhooks use an SSRF-guarded client that blocks private destinations by default. Development can explicitly permit local receivers. Hook outbound HTTP has its own host allowlist. An allowlist for JavaScript hooks does not configure application webhook destinations.

Provider credentials grant provider-side access. Before adding a custom or remote connector, review its code, deployment identity, signing protocol, and allowed network destinations. A connector integration is part of your payment trust boundary.

## Hosted pages and custom domains

Checkout and hosted-document URLs contain access tokens. Avoid leaking them into public logs, analytics, shared screenshots, or referrer-sensitive redirects. Validate custom-domain ownership and proxy/TLS configuration before marking a domain ready.

The public React site is a separate static application and makes no production payment requests from its sample demo. It should never collect live provider credentials.

## Backups and incident recovery

Retain the database, key, environment configuration, and any custom deployment assets needed to reproduce the service. Practice restoring into an isolated environment. After recovery, run provider reconciliation and replay failed app deliveries; the restored database may be behind provider reality.

If a key is exposed, revoke or replace the specific affected credential and update its consumers. Keep incident evidence while limiting access to customer information. Existing controls and known review items are recorded in the [security review](security-review.md) in the source repository; that review is separate from a live infrastructure audit.
