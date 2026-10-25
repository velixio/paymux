# Contributing to Paymux

Thanks for helping. Paymux moves money, so changes are small, tested and reviewed.

## The flow

1. **Branch from `main`.** One change per branch: `fix/…`, `feat/…`, `docs/…`, `chore/…`.
2. **Commit in small steps.** Write the failing test first, then the fix. Commit messages
   say what and why. Enable the commit-message hook once with
   `git config core.hooksPath .githooks`; CI runs the same check.
3. **Run the full gate locally:** `make ci`. It runs formatting, vet, the public-API
   snapshot, static analysis, the vulnerability scan, the whole test suite on SQLite and
   Postgres, the browser tests and the Docker build.
4. **Open a pull request against `main`.** Fill in the template: it lists what to check
   for money paths, migrations, provider code, auth and UI.
5. **Review.** [CodeRabbit](https://coderabbit.ai) reviews every pull request
   automatically, using the rules in [`.coderabbit.yaml`](../.coderabbit.yaml), and CI
   must be green. A maintainer reviews and merges. Answer review comments with a commit,
   not a force-push, so the history stays readable.

## Rules that are enforced

- **No secrets, tokens or real customer data** anywhere in the diff, tests or docs.
- **Both databases:** SQL lives in `internal/store`; migrations exist for SQLite and
  Postgres with the same number; the parity and upgrade tests stay green.
- **Connectors import only `pkg/...`.**
- **A provider webhook is only a hint.** Fetch the truth and match it strictly to our object.
- **No timing-based tests.** Use the fake clock; CI runners are slow and shared.
- **Docs change with behaviour.** The docs link test checks relative links and anchors.

## Where to look

- Building, testing and the architecture rules: [docs/development.md](../docs/development.md)
- Writing a connector: [docs/connectors.md](../docs/connectors.md)
- Hooks and framework mode: [docs/extending.md](../docs/extending.md)
- Reporting a security problem: do not open a public issue; contact the maintainers directly.
