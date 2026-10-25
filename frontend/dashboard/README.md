# Paymux React dashboard

Frontend-only React application using TanStack Router, TanStack Query, Tailwind CSS and the shared shadcn/ui components in `frontend/components/ui`. It adds no API, database, server functions or separate authentication system.

## Existing backend contract

The Go application already implements administration as authenticated HTML pages and CSRF-protected forms. `src/adapter.ts` converts those existing responses into a typed presentation model. React renders the model with shadcn controls and TanStack navigation/query state. Mutations submit the original action, method, field names, repeated values, clicked button value and upload encoding. Validation errors retain entered values. Downloads remain native browser links.

This adapter intentionally depends on the Go view contract. Keep its tests and browser workflow checks when changing templates. It does not scrape provider dashboards or store provider API credentials. Query data is not persisted; inactive page queries are discarded, and mode changes/logout clear cached data. A one-time key response stays only in the current view.

The original Go view is available until React mounts and remains a no-JavaScript fallback. `?classic=1` opens the classic view. Custom template replacements that remove the standard view structure should retain the classic experience or adopt the presentation contract.

## Develop

Install Bun 1.4.3 or newer. From the repository root:

```sh
bun install --cwd frontend --frozen-lockfile
bun run --cwd frontend dev
```

Run the existing Go hub separately at `http://localhost:18080` (adjust the proxy in `vite.config.ts` for another address). Open `http://localhost:5173/dashboard/overview`. Vite serves only the React shell and assets; requests for page data, sign-in and form actions are proxied to the existing hub. No development API is introduced.

## Build and check

```sh
bun run --cwd frontend typecheck
bun run --cwd frontend test:frontend
make build
```

`make build` compiles the dashboard frontend before embedding its assets into `bin/paymux`. Node or Bun is not required at runtime. Direct `go build` without the frontend build retains the server-rendered fallback but does not include the React UI.

Shared UI components are installed from the official shadcn registry. Add components with `(cd frontend && bunx shadcn@latest add COMPONENT)`. Vite builds client assets only; TanStack Start and server-side rendering are not used.

## Compact application shell

Operational routes fill the viewport with a 52px desktop header, a 208px sidebar, an independently scrolling content region, and a small footer. The sidebar toggle collapses navigation to a 64px icon rail; links retain accessible names and hover titles, and the browser remembers the preference. Changing routes resets content scroll, while changing sidebar size preserves the current form.

Desktop rows use 9px vertical cell padding and form controls use 32–34px heights. Phone and coarse-pointer layouts keep 44px minimum control heights. Mobile settings navigation and category tabs scroll horizontally, and filter fields use two columns where possible. Authentication screens retain their separate layout. The shell maps legacy form/style variables to the active React palette so light/dark colors remain consistent.

## Workspace tabs

The shell keeps pages open as tabs, like browser tabs. Overview is pinned (icon only). Opening a link from a pinned tab, or Alt-clicking any workspace link, opens a new tab; an ordinary click replaces the page in the current tab. At most ten unpinned tabs stay open (the least recently used one closes first), the last ten closed tabs can be reopened, and tabs can be pinned, duplicated, reordered by dragging, and closed from a context menu. Open tabs are saved per user and mode in `localStorage` and come back after a reload. On phones the strip becomes an "N open" button that opens a sheet.

The router URL is always the active tab's URL. Switching tabs replaces the history entry; navigating inside a tab pushes one; Back and Forward activate an already open tab or load the URL into the active one.

Keyboard: `[` and `]` step through tabs, `g` then `1`..`9` jumps to a tab, `g w` closes the current tab, `g t` reopens the last closed one, `Ctrl+K` (or `/`) opens the command bar and `?` lists every shortcut. Tables with links support `j`/`k` (or the arrow keys), `Home`/`End`, `Enter` to open the row and `Alt+Enter` to open it in a new tab. Code values have a copy button.

### Data retention of hidden tabs

