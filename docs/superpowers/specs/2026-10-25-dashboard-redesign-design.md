# Dashboard redesign: Stripe-style app shell, workspace tabs, revenue chart

*Status: draft for review · Date: 2026-10-25 · Scope: the React dashboard (`frontend/dashboard`) and the Go views it renders.*

## 1. Intent and why

The dashboard looks like a government portal. Both UIs share one palette: pale blue-grey page (`#f6f8fb`), plain white cards, one stock blue (`#2256d9`), system fonts and thin grey borders. Paymux is a merchant of record and payment engine. It should look and behave like the dashboards its users already know from that world (Stripe, Paddle, Lemon Squeezy): a calm, dense, app-like workspace where you keep several things open at once and see revenue at a glance.

The user picked from mockups (kept in `.superpowers/brainstorm/`):

- **Look: "A · Stripe-like".** White surfaces, a light grey sidebar, one violet accent, compact rows and tabular figures.
- **Tabs: "1 · Workspace tabs".** Every page or record opens as a tab across the top, like a browser or an IDE.

What the user asked for, in their words: *"app shell, app like, tab based, optimised for browser and cpu, compact, well designed, well featured"*, plus a *"revenue calculation graph: daily, monthly, quarter, 6, 12 months, all time"*. They also accepted all the extra features proposed in §5.

## 2. Goals

1. A new visual identity (Stripe-like, light by default, with a matching dark mode) applied through design tokens, so every existing page changes without per-page rewrites.
2. Workspace tabs (§4) on top of the existing compact shell.
3. A revenue chart on Overview with range presets and auto bucketing (§6).
4. A command bar and the other workspace features (§5).
5. Fast and light: small bundles, near-zero idle CPU, no jank on low-end laptops (§7).

## 3. Non-goals and anti-goals

**Non-goals**
- No change to how pages get their data. The React app keeps rendering the Go HTML views through `adapter.ts` (see `frontend/dashboard/README.md`). No JSON API, no new auth.
- No redesign of the customer-facing pages (`pay_*`, `invoice`, `sim_checkout`) or the marketing site (`frontend/home`). They may pick up shared tokens, but they are not the target.
- No page-by-page information-architecture changes beyond what tabs, command bar and chart need.
- No change to the theme system (`web/themes/`). Existing custom themes must keep working (see §8).

**Anti-goals (failure even if it "works")**
- It still reads as a government or bank portal: heavy borders, boxed-in everything, stock blue.
- It burns CPU while idle: hidden tabs polling, animation loops, or a chart library re-rendering on every mouse move.
- It breaks the no-JavaScript fallback or `?classic=1`.
- It breaks browser habits: links that can't be copied, middle-clicked or bookmarked; the back button doing something surprising; Ctrl+W / Ctrl+T / Ctrl+1–9 hijacked.
- Accessibility regresses: contrast under 4.5:1, focus rings removed, icon-only buttons without names.

## 4. Workspace tabs

**Behaviour (decided with the user, details are my calls unless marked):**

