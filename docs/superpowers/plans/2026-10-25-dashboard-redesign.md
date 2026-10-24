# Dashboard Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the React dashboard into a Stripe-style, app-like workspace with browser-style workspace tabs, a revenue chart on Overview, a command bar and the smaller workspace features in the spec.

**Architecture:** Pages keep coming from the Go HTML views through `frontend/dashboard/src/adapter.ts`. Tabs split today's `Workspace` component into a persistent shell plus one mounted (but hidden) `TabView` per tab. The revenue chart reads a bucketed data table that the Go overview handler renders into the page. That table doubles as the no-JS fallback. New client logic lives in small pure `.ts` modules with Vitest tests; components stay thin.

**Tech Stack:** Go 1.27 `html/template`, SQLite + Postgres; React 19, TanStack Router/Query, Tailwind 4, shadcn/Radix, lucide-react, Vite, Bun 1.4.3, Vitest (happy-dom), Playwright.

**Spec:** `docs/superpowers/specs/2026-10-25-dashboard-redesign-design.md`. Read it before every task. Each section number (§) below refers to it.

## Global Constraints

- **Data contract:** no JSON API and no new auth. Pages still come from Go HTML through `adapter.ts`. Keep `?classic=1` and the no-JS fallback working.
- **Customer pages must not change:** don't edit `web/themes/default/theme.json`, `web/static/css/app.css` or the `pay_*` / `invoice` / `sim_checkout` templates.
- **Bundle:** the dashboard bundle may grow at most **~25 KB gzipped** in total. Add no new runtime dependency except `@fontsource-variable/inter` (fonts, self-hosted). The command bar and chart are lazy-loaded (`React.lazy`).
- **Idle CPU ~0:** no polling in hidden tabs or while `document.hidden`, and no animation loops. Motion is transform/opacity only, 120–200 ms, and off under `prefers-reduced-motion`.
- **Accessibility:** WCAG AA (4.5:1) in light and dark. Never remove focus rings. Icon-only buttons need `aria-label`. Phone controls stay ≥ 44px.
- **Shortcuts** only fire when focus is not in an `input`, `textarea`, `select` or `[contenteditable]`, and they match `event.code`. Never bind Ctrl/Alt+digits, Ctrl+W or Ctrl+T.
- **Storage:** `localStorage` holds only URLs, titles, pins and saved-filter query strings. Wrap every access in try/catch. Never store page content or form values.
- **Process:** TDD with a commit after every red and every green step. Never squash. **No `Co-Authored-By` or other attribution trailers.** Never use a private email.
- **Go stores:** every new query has tests through `storetest.EachMigrated` (SQLite and Postgres).
- **Gates before each commit of code:** `go test ./...` (Go tasks); `bun run --cwd frontend typecheck && bun run --cwd frontend test:frontend` (frontend tasks).

## Review Focus

1. **Unsaved edits plus a reload or close of the browser tab:** the browser's own "leave site?" prompt must still fire when any workspace tab is dirty. → Task 5, test `beforeunload is armed while any tab is dirty`.
2. **Corrupt or foreign `localStorage`** (malformed JSON, an old shape, storage throwing in private mode, two windows writing): load falls back to the default tab set without crashing. → Task 3, tests `load ignores malformed JSON` and `load survives throwing storage`.
3. **Session expiry while tabs are open:** a tab's fetch redirects to login. The whole app shows the auth screen, and the login URL is never saved as a tab. → Task 5, test `auth page is never persisted as a tab`.
4. **Brand-new org or empty range** (no ledger rows, All time with no first entry): the chart shows an empty state, never NaN, a zero-height axis or a divide-by-zero % change. → Task 8 `TestRevenueEmpty`, Task 9 `scale handles all-zero series`.
5. **Refunds exceeding gross in a bucket** (negative net): bars draw below a visible zero line and the y-axis includes the negative range. → Task 9, test `scale includes negative net`.

---

## File map

| File | Responsibility |
|---|---|
| `frontend/styles.css` | Light/dark tokens (Stripe palette, status pills, sidebar) |
| `frontend/dashboard/src/app-shell.css`, `workspace.css` | Shell, tab strip, tables, pills, chart styles |
| `web/static/css/dash.css` | Classic/no-JS view gets the same palette (dashboard-only file) |
| `frontend/dashboard/src/tokens.test.ts` | Contrast and "customer pages untouched" guards |
| `frontend/dashboard/src/tabs.ts` (+`.test.ts`) | Pure tab model: open/close/pin/reorder/evict, persistence |
| `frontend/dashboard/src/keys.ts` (+`.test.ts`) | Key-sequence matcher shared by tabs, table nav and the command bar |
| `frontend/dashboard/src/shell.tsx` | Persistent shell (header, sidebar, tab strip host); replaces `Workspace` in `main.tsx` |
| `frontend/dashboard/src/tab-view.tsx` | One tab: its query, `RuntimeProvider`, hidden/visible, scroll memory |
| `frontend/dashboard/src/tab-strip.tsx` | Strip UI, context menu, drag, phone sheet |
| `frontend/dashboard/src/revenue.ts` (+`.test.ts`) | Parse the revenue table node, scales, compact money format |
| `frontend/dashboard/src/revenue-chart.tsx` | Lazy SVG chart |
| `frontend/dashboard/src/command.ts` (+`.test.ts`), `command-bar.tsx` | Intent parsing/ranking; lazy dialog |
| `frontend/dashboard/src/saved-filters.ts` (+`.test.ts`) | Saved-filter storage |
| `frontend/dashboard/src/table-nav.ts` (+`.test.ts`) | Row focus movement |
| `internal/migrations/{sqlite,postgres}/0028_ledger_time_index.sql` | Index for time-range queries |
| `internal/store/dashboard.go` | `LedgerSeries`, `FirstLedgerAt` |
| `internal/store/taxdash.go` | `ListFXRatesFor`, `DocFilter.Q` |
| `internal/invoice/fx.go` | Export `ToINR` |
| `internal/dashboard/revenue.go` (+`_test.go`) | Range parsing, bucketing, FX conversion, view model, CSV |
| `web/templates/dash_overview.html` | Range controls + `#revenue-data` table |
| `frontend/playwright.dashboard.config.ts`, `frontend/dashboard/e2e/*.spec.ts` | Dashboard browser flows |

