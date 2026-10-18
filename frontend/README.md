# Frontend workspace

Everything JavaScript and TypeScript lives here; run `bun` commands with
`--cwd frontend` (`bun run --cwd frontend typecheck`) or from this directory.

| Path | What |
|---|---|
| `package.json`, `bun.lock` | the one Bun workspace |
| `tsconfig*.json`, `vitest.config.ts`, `playwright.home.config.ts`, `components.json` | tooling |
| `components/`, `lib/`, `styles.css` | shared shadcn/ui components, helpers and design tokens (the `@/` alias) |
| `dashboard/` | the hub's React dashboard; builds to `web/static/dashboard/` (embedded in `bin/paymux`) |
| `home/` | the public website; builds to `web-home/dist/` (served by `web-home/`, the Go server) |

`make frontend` and `make frontend-home` install and build; `make build` does
both. The website renders the Markdown guides in `docs/` and the catalog in
`web-home/docs-catalog.json`, which its Go server also embeds.
