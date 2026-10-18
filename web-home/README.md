# Paymux public frontend

React + Vite + TanStack Router with shared shadcn/ui components. The website has no API, database, SSR, server functions or payment-hub runtime dependency.

## Develop

From the repository root with Bun 1.4.3 or newer:

```sh
bun install --frozen-lockfile
bun run dev:home
```

Open `http://localhost:5174`. Source files are in `web-home/src/`. Shared components and tokens are in `frontend/`.

## Build

```sh
bun run build:home
```

Deploy `web-home/dist/` to any static host, with the four routes (`/`, `/integrations`, `/docs`, `/self-hosting`) mapped to `index.html`. Asset files under `/assets/` should be served normally. The public `/docs` is a developer entry guide; the payment hub's `/docs` remains its API reference.

The separate Go binary is retained as an optional static-file server:

```sh
make build-home
./bin/paymux-home -listen 127.0.0.1:8090
```

This binary embeds the Vite build and has only static page serving and `/healthz`. It does not render React on the server or connect to the hub. Use `-listen :8090` behind a TLS reverse proxy to listen on all interfaces. A missing frontend build returns 503 rather than reporting a healthy site.

Update integration claims from `docs/capabilities.md` and `docs/provider-verification.md`. Homepage payment records remain labeled illustrations. No forms collect payment-provider credentials.
