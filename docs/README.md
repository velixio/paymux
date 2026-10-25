# Paymux documentation

Paymux is a self-hosted payment hub between your applications and payment providers. It gives each application one API for payments and related operations while preserving provider-specific capabilities and limitations.

These guides describe the implementation in this repository. Connector capability declarations are not evidence of live-provider certification: consult the [provider verification checklist](provider-verification.md) before production use.

## Start here

1. [Installation](installation.md): build or download the hub and understand the separate public website.
2. [Simulator quickstart](quickstart.md): create an app, receive an event, make a test payment, and issue a refund without a bank account.
3. [Integration guide](api-guide.md): authentication, scopes, amounts, errors, pagination, and idempotency.
4. [Payments and routing](payments.md): payment versus attempt state, route selection, checkout, and refunds.
5. [Webhooks](webhooks.md): signature verification, duplicate delivery, ordering, and replay.

## Operate the product

- [Dashboard guide](dashboard.md): apps, providers, roles, settings, and operational workflows.
- [Subscriptions and invoicing](billing.md): recurring billing modes, proration, GST document flow, and limitations.
- [Configuration reference](configuration.md): deployment variables, secrets, SMTP, SSO, and hooks.
- [Deployment and recovery runbook](runbook.md): Linux, containers, TLS, backups, restore, and monitoring.
- [Security](security.md): boundaries, credentials, dashboard access, domains, and outbound delivery.
- [Upgrade guide](upgrades.md): backups, migrations, updater, and rollback.
- [Troubleshooting](troubleshooting.md): symptoms, checks, and recovery paths.

## Providers and extension points

- [Capability matrix](capabilities.md): generated from connector declarations.
- [Connector development](connectors.md): implement a Go connector or remote service.
- [Provider verification](provider-verification.md): remaining real-account checks.
- [Hooks and framework mode](extending.md): extend the hub using Go or JavaScript.
- [Performance](performance.md): measured workload and benchmark commands.
- [Testing and coverage](testing.md): test suites, reports, and the repository-wide 100% target.
- [Dependency notes](dependencies.md): build and runtime dependencies.
- [Glossary](glossary.md): canonical product terminology.
- [Features](features.md): the complete feature list.
- [Access control](access-control.md): roles, API key scopes, payouts, logs, key rotation and single sign-on.
- [Development](development.md): building, testing, architecture rules and the public website.

## API reference and examples

The running **payment hub** serves its API reference at `/docs` and the machine-readable contract at `/docs/openapi.yaml`. The separate **public website** also has a `/docs` path, which serves these guides; it does not proxy your hub or expose an API.

Use [OpenAPI](openapi.yaml) for request and response fields. The [Go payment client example](../examples/api-client/main.go) uses the standard HTTP library. The [signed webhook receiver](../examples/webhook-receiver/receiver.py) demonstrates signature checks and a durable inbox. The [framework example](../examples/framework/main.go) extends the hub itself; it is not an application HTTP client.

## Documentation maintenance

Guide Markdown in `docs/` is also rendered by `cmd/paymux-home/`; edit the source once. The website catalog explicitly selects public guides. Keep code links and examples tied to the checked-in API contract, and run `bun run check:docs`, `bun run typecheck`, and `bun run test:home` after changing content or navigation. Internal planning documents are not part of the public catalog.