- Opening any dashboard page or record opens it as a tab in a strip under the header. If that URL (path plus the query that identifies the record) is already open, focus that tab instead of opening a duplicate.
- The browser URL always equals the active tab's URL. Copy, bookmark, back/forward and reload work as they do now. Ctrl-click or middle-click on a link opens a real browser tab (normal anchor behaviour; never intercepted).
- A plain click on a sidebar item or in-page link **replaces the current tab** (unless it's pinned). Alt-click or "Open in new tab" in the context menu opens a new workspace tab. *(My call: this matches IDE "preview" tabs and keeps the strip from filling up when people just browse.)*
- Each tab remembers its scroll position, filter values and unsaved form edits while you're in other tabs. Closing a tab with unsaved edits asks first (reuse the existing `useBlocker` / dirty-form tracking in `runtime.tsx`).
- Tabs persist across reloads in `localStorage`, keyed per user and per live/test mode. Only URLs and titles are stored, never page content or form values.
- **Pinned tabs** (pin icon or context menu) sit at the left, show an icon only and are never closed automatically. Overview is pinned by default for a fresh user.
- **Cap: 10 unpinned tabs** *(my call)*. Opening an 11th closes the least-recently-used unpinned tab without unsaved edits; if all have edits, open it anyway and show a small notice.
- **Only the active tab is live.** Hidden tabs stay **mounted but hidden** (their React tree and DOM are kept in memory, so scroll, filters and unsaved edits survive). They do not refetch or poll. When a tab becomes active again it refetches if its data is older than 30 seconds *(my call)*.
- **The existing data-retention rule stays (my call).** The README says page data is never persisted and inactive queries are discarded (`gcTime: 0` in `main.tsx`). Tabs keep that intent:
  - A hidden tab's content lives only in memory, as the current view does today. It is never written to storage.
  - Logout and live/test mode switches unmount every tab.
  - A view showing a **one-time secret** (a newly created API key or similar "shown once" response) is unmounted the moment its tab is hidden. Returning to it shows the page fresh, without the secret, exactly as navigating away does today. The builder marks those responses (e.g. a `data-once` attribute in the template) rather than guessing.
  - The builder may keep `gcTime: 0`. Tabs stay alive through being mounted, not through the query cache.
- **Memory bound:** a hidden tab with **no unsaved edits** that hasn't been visited for 10 minutes is unmounted; its URL stays and it re-fetches on return. A tab with unsaved edits is **never** unmounted or evicted automatically *(my call)*.
- Dirty-form tracking and the "unsaved changes" blocker move from per-route to per-tab. Closing a tab, evicting it, or navigating it away checks only that tab's forms.
- **History (my call, like Linear / VS Code web):**
  - Every navigation inside a tab pushes a browser history entry. Switching tabs replaces the current entry, so it adds no history.
  - On Back/Forward (`popstate`), if the target URL is open in a tab, focus that tab. Otherwise load it into the active tab (or a new one if the active tab is pinned).
  - Back never closes tabs and never discards edits without the usual prompt.
- Context menu on a tab: Close, Close others, Close to the right, Pin/Unpin, Duplicate, Copy link, Reload.
- **Keyboard (my call).** Alt+digits and Ctrl+digits switch browser tabs on Linux/Windows, and Option+letters type characters on macOS, so tab shortcuts use **single keys and sequences that only fire when focus is not in a text field**. Keys are matched with `event.code` so they work on any keyboard layout and on macOS.
  - `[` / `]`: previous / next tab.
  - `g` then `1`…`9`: go to tab N.
  - `g` then `w`: close tab.
  - `g` then `t`: reopen the last closed tab.
  - All tab actions are also in the command bar and in the tab context menu.
  - Drag to reorder.
- Switching live/test mode keeps the tab set separate per mode (consistent with "mode changes clear cached data" in the README).
- **Phone width:** the strip becomes one "N open" button that opens a sheet listing the tabs (reuse `components/ui/sheet.tsx`).
- Auth screens (login, 2FA, reauth, setup) never show tabs.

## 5. Workspace features

1. **Command bar (⌘K / Ctrl+K, also "/" when not typing).** It does three things:
   - Jump to any nav page.
   - Search records: typing an ID or email opens a result that is the existing payments/subscriptions list with `?q=` (those pages already support `q`). Exact ID patterns (`pay_…`, `sub_…`) go straight to the record page.
  - **Invoice-number search needs a small Go change (my call: do it).** Add a `q` filter (invoice number or buyer) to the invoices list (`internal/dashboard/invoices.go`) and its template. This adds to the page but doesn't restructure it, so it's in scope.
   - Run common actions: create payment link, switch live/test, toggle theme, open recent tabs.

   It also remembers recent items. It uses the existing Radix dialog; no new dependency like `cmdk` unless the builder finds it is smaller than hand-rolling it.
2. **Saved filters** on list pages (payments, subscriptions, invoices, disputes, logs, audit). Save the current query string with a name as a chip above the table. Stored in `localStorage` per user *(my call; server-side storage is a later step)*.
3. **Keyboard table navigation.** j/k or ↑/↓ moves the row focus, Enter opens the row (a plain click replaces the current tab, Alt+Enter opens a new tab), and `?` shows a shortcut sheet.
4. **Copy buttons** next to every ID, reference and API key (the runtime already has `copy()`), with a "Copied" toast.
5. **Light/dark toggle** in the header, defaulting to the OS setting (`paymux-theme` already exists in `localStorage`).
6. **Live/test badge** always visible in the header. Test mode also gets a thin coloured top border across the whole shell (Stripe's orange test banner pattern), so nobody refunds in the wrong mode.

## 6. Revenue chart (Overview)

**Placement:** the top of Overview, above the existing per-app tables. The tables stay below as the exact-figures view.

**Range presets:** Today · 7D · 30D · 3M · 6M · 12M · YTD · All time, plus a custom date range. The user asked for daily, monthly, quarter, 6, 12 months and all time; 7D, YTD and custom are my additions.

**Bucketing (automatic, overridable with Day / Week / Month):**

| Range | Bucket |
|---|---|
| Today | hour |
| 7D, 30D | day |
| 3M, 6M | week |
| 12M, YTD | month |
| All time | month (quarter if more than 36 months) |

Buckets follow the IST calendar (the configured `Location`), like everything else in the hub. Weeks start on Monday *(my call)*.

**Series:**
- Gross, refunds and net (gross − refunds), from the same ledger rules as `store.LedgerTotals` (`kind IN ('charge','refund')`).
- **Tax collected**, as a separate toggleable series, if the builder can get it cheaply from invoices. If that would mean a large new query, leave it out of v1 and note it.

**Summary row** above the chart: total gross, net and refunds for the range, each with a % change against the previous period of the same length, plus a faint line for the previous period on the chart.

**Filters:** app (all or one), plus the existing live/test mode. The range, bucket, app and currency live in the URL query so a tab or bookmark restores them.

**Currency (my call; the user didn't choose):**

The stored rates are RBI/FBIL reference rates, entered by hand (FX page or `paymux fx set`) one per currency per day, mainly for invoices. Expect gaps. Rules:

- **Default view: INR converted.** Each foreign-currency bucket amount uses the rate for that IST day.
  - If that day has none, use the most recent earlier rate for that currency.
  - If no earlier rate exists, use the earliest later rate.
  - Any conversion that isn't same-day marks the chart "includes estimated conversions", with a link to the FX rates page.
- **No rate at all for a currency:** leave its amounts out of the INR total. Show a line under the summary row: "Not included: $1,240 (USD) — no exchange rate stored. Add rates." Never show those amounts as if they were rupees.
- **Default when INR is not the main currency:** if INR makes up less than half of the range's gross, open the chart on the largest currency in its own amounts instead.
- **A currency picker** switches to any single currency in its own amounts with no conversion. That view is always exact.

**Interaction:** hovering or focusing a bar shows a tooltip with the exact values. Arrow keys move between bars. An accessible data table with the same numbers sits behind a "View as table" toggle. There's also a **CSV download** of the buckets.

**Data delivery (constraint from the architecture):**
- The Go overview handler gains a time-bucketed query (new `store` function, SQLite and Postgres both supported, as everywhere else).
- It renders the buckets into the overview template as a plain HTML table with machine-readable attributes. The adapter turns that table into the chart, and without JavaScript the table *is* the fallback.
- No JSON endpoint.
- **The chart needs a new index.** Today `ledger_entries` has only `ledger_entries_payment (payment_id)` and no `app_id` column, so the app filter joins `payments`, as `LedgerTotals` does.
  - Add a migration on **both** SQLite and Postgres: index `(org_id, mode, occurred_at)`, including `kind` if it helps.
  - **Target (my call):** any range including All time renders in under 300 ms server time at 1 million ledger rows, measured with a Go benchmark on seeded data.
  - If the index alone misses the target, a daily rollup table is the fallback. Propose it in the plan; don't build it unless needed.

**Rendering:** hand-written SVG in a small React component, with no chart library. Bars for the chosen series, a line for the previous period, and the y-axis in compact Indian notation (₹4.8 L, ₹1.2 Cr) for INR and K/M for other currencies.

## 7. Performance budget

- The dashboard JS bundle must not grow by more than ~25 KB gzipped for everything in this spec. Use the existing `lucide-react` icons and Radix primitives, and add no new runtime dependency without a reason written in the plan.
- Split the code per feature: the command bar and the chart load on first use.
- Idle CPU is ~0: no intervals while the window is hidden (`visibilitychange`), no polling in hidden workspace tabs, and no animation loops.
- Animations are transform/opacity only, 120–200 ms, and turned off under `prefers-reduced-motion`.
- Long tables keep the current rendering. Only add list virtualisation if a page measurably needs it.
- Fonts: use **Inter** variable, self-hosted (one `woff2`, `font-display: swap`) *(my call)*. Use tabular numerals for all money columns.

## 8. Visual system ("A · Stripe-like")

Apply this through the dashboard's token layer: the `:root` / `.dark` variables in `frontend/styles.css`, and the legacy variables in `web/static/css/dash.css` that `app-shell.css` already maps.

**Customer pages must not change (my call).** The default theme (`web/themes/default/theme.json`) is shared with checkout and invoice pages (`internal/look/look.go`). So:
- The new palette lives in **dashboard-only** variables and stylesheets. Don't change the default theme's `--color-accent` or other shared tokens.
- **Custom themes keep their current effect:** their dark/light default and fonts apply as now. If a custom theme sets an accent, it overrides the violet in the dashboard too.

| Token | Light | Dark (my call) |
|---|---|---|
| background | `#ffffff` (content), sidebar `#f7f8fa` | `#0f1117`, sidebar `#14161d` |
| foreground | `#1a1f36` | `#e6e8ef` |
| muted-foreground | `#5c6478` | `#9aa1b2` |
| border | `#e3e8ee` | `#262a35` |
| primary / ring | `#635bff` | `#8b85ff` |
| accent (selected nav, tab) | `#ebeafe` bg, `#4b44d6` text | `#24224a` bg, `#c4c1ff` text |
| success / warning / danger pills | `#d7f7e6`/`#0b6b47`, `#fdf0d5`/`#8a5a00`, `#fde2e1`/`#a4261d` | builder derives, ≥ 4.5:1 |
| radius | 8px controls, 10px cards | same |

- Fewer boxes: separate sections with spacing and a single hairline, not a bordered card around everything.
- Tables use a header row in muted small caps, 1px row dividers, a hover tint and right-aligned tabular amounts.
- Status pills appear everywhere a status is shown (payments, subscriptions, invoices, disputes, e-invoices).
- All colours must pass WCAG AA in both modes.
- The current mockup is `.superpowers/brainstorm/*/content/visual-direction.html` (option A).

## 9. Constraints

- Keep the existing frontend contract: the Go views remain the source of truth, `adapter.ts` tests stay green, `?classic=1` and the no-JS fallback keep working.
- Stack as is: React 19, TanStack Router/Query, Tailwind 4, shadcn/Radix, lucide icons, Bun, Vite. Go `html/template` on the server, SQLite and Postgres.
- Content Security Policy: inline styles use the existing `data-style-nonce`. Load nothing from third-party origins (fonts are self-hosted).
- Work is test-first with small commits (repo convention). New Go query: unit tests on both databases. Tabs, command bar and chart: Vitest plus a Playwright flow.
- **The dashboard has no Playwright setup yet**; only the marketing site does (`frontend/playwright.home.config.ts`). The plan must include building one: start the hub against a temp data dir, seed an owner, get through 2FA (the dev-only 2FA skip exists), and seed ledger rows for the chart.
- Phone layout keeps 44px touch targets (already in the shell).

## 10. Left to the builder

- Component structure, state management for tabs (a small store versus router context), and file layout.
- The exact HTML attributes used to pass chart buckets through the template.
- Whether the command bar is hand-rolled or uses `cmdk`, within the bundle budget.
- Exact dark-mode and status-pill shades, as long as they pass contrast.
- Order of delivery. Suggested: tokens and visual pass → tabs → chart (Go query then UI) → command bar → the smaller features.