---

### Task 1: Visual tokens and font

**Files:**
- Modify: `frontend/styles.css` (`:root`, `.dark`), `frontend/dashboard/src/app-shell.css`, `frontend/dashboard/src/workspace.css`, `web/static/css/dash.css` (the `--color-*` blocks at ~L463 and ~L860), `frontend/package.json`
- Create: `frontend/dashboard/src/tokens.test.ts`
- Modify: `frontend/vitest.config.ts` (add `"dashboard/src/**/*.test.tsx"` to `include` for later tasks)

**Interfaces:**
- Produces CSS custom properties used by every later task: `--background`, `--foreground`, `--muted-foreground`, `--border`, `--primary`, `--ring`, `--ui-accent`, `--accent-foreground`, `--sidebar`, `--status-ok-bg/fg`, `--status-warn-bg/fg`, `--status-bad-bg/fg`, `--status-neutral-bg/fg`, `--test-mode` (top border colour), `--radius` = `0.625rem` (cards), controls `8px`.

- [ ] **Step 1: Record the bundle baseline.** Run `make build && gzip -c web/static/dashboard/main.js | wc -c`. Write the number into the commit message of Step 6 as `bundle-baseline: N`.
- [ ] **Step 2: Write the failing test** `tokens.test.ts`. Read `frontend/styles.css` with `node:fs` and parse the `:root` and `.dark` blocks into maps. The test needs a small `contrast(hexA, hexB)` helper (WCAG relative luminance).
  - `test("light tokens match spec")`: `--background` = `#ffffff`, `--sidebar` = `#f7f8fa`, `--foreground` = `#1a1f36`, `--muted-foreground` = `#5c6478`, `--border` = `#e3e8ee`, `--primary` = `#635bff`, `--ui-accent` = `#ebeafe`, `--accent-foreground` = `#4b44d6`.
  - `test("dark tokens match spec")`: `#0f1117`, `#14161d`, `#e6e8ef`, `#9aa1b2`, `#262a35`, `#8b85ff`, `#24224a`, `#c4c1ff`, in the same order.
  - `test("text pairs reach 4.5:1 in both modes")`: check foreground/background, muted-foreground/background, muted-foreground/sidebar, accent-foreground/ui-accent, and each `--status-*-fg`/`--status-*-bg`.
  - `test("customer pages keep their palette")`: `web/static/css/app.css` and `web/themes/default/theme.json` do not contain `635bff` (case-insensitive).
- [ ] **Step 3:** Run `bun run --cwd frontend test:frontend -- tokens`. Expected: FAIL on the token values.
- [ ] **Step 4: Implement the tokens.**
  - Light status pills: ok `#d7f7e6`/`#0b6b47`, warn `#fdf0d5`/`#8a5a00`, bad `#fde2e1`/`#a4261d`, neutral `#eef1f5`/`#4f566b`. Derive dark ones that pass the test.
  - `--test-mode: #f59e0b`.
  - Add `@fontsource-variable/inter` (`bun add --cwd frontend @fontsource-variable/inter`) and import it once in `main.tsx`. Set `font-family: "Inter Variable", system-ui, sans-serif` and `font-feature-settings: "tnum"` on `td`, `.num` and money cells.
  - Mirror the light/dark palette into the two `dash.css` blocks. Map `--color-accent` → `#635bff` there; that file is dashboard-only.
  - Restyle the shell CSS per spec §8: sidebar background `--sidebar` with no card border; tables get a small muted uppercase header, 1px dividers, a hover tint and right-aligned `.num`; fewer bordered boxes.
- [ ] **Step 5:** Run the tests and `bun run --cwd frontend typecheck`. Expected: PASS.
- [ ] **Step 6:** Commit `feat(dashboard): Stripe-style tokens, Inter, status pills (bundle-baseline: N)`.

### Task 2: Header: theme toggle, live/test badge, test-mode border, status pills

**Files:**
- Modify: `frontend/dashboard/src/main.tsx` (header in `Workspace`), `frontend/dashboard/src/render.tsx:231` (status span branch), `app-shell.css`
- Create: `frontend/dashboard/src/pill.ts` + `pill.test.ts`

