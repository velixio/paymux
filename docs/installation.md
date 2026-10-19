# Installation

Paymux has two independently deployable programs: the **payment hub** (`paymux`) and the **public website** (`paymux-home`). Your applications integrate with the hub. Hosting the website does not start a payment service.

## Requirements

For a source build, install Git, Make, Bun 1.4.3 or newer, and Go with automatic toolchain downloads enabled. `go.mod` currently declares Go 1.27 and pins toolchain `go1.27.2`; use that toolchain directly if your build environment cannot download it. The build requires network access to fetch Go and frontend dependencies.

A distributed hub binary does not need Bun, Node, or a compiler at runtime. It needs a persistent database, the master key used to encrypt secrets, and a writable data directory. Production deployments also need a public HTTPS origin and a reverse proxy.

## Build from source

```sh
git clone https://github.com/velixio/paymux.git
cd paymux
make build
./bin/paymux version
```

`make build` installs frontend packages from `bun.lock`, checks TypeScript, builds the React dashboard and the website, and writes every binary to `bin/`: `paymux` (the hub, with the dashboard embedded), `paymux-home` (the public website) and the example programs. `make build-paymux` builds only the hub. `make dev` builds everything and runs the hub and the website together (`HOME_LISTEN`, default `0.0.0.0:8051`, reachable from the LAN); Ctrl-C, or either one exiting, stops both. A direct `go build` does not build frontend assets: prefer Make for a complete distribution.

To build the separate public site:

```sh
make build-home
./bin/paymux-home -listen 127.0.0.1:8051
```

Its only runtime responsibilities are serving static pages and `/healthz`. It requires no hub environment file or database. Alternatively, deploy `cmd/paymux-home/dist/` on a static host with SPA fallback for documentation routes.

## Use a release binary

Download the archive matching your operating system and CPU from the repository's releases. Verify its checksum against `SHA256SUMS` before unpacking. Signature verification, when signing artifacts are supplied, is described in the [upgrade guide](upgrades.md).

Do not copy a version-specific download URL from an old guide: select the release you intend to run. `paymux version` reports the installed version.

## Initialize the hub

Use the [simulator quickstart](quickstart.md) for a local-only trial. For a deployment, create the master key once, store it securely, set `PAYMUX_BASE_URL`, configure persistence, and follow the [runbook](runbook.md).

The first administrator is created at `/setup` using the one-time code in the server log. There is no default password. Complete setup promptly and keep logs private. API keys and endpoint signing secrets are displayed once: save them in your application's secret store.

## Choose a database

| Database | Appropriate use | Operational responsibility |
|---|---|---|
| SQLite | One hub process with persistent local storage | Keep the database and related files on durable storage; stop the hub for CLI commands that need its exclusive lock. |
| PostgreSQL | Deployments needing shared database access or managed recovery | Set `PAYMUX_DATABASE_URL`, configure database TLS and backups, and manage connection availability. |

Migrations run on startup. Database rollback requires the matching pre-upgrade backup; replacing the binary alone does not undo schema changes. See [upgrades](upgrades.md).

## Confirm a working installation

1. `GET /healthz` responds on the hub's public listener.
2. `GET /readyz` reports ready, rather than merely an alive process.
3. First-run setup creates an administrator and application.
4. A linked test-mode simulator completes a payment.
5. The application receives and verifies `payment.succeeded`.

A reachable homepage or a browser redirect is not proof of a healthy payment integration.
