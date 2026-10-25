# Dashboard app frame: implementation plan

> For agentic workers: execute lane by lane; each lane is TDD with small commits, no attribution trailers.

**Goal:** Make the dashboard feel like an application, not a set of pages. The target is the mockup the user approved: one title bar with tabs, a toolbar per screen, records in a side pane, inner tabs on records, and a status bar.

**Builds on:** `docs/superpowers/specs/2026-10-25-dashboard-redesign-design.md` (tabs, tokens, data contract). Everything there still holds:
- Pages come from Go HTML through `adapter.ts`.
- No JSON API.
- `?classic=1` and the no-JS fallback keep working.
- Strict CSP.
- Tokens only; the dashboard stylesheets contain no fixed hex colours.

**Approved mockup:** `.superpowers/brainstorm/882996-1792900581/content/app-frame.html`.

## Global constraints

- Tokens from `frontend/styles.css` only. `tokens.test.ts` forbids hex colours in `workspace.css`/`app-shell.css`.
- Desktop ≥ 1100px gets the full frame. Below that, the side pane falls back to normal navigation, and the phone layout keeps 44px targets and the "N open" tab sheet.
- Every existing unit, Go and browser test stays green. Update a test only when the behaviour it pins is intentionally replaced, and say so in the commit.
- Gzipped JS growth for this plan: ≤ 15 KB in total.
- Verify visually: throwaway Playwright screenshot spec (never committed) at 1440×900 light and dark, plus 390×844. Look at the screenshots before reporting.

## Lane F (frontend shell): Tasks A, B

### Task A: Title bar, sidebar trim, status bar

**Title bar (40px):**
- Brand on the left (logo + "paymux", width = sidebar width; collapses with the sidebar), then the workspace tabs, then search, LIVE/TEST badge, refresh, theme and account on the right.
- The separate breadcrumb header row and the separate tab-strip row are removed: the tab strip lives inside the title bar.
- Tabs keep all Task 6 behaviour (context menu, drag, shortcuts, middle-click, phone sheet).

**Sidebar:**
- Remove the "Payment workspace / Self-hosted infrastructure" card and the "Your infrastructure. Your control." tagline.
- Section captions stay small.
- Nav items show a right-aligned count when the nav entry carries one (`data-count`, added by lane G; render nothing when absent).

**Status bar (24px, bottom):**
- Left: connection (`navigator.onLine`, online/offline events) and "Updated N s ago" for the active tab. Update the text once every 10 s with a single timer that is paused while `document.hidden`; the idle test must still pass.
- Warnings: the `page.warnings` strip under the header is removed. Warnings become a status-bar chip ("▲ 2FA disabled (dev)", or "▲ N warnings" when there are several) that opens a popover listing them.
- Right: shortcut hints and the `?` sheet button.
- The old footer ("Paymux control plane / API reference") is removed; "API reference" moves into the account menu.

### Task B: Screen toolbar

Every non-auth page gets one 44px sticky toolbar row, built from the server content in `render.tsx`:
- **Title:** the page `h1`, without the inline mode/status spans. A status span inside the h1 (e.g. on a record) stays as a pill next to the title. The duplicate "live" mode span is dropped because the title bar shows the mode.
- **Description:** the first `p.muted` directly after the h1 becomes an ⓘ button with a tooltip/popover, not visible text.
- **Actions:** links/buttons in `nav.overview-actions` (and any `a.btn` directly after the h1/description) move to the right of the toolbar. The first is primary, the others secondary.
- **Back link:** a leading back link such as "← Payments" becomes a small back button left of the title.
- **Filter chips:** when the page has a GET `form.filters` with `select[name=status]`, its options render as chips in the toolbar (All + each status), and clicking one applies it. The rest of the filter form stays below the toolbar as now.
- The toolbar stays put while the content scrolls.

## Lane G (Go + record views): Tasks E, D, C

### Task E (Go): Nav counts

- In `web/templates/_dash.html`, nav links get `data-count` for **disputes** (open disputes that need a response: reuse an existing store count if one exists, else add one with tests on both DBs) and **alerts** (reuse `AlertsOpen`).
- No count means no attribute.
- `adapter.ts` reads `data-count` into `nav[].count?: number` (adapter test).

### Task D: Inner tabs on record pages

A record page is any dashboard page whose path is `/dashboard/<section>/<id>` (payments, subscriptions, invoices, credit-notes, disputes, apps, providers, tax-entities, …).
- Its content after the toolbar is split at its section headings (h2/h3 that start a section): "Details" (everything before the first section heading), then one tab per section, named by the heading text.
- Use the Radix Tabs already in `components/ui/tabs.tsx`. Keep the selected tab in component state, plus `#tab-<slug>` in the URL hash so a reload keeps it.
- Forms inside sections keep working, as do dirty tracking and data-once.
- Pages with fewer than 2 sections stay as they are.
- New file `section-tabs.tsx`; hook it in `render.tsx`'s `PageContent` with a small, isolated change.

### Task C: Side pane (peek)

- On list pages (desktop ≥ 1100px), a plain click on a row's record link (`/dashboard/<section>/<id>` inside a table row) opens that record in a right-side pane (420px; resizable 360–640px by dragging its edge; width saved in localStorage). The list stays visible, and the clicked row is highlighted.
- The pane state is in the URL as `?peek=<record path>`. Reload restores it, Back closes it, and query-only navigation stays in the same tab (existing rule).
- Pane header: record title and status (from its toolbar), with buttons "Open in tab" (`openHref(path,"new")`, then close the pane) and Close (Esc).
- Pane body: the record page's content with inner tabs (Task D), fetched through the same query cache (`queryKey(path)`). Forms inside work. An unsaved-edits guard applies when closing the pane.
- j/k row navigation with the pane open moves the selection and updates the pane. Enter opens in a tab.
- Ctrl/Meta/middle-click on row links keeps browser behaviour. Alt-click keeps "open in new tab".
- New files `peek-pane.tsx`, `peek.ts` (pure: path detection, URL param handling, width clamp; unit tests). Hook into `tab-view.tsx` layout and the WorkspaceLink click path with minimal changes.

## Merge and finish

The controller merges F then G, resolves conflicts, runs all suites plus `scripts/ci.sh`, re-shoots screenshots (payments list with pane, payment in a tab with inner tabs, settings, overview; light, dark and phone), compares them with the mockup, then pushes to PR #7.