**Interfaces:**
- Produces: `statusTone(text: string): "ok" | "warn" | "bad" | "neutral"` in `pill.ts`.
- Produces: `data-mode="live|test"` on the `.app-shell` element.

- [ ] **Step 1: Failing test** `pill.test.ts`:
  - `"ok"` for `succeeded`, `paid`, `active`, `completed`, `done`, `registered`.
  - `"bad"` for `failed`, `refunded`, `disputed`, `lost`, `canceled`, `cancelled`.
  - `"warn"` for `pending`, `processing`, `past_due`, `requires_action`, `needs_response`.
  - `"neutral"` for anything else.
  - Matching is case-insensitive and ignores surrounding whitespace.
- [ ] **Step 2:** Run it. Expected: FAIL (module missing).
- [ ] **Step 3: Implement `statusTone`.**
  - In `render.tsx`, the status/badge span renders a pill with class `pill pill-${tone}`.
  - Header gains a sun/moon toggle (`aria-label="Switch to dark theme"` / `"Switch to light theme"`) that writes `paymux-theme`. Reuse the existing `dark` state.
  - Header also gains a permanent `LIVE`/`TEST` badge taken from `page.mode`.
  - The shell element gets a `data-mode` attribute. `.app-shell[data-mode="test"]` draws a 3px top border in `var(--test-mode)`.
- [ ] **Step 4:** Run tests and typecheck. Expected: PASS.
- [ ] **Step 5:** Commit `feat(dashboard): theme toggle, mode badge, test-mode border, status pills`.

### Task 3: Tab model (pure)

**Files:** Create `frontend/dashboard/src/tabs.ts`, `tabs.test.ts`

**Interfaces (produced, used by Tasks 5, 6, 11):**
```ts
export type Tab = { id: string; href: string; title: string; pinned: boolean; lastActive: number };
export type TabState = { tabs: Tab[]; activeId: string; closed: Tab[] /* most recent first, max 10 */ };
export const MAX_UNPINNED = 10;
export function tabKey(href: string): string;           // pathname + identifying query, sorted; drops "classic", hash
export function initialState(now: number): TabState;   // one pinned Overview tab, href "/dashboard/overview"
export function open(s: TabState, href: string, title: string, how: "replace" | "new", now: number, dirtyIds: ReadonlySet<string>): TabState;
export function activate(s: TabState, id: string, now: number): TabState;
export function close(s: TabState, id: string): TabState;  // pushes onto closed
export function closeOthers(s: TabState, id: string): TabState; // keeps pinned
export function closeRight(s: TabState, id: string): TabState;
export function reopen(s: TabState, now: number): TabState;
export function duplicate(s: TabState, id: string, now: number): TabState; // the only way to get two tabs with one key; inserted right of id, activated
export function setPinned(s: TabState, id: string, pinned: boolean): TabState; // pinned move to the left
export function move(s: TabState, id: string, toIndex: number): TabState;
export function rename(s: TabState, id: string, title: string): TabState;
export function navigate(s: TabState, id: string, href: string, title: string): TabState; // in-tab navigation
export function storageKey(user: string, mode: string): string; // `paymux-tabs:${user}:${mode}`
export function load(storage: Pick<Storage, "getItem"> | null, key: string, now: number): TabState;
export function save(storage: Pick<Storage, "setItem"> | null, key: string, s: TabState): void;
```

- [ ] **Step 1: Failing tests:**
  - `open focuses an existing tab with the same key`
  - `replace swaps the active unpinned tab`
  - `replace on a pinned active tab opens a new tab`
  - `new appends after the active tab`
  - `11th unpinned tab evicts the least recently active clean tab`
  - `eviction skips dirty and pinned tabs; all dirty -> tab count exceeds cap` (returns an 11-tab state)
  - `close of the active tab activates its right neighbour, else left`
  - `closing the last tab yields initialState`
  - `reopen restores the most recent closed tab`
  - `duplicate creates a second tab with the same href to the right`
  - `pinned tabs stay left of unpinned after move`
  - `tabKey ignores classic, hash and param order`
  - `load ignores malformed JSON`
  - `load survives throwing storage`
  - `load drops tabs whose href is not under /dashboard`
  - `save writes only id, href, title, pinned`
