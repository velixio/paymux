## What and why

<!-- One or two sentences: what changes, and the problem it solves. Link the issue if there is one: Closes #123 -->

## How it was tested

<!-- Commands you ran, and what you saw. For a provider change: which sandbox, which test payment. -->

- [ ] `make ci` passes locally (formatting, vet, API snapshot, static analysis, tests on SQLite **and** Postgres, browser tests, Docker build)
- [ ] New behaviour has tests, including the failure and retry paths
- [ ] Tested against the real provider sandbox (for connector changes): ____________

## What it touches

<!-- Tick everything that applies. Each box is a prompt for extra care in review. -->

- [ ] **Money paths**: payments, refunds, subscriptions, invoices, tax, payouts. Amounts stay integer minor units; state, ledger and events commit in one transaction; a retry cannot move money twice
- [ ] **Database**: a migration for **both** SQLite and Postgres, same number, schema parity and upgrade tests updated. No applied migration was edited
- [ ] **Provider or webhook code**: signatures verified in constant time; the truth is fetched, not taken from the webhook body; no credential in fixtures or errors
- [ ] **Auth, sessions, roles or the dashboard**: every handler has an authorization action; CSRF and tenant scoping covered
- [ ] **Public API or SDK**: `docs/openapi.yaml` and `scripts/apisnap/pkg.txt` updated, and `docs/capabilities.md` regenerated if connector capabilities changed
- [ ] **UI**: checked at 360px wide, keyboard-only and in dark mode (screenshots below)
- [ ] None of the above (docs, tests or tooling only)

## Safety checklist

- [ ] No secrets, tokens or real customer data in the diff, tests, fixtures or docs
- [ ] No `Co-Authored-By` or other attribution trailers in the commit messages (a commit hook and CI reject them)
- [ ] Docs updated where behaviour changed (`docs/`), and the docs link test passes
- [ ] Breaking changes and upgrade steps are noted in `docs/upgrades.md`

## Screenshots

<!-- For any UI change: before and after. Delete this section otherwise. -->

## Notes for the reviewer

<!-- Anything unusual: trade-offs, what you chose not to do, what is deliberately left for later. -->
