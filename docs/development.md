# Development

Building, testing and the architecture rules, and the public website.

## Development

```bash
make test       # SQLite
make test-pg    # Postgres (uses PAYMUX_TEST_POSTGRES_URL or starts postgres:17 in Docker)
make ci         # gofmt, vet, API snapshot, capability matrix, staticcheck, govulncheck, both databases, cross builds, docker build
scripts/release.sh v1.2.3   # release archives with checksums (and a cosign signature if cosign is installed)
```

**Architecture rules, enforced by `internal/seams`:**

- All SQL lives in `internal/store`.
- Connectors import only `pkg/...`.
- `main.go` stays thin.
- Policy values are read through `internal/settings`.

## Public website

The React marketing and developer-entry website (sources in [`frontend/home/`](../frontend/home), Go server in [`cmd/paymux-home/`](../cmd/paymux-home)) runs independently of the payment hub. Both frontends use TanStack Router and shadcn/ui; the dashboard also uses TanStack Query. They use Vite client builds, without TanStack Start, SSR or new APIs. The home site can deploy directly to a static host or use its separate Go serving binary.

```bash
bun install --cwd frontend --frozen-lockfile
make build-home
./bin/paymux-home -listen 127.0.0.1:8051
```

Routes: `/`, `/integrations`, `/docs`, `/self-hosting`; readiness: `/healthz`. Bind behind your TLS reverse proxy for public hosting. The payment hub continues to serve its own operational dashboard and API reference separately.

Frontend development: `bun run --cwd frontend dev:home` serves the public site; `bun run --cwd frontend dev` serves the dashboard with a proxy to the existing hub. See [`frontend/dashboard/README.md`](../frontend/dashboard/README.md) for the existing page/form adapter contract and development setup.