- [ ] **Step 2:** Run `bun run --cwd frontend test:frontend -- tabs`. Expected: FAIL.
- [ ] **Step 3: Implement.** Every function is pure and returns a new state. `load` validates the shape and falls back to `initialState`.
- [ ] **Step 4:** Run tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(dashboard): workspace tab model`.

### Task 4: Key-sequence matcher

**Files:** Create `frontend/dashboard/src/keys.ts`, `keys.test.ts`

**Interfaces (produced):**
```ts
export type Binding = { keys: string[]; run: () => void };  // keys use KeyboardEvent.code, e.g. ["KeyG","Digit1"], ["BracketLeft"]
export function isTyping(target: EventTarget | null): boolean;
export function createMatcher(bindings: Binding[], timeoutMs?: number /* 800 */): (e: KeyboardEvent, now: number) => boolean; // true if handled
```
The command-bar binding is a separate check: `(e.metaKey || e.ctrlKey) && e.code === "KeyK"` (always active), or `Slash` when not typing.

- [ ] **Step 1: Failing tests:**
  - `single key fires`
  - `g then 1 fires within timeout`
  - `g then 1 after timeout does not fire`
  - `ignored while typing in input/textarea/select/contenteditable`
  - `ignores events with ctrl/alt/meta`
  - `matches by code not key` (an event with `key: "ü"`, `code: "BracketLeft"` fires `[`)
- [ ] **Step 2:** Run tests. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(dashboard): key sequence matcher`.

### Task 5: Shell split, mounted hidden tabs, history, one-time secrets

**Files:**
- Create: `frontend/dashboard/src/shell.tsx`, `tab-view.tsx`, `tab-host.test.tsx`
- Modify: `frontend/dashboard/src/main.tsx` (`DashboardApp` and `Workspace` move into `shell.tsx` / `tab-view.tsx`; keep `Boundary`, router, query client)
- Modify: `frontend/dashboard/src/runtime.tsx` (dirty state reported per tab)
- Modify: `frontend/dashboard/src/adapter.ts` (`DashboardPage.once: boolean`)
- Modify templates that show one-time secrets: `web/templates/dash_app.html` (the new-key block ~L13), `dash_setup_done.html`, `dash_subscription.html:12`. Also grep `web/templates` for "once" / "cannot be retrieved" and mark any other matches.
- Test (Go): `internal/dashboard/keys_test.go` (assert the attribute is present on the key-reveal response)

**Interfaces:**
- Consumes: `tabs.ts` (Task 3).
- Produces in `shell.tsx`:
  ```ts
  type TabsApi = {
    state: TabState;
    openHref(href: string, how: "replace" | "new"): void;
    activate(id: string): void;
    close(id: string): void;
    reopen(): void;
    pin(id: string, v: boolean): void;
    move(id: string, i: number): void;
    dirtyIds: ReadonlySet<string>;
  };
  export function useTabs(): TabsApi;
  ```
- `RuntimeProvider` gets a new prop `onDirtyChange(dirty: boolean)`.
- `adapter.ts`: `once` is true when the page content contains `[data-once]`.

**Behaviour to implement (spec §4):**
- **Router URL = active tab URL.** In-tab navigation (link click, form redirect) uses `navigate`, which pushes. Switching tabs uses `router.navigate({replace: true})`. On `popstate`, a URL already open in a tab activates that tab; otherwise it loads into the active tab, or a new one if the active tab is pinned.
- **Each `TabView`** runs `useQuery({queryKey: queryKey(href), enabled: isActive})`. Inactive views stay mounted under a `hidden` wrapper. Keep `gcTime: 0`.
- **On activation**, refetch if `Date.now() - dataUpdatedAt > 30_000` and the tab is not dirty.
- **Scroll position** is saved per tab on hide and restored on show.
- **`page.once`:** the tab unmounts when hidden. On return it re-fetches with the same URL, so the secret is gone because the server shows it only once.
- **Memory bound:** a hidden clean tab unmounts after 10 minutes without being visited. Use a single `setTimeout` per hidden tab and clear it on show; never `setInterval`.
- **Auth pages:**
  - `page.auth`, or a redirect to a login/2FA/reauth URL, renders the auth layout in place of the shell.
  - Tabs are not saved while the app is on an auth page, and the auth URL never becomes a tab.
  - After login, saved tabs are reloaded from storage.
- **Dirty state:** if any tab is dirty, a `beforeunload` listener is installed. Closing a dirty tab shows the existing review dialog.
- **Logout or mode switch** clears the mounted views. Tabs reload from that user+mode's storage key.
- **Shell-level `useEffect`s** (title, theme link, `data-ready`) read from the active tab's page.

- [ ] **Step 1: Failing Go test** `TestKeyRevealMarkedOnce`. The response containing a new API key has `data-once` on the reveal block.
- [ ] **Step 2:** Run `go test ./internal/dashboard -run TestKeyRevealMarkedOnce`. Expected: FAIL.
- [ ] **Step 3:** Add `data-once` to the templates listed above.
- [ ] **Step 4:** Run the Go test again. Expected: PASS. Commit `feat(dashboard): mark one-time secret views`.
- [ ] **Step 5: Failing frontend tests** `tab-host.test.tsx`. Render the shell in happy-dom with a stubbed `fetch` that returns fixture HTML (reuse the `adapter.test.ts` fixture style).
  - `opening a second page keeps the first mounted but hidden`
  - `hidden tab does not fetch`
  - `returning after 31s refetches, before 30s does not`
  - `once page unmounts when hidden`
  - `beforeunload is armed while any tab is dirty`
  - `auth page is never persisted as a tab`
  - `popstate to an open tab activates it`
  - `switching tabs replaces history; in-tab navigation pushes` (spy on `history.pushState`/`replaceState`)
- [ ] **Step 6:** Run tests. Expected: FAIL.
- [ ] **Step 7:** Implement as described above.
- [ ] **Step 8:** Run `bun run --cwd frontend typecheck && bun run --cwd frontend test:frontend`. Expected: PASS. Then `go test ./internal/dashboard` (the `frontend_test.go` contract).
- [ ] **Step 9:** Commit `feat(dashboard): persistent shell with mounted workspace tabs`.

### Task 6: Tab strip UI

**Files:**
- Create: `frontend/dashboard/src/tab-strip.tsx`, `tab-strip.test.tsx`
- Modify: `shell.tsx`, `app-shell.css`, `runtime.tsx` (`WorkspaceLink`: Alt-click → `openHref(href,"new")`; plain click → `"replace"`; Ctrl/Meta/middle-click left to the browser)

**Interfaces:** Consumes `useTabs()` (Task 5) and `createMatcher` (Task 4).

- [ ] **Step 1: Failing tests:**
  - `renders pinned tabs icon-only with aria-label = title`
  - `context menu offers Close, Close others, Close to the right, Pin/Unpin, Duplicate, Copy link, Reload`
  - `[ and ] move between tabs; g 1 activates first; g w closes; g t reopens`
  - `alt-click on a workspace link opens a new tab, plain click replaces`
  - `ctrl-click is not intercepted` (no `preventDefault`)
  - `below 640px the strip renders an "N open" button that opens a sheet listing tabs`
  - `drag reorders` (fire pointer events; assert `move` was called)
- [ ] **Step 2:** Run tests. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - Radix `DropdownMenu` for the context menu.
  - Native pointer events for drag; no DnD library.
  - Strip height 34px; the active tab is white with an accent underline.
  - Mount `createMatcher` on `window` once, in `shell.tsx`.
- [ ] **Step 4:** Run tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(dashboard): tab strip, context menu, shortcuts, phone sheet`.

### Task 7: Ledger time index and series query

**Files:**
- Create: `internal/migrations/sqlite/0028_ledger_time_index.sql`, `internal/migrations/postgres/0028_ledger_time_index.sql`
- Modify: `internal/store/dashboard.go`
- Test: `internal/store/dashboard_test.go`, `internal/store/ledger_series_bench_test.go`

**Interfaces (produced):**
```go
type SeriesFilter struct {
	OrgID, Mode, AppID string // AppID "" = all apps
	From, To           time.Time // [From, To)
	Hourly             bool      // false = daily slots
	OffsetSeconds      int       // Location offset at From; slots are local days/hours
}
type SeriesRow struct {
	Slot     int64 // floor((occurred_at_ms + OffsetSeconds*1000) / unit_ms), unit = 3_600_000 or 86_400_000
	Currency string
	Gross, Refunds int64 // Refunds positive, as in LedgerTotal
}
func LedgerSeries(ctx context.Context, q Queryer, f SeriesFilter) ([]SeriesRow, error) // ordered by Slot, Currency
func FirstLedgerAt(ctx context.Context, q Queryer, orgID, mode string) (time.Time, bool, error)
```
The index is `CREATE INDEX ledger_entries_org_mode_time ON ledger_entries (org_id, mode, occurred_at)` (same SQL on both databases). Use the same ledger rules as `LedgerTotals`: `kind IN ('charge','refund')`, join `payments` for `app_id` and the `p.org_id` check.

- [ ] **Step 1: Failing tests** (pattern: `TestLedgerTotals`).
  - `TestLedgerSeriesDaily`:
    - Entries at t0, t0+1h, t0+25h and a refund at t0+2h, with `OffsetSeconds` 19800.
    - Expect two day slots with the right gross/refunds.
    - Fee rows, other modes, other orgs and entries outside [From,To) are excluded.
  - `TestLedgerSeriesHourlyAndApp`: hourly slots, filtered to one app.
  - `TestFirstLedgerAt`: the earliest entry of this org/mode; `false` when there is none.
- [ ] **Step 2:** Run `go test ./internal/store -run 'LedgerSeries|FirstLedgerAt'`. Expected: FAIL.
- [ ] **Step 3:** Implement the migrations and functions. The slot expression is integer arithmetic on the BIGINT ms column and works the same on both databases.
- [ ] **Step 4:** Run tests (SQLite, plus Postgres via `PAYMUX_TEST_POSTGRES_URL`). Expected: PASS.
- [ ] **Step 5: Benchmark** `BenchmarkLedgerSeriesAllTime`.
  - Seed 1,000,000 entries over 5 years on SQLite, then run all-time daily.
  - Run `go test ./internal/store -run x -bench LedgerSeriesAllTime -benchtime 5x`.
  - Pass: < 300 ms/op. If it fails, stop and report the number with a daily-rollup proposal (spec §6). Don't build the rollup.
- [ ] **Step 6:** Commit `feat(store): ledger time index, LedgerSeries, FirstLedgerAt` (include the ns/op in the message).

### Task 8: Revenue view model, FX conversion, template, CSV

**Files:**
- Create: `internal/dashboard/revenue.go`, `internal/dashboard/revenue_test.go`
- Modify: `internal/invoice/fx.go` (rename `toINR` → exported `ToINR`, update its callers), `internal/store/taxdash.go` (add `ListFXRatesFor`), `internal/dashboard/audit_overview.go` (`overviewPage` builds `v.Revenue`; register `GET /dashboard/overview/revenue.csv` in `routesAuditOverview`), `web/templates/dash_overview.html`

**Interfaces:**
- Consumes: `store.LedgerSeries`, `store.FirstLedgerAt` (Task 7); `invoice.ToINR(minor int64, currency, rate string) (int64, error)`.
- Produces:
```go
func ListFXRatesFor(ctx context.Context, q Queryer, orgID, currency string) ([]FXRate, error) // ascending by Date

type revenueQuery struct {
	Range    string // today|7d|30d|3m|6m|12m|ytd|all|custom; default 30d
	Bucket   string // auto|hour|day|week|month|quarter; default auto
	App      string
	Currency string // "" = default rule (spec §6), else an ISO code
	From, To time.Time // resolved, local midnight boundaries
}
func parseRevenueQuery(v url.Values, now time.Time, loc *time.Location, first time.Time, hasFirst bool) (revenueQuery, error)
func autoBucket(rangeID string, from, to time.Time) string // per the table in spec §6; all > 36 months -> quarter
func bucketStart(t time.Time, bucket string, loc *time.Location) time.Time // week starts Monday; quarters Jan/Apr/Jul/Oct

type revenuePoint struct{ Start time.Time; Label string; Gross, Refunds, PrevGross, PrevRefunds int64 }
type revenueView struct {
	Query       revenueQuery
	Currency    string // currency of the amounts shown
	Converted   bool   // INR conversion applied
	Estimated   bool   // some conversion used a non-same-day rate
	Excluded    []string // pre-formatted "1,240.00 USD" with no rate at all
	Points      []revenuePoint
	Gross, Refunds, Net int64
	GrossChange, NetChange, RefundsChange string // "+12.4%", "−3.0%", or "" when the previous period is 0
	Currencies  []string // for the picker
	Presets     []revenuePreset // {ID, Label, Href, Current}
}
func (d *Dashboard) revenue(ctx context.Context, q store.Querier, org, mode string, rq revenueQuery) (revenueView, error)
```

**Rules:**
- **Ranges and buckets:** see spec §6.
  - The previous period is the same length, ending at `From`.
  - `custom` requires `from` ≤ `to`, both `YYYY-MM-DD`, with `to` inclusive; a bad value is a 400 via `badRequest`.
  - `all` with no ledger rows gives an empty view, not an error.
- **FX** (only when INR is the shown currency and the range has non-INR money):
  - The rate for each day slot is that IST date's rate; else the latest earlier rate; else the earliest later rate. The last two set `Estimated`.
  - A currency with no rates at all is left out and listed in `Excluded`.
  - Hourly ranges use the rate for the range's date.
- **Tax collected series is out of v1** (spec §6 allows this). It would need a second, invoice-based query with its own FX path. Note it as a follow-up in the README (Task 15).
- **Default currency:** INR converted, unless INR is < 50% of the range's gross in native-minor terms after conversion. In that case use the largest currency by converted gross, unconverted.
- **Template:**
  - A `<nav class="range-presets">` of links (`aria-current="page"` on the current one), a GET form for custom/app/currency/bucket, and a summary `<dl class="revenue-summary">`.
  - A `<table id="revenue-data" class="grid" data-currency data-exp data-bucket data-converted data-estimated>`, one `<tr data-start data-gross data-refunds data-prev-gross data-prev-refunds>` per point (minor units), with visible cells Label/Gross/Refunds/Net formatted by `fmtMoney`.
  - An excluded-currency note linking `/dashboard/fx`.
  - A CSV link carrying the current query.
  - The existing period tables stay below.
- **CSV:** `GET /dashboard/overview/revenue.csv` takes the same query and the same permission as overview. Columns: `start,label,currency,gross,refunds,net` in major units. Filename `revenue-<range>-<currency>.csv`. Add the path to `isDownloadPath` in `adapter.ts` if it isn't matched already.

- [ ] **Step 1: Failing tests** (pattern: `TestOverviewTotalsFromLedger`):
  - `TestParseRevenueQuery`: every preset's From/To in `Asia/Kolkata`, custom validation, all-time from first entry.
  - `TestAutoBucket`: every row of the spec table, plus the >36-month quarter case.
  - `TestBucketStartWeekMondayQuarter`
  - `TestRevenueINRConversionAndEstimates`:
    - USD at a same-day rate, then a later day with no rate → uses the earlier rate and sets `Estimated`.
    - An EUR amount with no rate at all → in `Excluded`, not in the total.
  - `TestRevenueDefaultCurrencyMajority`: mostly-USD org → default view is USD, unconverted.
  - `TestRevenuePreviousPeriodChange`: `+50.0%`; previous 0 → `""`.
  - `TestRevenueEmpty`: new org, `range=all` → 200, zero points, no NaN text.
  - `TestOverviewRendersRevenueTable`: page contains `id="revenue-data"`, preset links, `data-gross` values.
  - `TestRevenueCSV`: header row + values; viewer role allowed; other org's ledger absent.
  - `TestListFXRatesFor` in `internal/store/taxdash_test.go`.
- [ ] **Step 2:** Run `go test ./internal/dashboard ./internal/store ./internal/invoice -run 'Revenue|AutoBucket|BucketStart|ListFXRatesFor'`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** Run `go test ./...`. Expected: PASS. The existing overview tests still pass.
- [ ] **Step 5:** Commit `feat(dashboard): revenue series with ranges, buckets, INR conversion, CSV`.

### Task 9: Revenue chart (frontend)

**Files:**
- Create: `frontend/dashboard/src/revenue.ts`, `revenue.test.ts`, `revenue-chart.tsx`
- Modify: `render.tsx` (`table#revenue-data` → lazy `<RevenueChart node={n} />` above a "View as table" toggle that reveals the original table; `nav.range-presets` → segmented control styling), `workspace.css`

**Interfaces:**
```ts
export type Point = { start: string; gross: number; refunds: number; net: number; prevNet: number };
export type Series = { currency: string; exp: number; bucket: string; converted: boolean; estimated: boolean; points: Point[] };
export function parseRevenue(node: ViewNode): Series | null;
export function scale(values: number[], height: number): { min: number; max: number; y: (v: number) => number; ticks: number[] }; // includes 0, nice ticks
export function compactMoney(minor: number, currency: string, exp: number): string;
```

- [ ] **Step 1: Failing tests** `revenue.test.ts`:
  - `parseRevenue reads data attributes`
  - `parseRevenue returns null without rows`
  - `scale handles all-zero series` (no NaN, max > min)
  - `scale includes negative net` (min < 0, 0 in ticks)
  - `compactMoney`, INR:
    - `95000, "INR", 2` → `₹950`
    - `4823100` → `₹48.2K`
    - `48231000` → `₹4.8 L`
    - `1200000000` → `₹1.2 Cr`
  - `compactMoney`, other currencies:
    - `123456789, "USD", 2` → `$1.2M`
    - `950, "JPY", 0` → `¥950`
- [ ] **Step 2:** Run tests. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - The chart is hand-written SVG:
    - Net bars, gross as a light bar behind them, refunds as a thin red segment.
    - A dashed previous-period net line.
    - Zero line, y ticks from `scale`, x labels thinned to ≤ 8.
  - Hover and focus (each bar is a `<g tabindex=0 role="img" aria-label>`) show a tooltip with exact values formatted with `Intl.NumberFormat`. ←/→ move focus between bars.
  - Show "Includes estimated conversions" when `estimated` is true.
  - No animation.
  - `React.lazy` + `Suspense` with a skeleton the size of the chart (no layout shift).
- [ ] **Step 4:** Run tests, typecheck and `make build`. Expected: PASS. The chart is a separate chunk (check `ls web/static/dashboard`).
- [ ] **Step 5:** Commit `feat(dashboard): revenue chart`.

### Task 10: Invoice search filter

**Files:** Modify `internal/store/taxdash.go` (`DocFilter.Q string`; `where` adds `(number = ? OR number LIKE ? OR recipient LIKE ?)` with escaped `%q%`), `internal/dashboard/invoices.go:111` (read `q`), `web/templates/dash_invoices.html` (search input `name="q"`, placeholder "Invoice number or buyer"). Tests: `internal/store/taxdash_test.go`, `internal/dashboard/invoices_test.go`.

- [ ] **Step 1: Failing tests:**
  - `TestSearchInvoicesQ`: exact number, prefix, buyer substring, a LIKE wildcard in input treated literally.
  - `TestInvoicesPageQFilter`
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement. Escape `%`, `_` and `\` and use `ESCAPE '\'` (works on both databases).
- [ ] **Step 4:** Run `go test ./...`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(dashboard): search invoices by number or buyer`.

### Task 11: Command bar

**Files:** Create `frontend/dashboard/src/command.ts`, `command.test.ts`, `command-bar.tsx`. Modify `shell.tsx` (⌘K/Ctrl+K, `/`, a header search button that opens it).

**Interfaces:**
```ts
export type Item = { id: string; label: string; hint?: string; href?: string; action?: "toggle-theme" | "switch-mode" | "reopen-tab" };
export function intents(query: string): Item[]; // ID patterns -> direct record hrefs; emails -> payments/subscriptions ?q=; anything else -> payments ?q= and invoices ?q=
export function rank(items: Item[], query: string): Item[]; // case-insensitive subsequence score, label match beats hint
export function recent(storage: Pick<Storage,"getItem">|null, user: string): Item[]; // max 8
export function remember(storage: Pick<Storage,"getItem"|"setItem">|null, user: string, item: Item): void;
```
Sources: `page.nav` (pages), open tabs (`useTabs`), actions ("Create payment link" → `/dashboard/links`, "Switch to live/test" submits `page.modeForm`, "Toggle theme", "Reopen closed tab"), and `intents(query)`.

- [ ] **Step 1: Failing tests:**
  - `pay_ id goes to /dashboard/payments/<id>`
  - `sub_ id goes to /dashboard/subscriptions/<id>`
  - `email searches payments and subscriptions`
  - `free text searches payments and invoices`
  - `rank prefers label prefix`
  - `recent caps at 8 and dedupes`
  - `recent survives throwing storage`

  Before writing these, check the record URL shapes in `internal/dashboard/payments.go` and `subscriptions.go` routes.
- [ ] **Step 2:** Run tests. Expected: FAIL.
- [ ] **Step 3: Implement** with the Radix `Dialog`: `role="combobox"`, `aria-activedescendant`, ↑/↓/Enter. Alt+Enter opens in a new tab. Lazy-load the bar on first open.
- [ ] **Step 4:** Run tests and the build. Expected: PASS.
- [ ] **Step 5:** Commit `feat(dashboard): command bar`.

### Task 12: Saved filters

**Files:** Create `frontend/dashboard/src/saved-filters.ts`, `saved-filters.test.ts`. Modify `render.tsx` (above any `form.filters` with `method="get"`: chips + "Save filter" button).

**Interfaces:**
```ts
export type SavedFilter = { name: string; query: string };
export function list(storage: Pick<Storage,"getItem">|null, user: string, path: string): SavedFilter[];
export function add(storage: Pick<Storage,"getItem"|"setItem">|null, user: string, path: string, f: SavedFilter): void; // same name replaces; max 12 per path
export function remove(storage: Pick<Storage,"getItem"|"setItem">|null, user: string, path: string, name: string): void;
```
Key: `paymux-filters:${user}:${pathname}`. The query string drops `csrf` and empty values.

- [ ] **Step 1: Failing tests:**
  - `add then list`
  - `same name replaces`
  - `cap 12`
  - `query drops csrf and empties`
  - `throwing storage returns []`
- [ ] **Step 2:** Run tests. Expected: FAIL.
- [ ] **Step 3:** Implement. A chip click navigates the current tab to `path?query`.
- [ ] **Step 4:** Run tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(dashboard): saved filters`.

### Task 13: Table keyboard navigation, copy buttons, shortcut sheet

**Files:** Create `frontend/dashboard/src/table-nav.ts`, `table-nav.test.ts`. Modify `render.tsx` (tables whose rows contain a link get `data-nav`; `<code>` inside `td`/`dd` gets a copy button via `useRuntime().copy`, `aria-label="Copy <value>"`, with a "Copied" status toast), `shell.tsx` (`?` opens a shortcut sheet dialog listing every binding from Tasks 4/6/11).

**Interfaces:** `export function nextRow(current: number, count: number, key: "down" | "up" | "first" | "last"): number`. j/↓, k/↑, Home, End; Enter opens the row's first link (replace); Alt+Enter opens a new tab.

- [ ] **Step 1: Failing tests:**
  - `nextRow clamps at ends`
  - `empty table returns -1`
  - In `tab-host.test.tsx`: `copy button copies code text`.
- [ ] **Step 2:** Run tests. Expected: FAIL.
- [ ] **Step 3:** Implement. The focused row gets a visible outline (`:focus-visible` with `--ring`).
- [ ] **Step 4:** Run tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(dashboard): table keyboard navigation, copy buttons, shortcut sheet`.

### Task 14: Dashboard Playwright harness and flows

**Files:**
- Create: `frontend/playwright.dashboard.config.ts`, `frontend/dashboard/e2e/global-setup.ts`, `frontend/dashboard/e2e/workspace.spec.ts`
- Modify: `frontend/package.json` (`"test:dashboard": "playwright test --config playwright.dashboard.config.ts"`), `scripts/ci.sh` (run it after `test:home`)

**Setup:**
- `make build`, then start `bin/paymux` against a temp data dir with `PAYMUX_DEV_SKIP_2FA=true` and a free port.
- Seed an owner, an app, payments and ledger rows across 14 months in two currencies, plus one FX rate. Use the existing CLI where it can; otherwise add a test-only Go helper under `internal/devseed`, never reachable from a production flag.
- Mirror `playwright.home.config.ts` for the `webServer` and browser settings.

**Flows:**
- `login, overview shows chart, switch to 12M shows monthly bars, CSV downloads`
- `open payment in new tab with alt-click, switch tabs keeps filter, reload restores tabs`
- `dirty form in hidden tab survives switching and blocks close`
- `ctrl+k finds a payment id and opens it`
- `test mode shows the top border and TEST badge`
- `phone width shows the open-tabs sheet`
- `no request fires from a hidden tab for 60s` (count network requests via `page.on('request')` with clock advance)

- [ ] **Step 1:** Write the spec and config.
- [ ] **Step 2:** Run `bun run --cwd frontend test:dashboard`. Expected: the flows exercise the finished features and pass. Fix any defects found in the owning task's files, with their own commits.
- [ ] **Step 3:** Commit `test(dashboard): browser flows for tabs, chart, command bar`.

### Task 15: Budget check and docs

**Files:** Modify `frontend/dashboard/README.md` (the "Compact application shell" section: tabs, shortcuts, chart, command bar, data-retention rule for hidden tabs and `data-once`), `docs/` user guide page for the dashboard if one exists (find it with `grep -rl "Your sessions" docs`).

- [ ] **Step 1:** Run `make build && gzip -c web/static/dashboard/main.js | wc -c`. Sum the gzip sizes of all dashboard chunks. Pass: initial `main.js` growth ≤ 25 KB versus `bundle-baseline`. If it's over, identify the cause in the commit message and fix it before continuing.
- [ ] **Step 2: Idle CPU check.** Use Chrome DevTools Performance, or Playwright `page.evaluate` with `PerformanceObserver` long tasks. Leave Overview plus 4 hidden tabs open for 60s: no long tasks and no network requests.
- [ ] **Step 3:** Update the docs; run `bun run --cwd frontend check:docs`. Expected: PASS.
- [ ] **Step 4:** Commit `docs(dashboard): workspace tabs, revenue chart, command bar`.