- Hidden tabs stay mounted (scroll, filters and half-typed forms survive switching) but do not fetch, poll or re-render from data updates.
- On return, a tab refetches only if its data is older than 30 seconds and it has no unsaved edits. A tab with unsaved edits is never refetched or evicted, and closing it asks first; `beforeunload` is armed while any tab is dirty.
- A clean hidden tab is unmounted after 10 minutes without a visit (one timer per tab, no polling) and reloads when opened again.
- Pages that show a secret only once (a new API key, the setup keys, a checkout link, an enrolment code) carry a `data-once` attribute. Such a tab unmounts as soon as it is hidden, so the secret is gone when you return; the server will not show it again.
- Budget, measured with `bun run --cwd frontend build:dashboard` (gzip of every JavaScript file in `web/static/dashboard`): 156.9 KB before the redesign, 176.5 KB now. Libraries are split into hashed chunks (`vendor-react` 68 KB, `vendor-ui` 45 KB, `vendor-tanstack` 36 KB gzipped) so they stay cached across updates; `main.js` itself is 22 KB. The command bar and the revenue chart are lazy chunks (about 2.5 KB and 1.8 KB gzipped). `e2e/idle.spec.ts` checks that Overview plus four hidden tabs make no requests and run no long tasks for 60 seconds.

## Revenue chart

The top of Overview shows revenue from the ledger (charges and refunds, the same rules as the per-app tables below it, which stay as the exact figures). Everything lives in the URL query, so a tab or bookmark restores it: `range` (`today`, `7d`, `30d`, `3m`, `6m`, `12m`, `ytd`, `all`, `custom` with `from` and `to`), `bucket` (`auto`, `hour`, `day`, `week`, `month`, `quarter`), `app` and `currency`.

- Automatic buckets: Today by hour; 7D and 30D by day; 3M and 6M by week; 12M and YTD by month; All time by month, or by quarter above 36 months. Buckets follow the configured time zone; weeks start on Monday. A series is capped at 400 buckets (a finer bucket steps up to the next coarser one) and custom ranges must lie between 2000-01-01 and one year ahead.
- The server renders the buckets as a plain table, `#revenue-data`, with `data-currency`, `data-exp`, `data-bucket`, `data-converted` and `data-estimated` on the table and `data-start`, `data-gross`, `data-refunds`, `data-prev-gross` and `data-prev-refunds` (minor units) on each row. The adapter turns it into the chart; without JavaScript the table is the page. `GET /dashboard/overview/revenue.csv` takes the same query and exports the buckets.
- Currency: the default view is INR with foreign currencies converted at the stored FX rate of each day. A day without a rate uses the latest earlier rate, else the earliest later one, and the view is marked "includes estimated conversions". A currency with no rate at all is left out and listed under "Not included" with a link to the FX page. If INR is under half of the converted gross, the view opens on the largest currency in its own amounts. The currency picker shows any single currency unconverted (picking INR gives the converted view).
- The previous period (same length, ending where this one starts) is the faint line and the base of the percentage changes; no change is shown when it was zero.

### Command bar and saved filters

`Ctrl+K` opens the command bar: pages, open tabs, actions (create payment link, switch mode, toggle theme, reopen tab), and what you type. A `pay_`, `sub_`, `inv_`, `crn_` or `dsp_` id opens that record, an email searches payments and subscriptions, and other text searches payments and invoices. The last eight items are remembered per user. List pages with a GET filter form show chips for filters saved with "Save filter" (up to 12 per page and user, stored in `localStorage` as `paymux-filters:<user>:<path>`). The invoice list also searches by number or buyer.

### Follow-ups

- All-time on a very large ledger: the 1M-row benchmark (`BenchmarkLedgerSeriesAllTime`) takes about 805 ms per query against the 300 ms goal, even with the covering index. A daily rollup table is the planned fix; the default range (30D) is unaffected.
- A "tax collected" series (needs its own invoice-based query and FX path) is not in the chart yet.
