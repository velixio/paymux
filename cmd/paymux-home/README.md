# Paymux public frontend

React + Vite + TanStack Router with shared shadcn/ui components. The website has no API, database, SSR, server functions or payment-hub runtime dependency.

## Develop

From the repository root with Bun 1.4.3 or newer:

```sh
bun install --cwd frontend --frozen-lockfile
bun run --cwd frontend dev:home
```

Open `http://localhost:5174`. Source files are in `frontend/home/src/`. Shared components and tokens are in `frontend/`.

## Build

```sh
bun run --cwd frontend build:home
```

Deploy `cmd/paymux-home/dist/` to any static host, with the marketing and guide routes (`/`, `/integrations`, `/docs`, `/docs/<guide>`, `/self-hosting`) mapped to `index.html`. Asset files under `/assets/` should be served normally. The public `/docs` is a developer entry guide; the payment hub's `/docs` remains its API reference.

The separate Go binary is retained as an optional static-file server:

```sh
make build-home
./bin/paymux-home -listen 127.0.0.1:8051
```

This binary embeds the Vite build and has only static page serving and `/healthz`. It does not render React on the server or connect to the hub. Use `-listen :8051` behind a TLS reverse proxy to listen on all interfaces. A missing frontend build returns 503 rather than reporting a healthy site.

Update integration claims from `docs/capabilities.md` and `docs/provider-verification.md`. Homepage payment records remain labeled illustrations. No forms collect payment-provider credentials.

## Homepage design and browser checks

The homepage uses a light paper/cobalt palette or a dark slate palette, with an explicit theme toggle that remembers the choice. A dark payment workbench demonstrates successful captures, provider declines, and failed webhook delivery with a manual retry. Its provider selection and records are illustrative browser-only data: no network payment requests are made.

Run the repeatable Chromium checks after installing the browser:

```sh
(cd frontend && bunx playwright install chromium)
bun run --cwd frontend test:home
```

The tests build the site and launch its actual Go static server, preserving production CSP checks. They cover payment/ledger/delivery outcomes, canceling a running demo, theme persistence, system preference, keyboard tabs, mobile navigation, and five viewport widths. Set `PLAYWRIGHT_CHROMIUM_EXECUTABLE` to use an existing Chromium installation.


## Documentation library

The site renders the guides listed in `docs-catalog.json` directly from repository Markdown. The same catalog configures client navigation and the Go server's valid guide routes. Add a guide's source, slug, section, and summary there, then run `bun run --cwd frontend check:docs`, `bun run --cwd frontend test:frontend`, and `bun run --cwd frontend test:home`. Unknown guide URLs return 404. The API specification and source/example links point to the repository; guide-to-guide links stay inside the site.

Markdown parsing/rendering is in a lazy-loaded documentation chunk. Raw HTML in Markdown is skipped. Search runs locally across published guide content. Neither Markdown content nor search introduces a server API.
