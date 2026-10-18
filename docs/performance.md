# Performance

Targets come from spec section 12. A load test checks them against the real
in-process app, on SQLite and on PostgreSQL.

| Target | Limit |
|---|---|
| `POST /v1/payments` at 20 req/s sustained | p99 < 100 ms |
| Pay page render, provider excluded (fake connector) | < 1.5 s |
| Invoice PDF render | < 500 ms |
| RSS after 10,000 payments | < 150 MB |

## Running it

The test lives behind the `loadtest` build tag so normal `go test ./...` never
runs it (CI only vets it: `go vet -tags loadtest ./...`).

```sh
# SQLite only
go test -tags loadtest -run 'TestLoad/sqlite' -v -timeout 30m ./hub

# both, with a PostgreSQL to hand
PAYMUX_TEST_POSTGRES_URL='postgres://user:pass@127.0.0.1:5432/paymux?sslmode=disable' \
  go test -tags loadtest -run TestLoad -v -timeout 30m ./hub

# profile
go test -tags loadtest -run 'TestLoad/postgres' -cpuprofile cpu.pprof -memprofile mem.pprof ./hub
go tool pprof -top -cum hub.test cpu.pprof
```

Knobs: `LOADTEST_SECONDS` (length of the sustained phase, default 20),
`LOADTEST_CLIENTS` (concurrent API clients in that phase, default 8) and
`LOADTEST_PAYMENTS` (payments created before the memory check, default
10000). The test fails when a target is missed.

## What it does

1. **Sustained.** Starts the app on a loopback port with a fake provider and
   sends `POST /v1/payments` on a fixed 20 req/s schedule (open loop: a slow
   response does not delay the next request) for 20 s, spread over 8 clients,
   each with its own API key. p99 is taken over all requests.
2. **Pay page.** Opens `GET /pay/{id}?t=...` for 60 of those payments. The page
   starts the checkout through the fake connector, so this measures Paymux's
   own work: payment and routing lookups, the checkout attempt, template
   rendering.
3. **Invoice PDF.** Renders a five-line inter-state GST invoice with the
   document renderer 50 times.
4. **Memory.** Creates payments up to 10,000 in total with 32 clients (each
   paced under the 20 req/s per-key rate limit), then runs a GC, returns freed
   memory to the OS and reads `VmRSS`. The test process itself (HTTP client,
   test harness) is in the same RSS, so the figure is an upper bound for the
   server.

## Results

Run on a 16-core AMD Ryzen 9 7950X, Go 1.27.2, SQLite on local disk, PostgreSQL
16 in a local container, everything on loopback. Your numbers will differ; the
targets leave a wide margin.

| Measure | Target | SQLite | PostgreSQL |
|---|---|---|---|
| `POST /v1/payments` p50 at 20 req/s | | 1.2 ms | 8.5 ms |
| `POST /v1/payments` p99 at 20 req/s | < 100 ms | 7.8 ms | 21 ms |
| `POST /v1/payments` max | | 14 ms | 40 ms |
| Pay page p99 | < 1.5 s | 25 ms | 37 ms |
| Invoice PDF render p99 | < 500 ms | 62 ms | 48 ms |
| Bulk creation, 10,000 payments | | 572/s | 556/s |
| RSS after 10,000 payments | < 150 MB | 45 MB | 48 MB |

On a cold PostgreSQL container, the first run after it started showed a
p99 of 345 ms in the sustained phase (connection set-up and a cold buffer
cache); runs against a warm database stay well under the limit. Treat the
first run on a freshly created database as a warm-up.

## Findings

Profiles show the server mostly waiting: the CPU was busy about 7% of the time
during the sustained phase, mostly in database round trips, so
latency is set by the number of queries per request, not by computation.

- **API-key authentication is cheap.** Keys are verified with a SHA-256 lookup
  by prefix and a constant-time compare. There is no argon2 (or any other
  deliberately slow hash) on the API path; argon2 is only used for dashboard
  passwords.
- **Settings resolution** was the largest avoidable cost. Creating a payment
  read the payment TTL and the routing rules (org and app level) from the
  database on every request, each read costing up to four queries (the app
  row, the app override, the org value, the org id). About 16 queries ran per
  `POST /v1/payments`; resolving policy values through a cache removes
  about six of them (counted with a temporary SQL trace).
  - `settings.Service` now keeps a cache with a short TTL
    (`settings.DefaultCacheTTL`, 3 s). Every `SetOrg`, `SetApp`, `ResetApp` and
    `SeedOrg` clears it once the change has committed, and a read that raced
    with a change is not stored, so a change made through the running server
    is visible at once.
  - A change made by a different process (the CLI, or a second node on a
    shared PostgreSQL) shows within the TTL.
  - Only policy reads use it: `ResolveCachedWith` for payment TTL, the
    duplicate-payment action and the routing rules. `ResolveWith` is
    unchanged and still sees a transaction's own writes (the settings service
    uses it to record the "before" value of an audited change).
- **PDF rendering** spends its time in font parsing and PDF assembly; at about
  30 ms per invoice (and the PDF is rendered once and stored) it is far from
  the 500 ms limit, so it was left alone.
- **Memory** is flat: the heap in use stays around 5-7 MB after 10,000
  payments, and RSS is dominated by the Go runtime, the database drivers and
  the embedded assets.

Not done, because the targets are met with a wide margin: caching the app row
(it is read about three times per payment creation) and batching the write
transaction's reads.
