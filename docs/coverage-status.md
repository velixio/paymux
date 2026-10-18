# Coverage measurement — 18 October 2026

This is a measured snapshot from the comprehensive documentation work, not a claim that the repository meets its 100% target. The reports include production files that tests do not execute. Generated local HTML/profile artifacts are ignored by Git; rerun the commands below to inspect current results.

## Results

| Suite | Measured result | Target |
|---|---|---|
| Go, cross-package statements | 85.1411% (22937/26940) | 100% |
| Frontend unit statements | 34.62% | 100% |
| Frontend unit branches | 36.87% | 100% |
| Frontend unit functions | 36.25% | 100% |
| Frontend unit lines | 34.66% | 100% |
| Python signed receiver, statements and branches | 100% (70 statements, 18 branches) | 100% |

All Go package tests passed in the final cross-package run; the coverage command returned nonzero because the target was not met. PostgreSQL was not available for this local run, so conditional PostgreSQL paths were not exercised. Browser checks passed separately; their execution is not folded into the Vitest unit percentage. Shell workflow line coverage is not measured.

The documentation catalog and homepage demo have 100% unit statement/branch/function/line coverage in this snapshot. This is a limited component-level result, not repository-wide completion.

## Largest Go gaps

| Package | Statement coverage | Uncovered statements |
|---|---|---|
| `internal/dashboard` | 83.04% | 688 |
| `internal/subscriptions` | 75.92% | 456 |
| `internal/store` | 85.27% | 387 |
| `internal/cli` | 81.23% | 195 |
| `internal/invoice` | 86.20% | 195 |
| `connectors/simulator` | 81.57% | 181 |
| `internal/core` | 87.26% | 112 |
| `connectors/zoho` | 84.68% | 106 |
| `hub` | 81.57% | 87 |
| `connectors/payu` | 83.62% | 87 |
| `connectors/razorpay` | 85.96% | 75 |
| `connectors/whop` | 83.66% | 75 |

## Remaining work

Add behavioral tests for dashboard actions and errors, subscription renewal/mandate failure paths, database errors and PostgreSQL behavior, provider transport/verification failures, and CLI entrypoints. Frontend gaps include dashboard runtime/render/bootstrap behavior and website bootstrap/theme behavior. Exercise them without excluding the files, weakening assertions, or substituting route counts for code coverage.

## Reproduce

```sh
bun run --cwd frontend coverage:go
bun run --cwd frontend coverage:frontend
```

Both commands enforce the 100% target and currently fail their coverage gate. See [testing and coverage](testing.md) for baseline collection, PostgreSQL configuration, browser checks, and report locations.

The Python receiver can be checked with the `coverage` development tool, installed in your own development environment:

```sh
python3 -m coverage run --branch --source=receiver -m unittest discover -s examples/webhook-receiver -p 'test_*.py'
python3 -m coverage report --fail-under=100
```
