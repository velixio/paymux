# Installation

Paymux has two independently deployable programs: the **payment hub** (`paymux`) and the **public website** (`paymux-home`). Your applications integrate with the hub. Hosting the website does not start a payment service.

## Requirements

For a source build, install Git, Make, Bun 1.4.3 or newer, and Go with automatic toolchain downloads enabled. `go.mod` declares Go 1.27.3, the minimum version; a newer installed Go builds it as it is, and an older one downloads 1.27.3 by itself. Install Go 1.27.3 or newer directly if your build environment cannot download it. The build requires network access to fetch Go and frontend dependencies.

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

### The install script

Each release also publishes `install.sh`, covered by the release's `SHA256SUMS` and signature like the archives:

```sh
curl -fsSL https://github.com/velixio/paymux/releases/latest/download/install.sh | sh
```

It supports Linux and macOS on amd64 and arm64. It downloads the archive for the machine and `SHA256SUMS`, always checks the checksum, checks the cosign signature of `SHA256SUMS` against the release workflow of the repository when `cosign` is installed (the same check as `paymux update`), unpacks only the `paymux` binary, runs its `version`, and installs it. Without `cosign` it installs on the checksum alone, prints a warning and the `cosign verify-blob` command to run yourself. The checksum comes from the same release page as the archive, so it detects a damaged download, not a tampered release; only the signature does that.

| Setting | Meaning |
|---|---|
| `PAYMUX_VERSION=v0.1.0` | Install that release (the `v` is optional) instead of the latest. |
| `PAYMUX_INSTALL_DIR` | Where to put `paymux`. Default `/usr/local/bin`. |
| `PAYMUX_USE_SUDO=1` | Use `sudo` if the install directory is not writable. Without it the script stops and says so; it never calls `sudo` on its own. |
| `PAYMUX_REQUIRE_SIGNATURE=1` | Fail, instead of warning, when the signature cannot be verified (no `cosign`, or an unsigned release). Recommended for automation. |
| `PAYMUX_REPO`, `PAYMUX_DOWNLOAD_BASE`, `PAYMUX_COSIGN` | A fork or mirror, and the cosign program to use. |

To read the script before running it, download `install.sh` from the release page and check it against `SHA256SUMS`. The stable links `https://github.com/velixio/paymux/releases/latest/download/paymux_<os>_<arch>.tar.gz` (`linux` or `darwin`, `amd64` or `arm64`) always point at the newest release.

Do not copy a version-specific download URL from an old guide: select the release you intend to run. `paymux version` reports the installed version.

## Initialize the hub

Use the [simulator quickstart](quickstart.md) for a local-only trial. For a deployment, create the master key once, store it securely, set `PAYMUX_BASE_URL`, configure persistence, and follow the [runbook](runbook.md).

The first administrator is created at `/setup` using the one-time code in the server log. There is no default password. Complete setup promptly and keep logs private. API keys and endpoint signing secrets are displayed once: save them in your application's secret store.

## Two host names (recommended in production)

Serve the customer pages from a second host name so that nothing running on a payment page (a provider's script, a remote connector's page) shares an origin, and so cookies, with the dashboard:

| Host | Serves | Setting |
|---|---|---|
| `pay.example.com` | dashboard, `/v1` API, `/hooks`, `/setup`, `/docs` | `PAYMUX_BASE_URL=https://pay.example.com` |
| `checkout.example.com` | `/pay/`, `/l/`, `/s/`, `/i/`, `/return/`, `/sim/`, `/assets/`, `/theme/` and nothing else (404) | `PAYMUX_CHECKOUT_URL=https://checkout.example.com` |

Point both names at the same proxy and the same hub listener (`PAYMUX_LISTEN`); the hub tells them apart by `Host`. With the Caddy kit the second name needs no extra block: the catch-all block issues its certificate on demand and the hub's `tls-allow` answer approves it. With nginx, give the checkout host its own server block and certificate that proxies to the same upstream, and forward the `Host` header. Dashboard cookies are host-only, so the checkout host never receives them. Custom domains of apps work as before. Turning this on on a running hub changes the URLs registered at providers: see [upgrades](upgrades.md#release-note-the-customer-pages-get-their-own-host).

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
