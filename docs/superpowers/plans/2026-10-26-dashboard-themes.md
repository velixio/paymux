# Dashboard themes: implementation plan

> **For agentic workers:** one builder executes tasks in order; the controller reviews each. TDD: a failing test commit, then the implementation commit. No attribution trailers. Prettier/gofmt on touched files.

**Goal:** Bundled themes Stripe/Paddle/Midnight (light + dark), an org default plus a per-user choice, and themes made from seed colours, with an in-dashboard editor, management and export/import.

**Spec:** `docs/superpowers/specs/2026-10-26-dashboard-themes-design.md` (read it first).

## Global constraints

- **Data contract:** the dashboard still reads Go HTML views through `adapter.ts`. No JSON API, except the read-only theme manifest endpoint below.
- **Customer pages:** untouched. Guard test: the customer-page CSS and the `tokens` behaviour are unchanged.
- **Colours:** no fixed hex colours in `workspace.css` / `app-shell.css` (the existing test). Every theme passes 4.5:1 on all text pairs, light and dark.
- **Status tokens** (`--status-*`) are never set by a theme.
- **Bundle:** the editor is lazy-loaded; `main.js` growth ≤ 8 KB gz.
- **Gates:** `go test ./internal/...`; `bun run --cwd frontend typecheck && test:frontend`; `PAYMUX_E2E_PORT=<own port> test:dashboard` before reporting. Screenshots verified by the builder (throwaway spec, never committed).

## File map

| File | Role |
|---|---|
| `internal/web/theme.go` (+test) | `Manifest.Dashboard` (seeds, dark seeds, scheme, overrides), validation, `GET /theme/<name>/dashboard.json` |
| `web/themes/{stripe,paddle,midnight}/theme.json` | Bundled dashboard themes (`default` stays the customer-page theme) |
| `frontend/dashboard/src/theme-derive.ts` (+test) | Pure: seeds → full light/dark token sets, contrast nudging, `contrastReport` |
| `frontend/dashboard/src/theme-apply.ts` (+test) | Fetch the manifest, write tokens into a `<style nonce>` scoped to the dashboard, scheme default |
| `internal/migrations/{sqlite,postgres}/0034_user_theme.sql` | `ALTER TABLE users ADD COLUMN theme TEXT` |
| `internal/store/users*.go` (+test) | `SetUserTheme`, `UserTheme`, `ThemeUsers(name)`, `RenameThemeRefs` |
| `internal/dashboard/appearance.go`, `account.go` (+tests) | Effective theme in the shell view, org default, user choice, theme CRUD, export/import |
| `web/templates/dash_appearance.html`, `dash_account.html`, `dash_theme_edit.html` | Server forms (no-JS works) |
| `frontend/dashboard/src/theme-editor.tsx` | Lazy: colour pickers, live preview, contrast ✓/✗ |
| `docs/themes.md` | How to make a theme |

## Tasks

### Task 1: Manifest `dashboard` block and endpoint (Go)
- **Manifest:** `Manifest.Dashboard *DashboardTheme`, with fields:
  - `Seeds Seeds` and `DarkSeeds *Seeds`;
  - `Scheme string` (`""|light|dark`);
  - `Overrides map[string]map[string]string` (`light|dark` → token → colour).
- **Seeds:** `{Accent, Background, Sidebar, Text, SidebarText string}`, each optional except `Accent`, all `#rrggbb`.
- **Overrides:** keys limited to the dashboard token allow-list (exported `DashboardTokens`, excluding `--status-*`); values `#rrggbb`.
- **Endpoint:** `GET /theme/<name>/dashboard.json` returns `{"name","scheme","seeds","dark_seeds","overrides"}`, with an ETag and `no-cache`.
- **Tests:** valid and invalid manifests (bad hex, a status token in overrides, unknown scheme), endpoint 200/404, a theme without a `dashboard` block serves `{}` seeds.

### Task 2: Derivation (TS, pure)
- **Signature:** `derive(seeds, darkSeeds?, overrides?) → { light: Record<string,string>, dark: Record<string,string> }`, covering every non-status token in `frontend/styles.css` (`--background`, `--foreground`, `--card`, `--sidebar`, `--primary`, `--primary-foreground`, `--ring`, `--ui-accent`, `--accent-foreground`, `--muted`, `--muted-foreground`, `--border`, `--input`, `--secondary*`, `--popover*`).
- **Contrast:** `contrast(a,b)`, plus `contrastReport(tokens) → {pair, ratio, ok}[]` for the same pairs `tokens.test.ts` checks.
- **Nudging:** text tokens are adjusted until each pair is ≥ 4.5.
- **Tests:**
  - Stripe seeds reproduce today's palette within a small tolerance (or exactly, if the builder keeps the current values as the Stripe overrides).
  - Every bundled theme passes `contrastReport` in light and dark.
  - Extreme seeds (yellow on white, black on black) still pass after nudging.
  - Overrides win.

### Task 3: Bundled themes
- `web/themes/stripe` (the current colours), `paddle` (accent `#0d7d68`, background `#f5f5f2`, sidebar `#121417`, sidebar text light, text `#16181d`) and `midnight` (accent `#c6f36b`, background `#0c0d10`, scheme `dark`, plus light seeds for its light version).
- Test that all three load and pass contrast (reuses Task 2).

### Task 4: Apply themes in the dashboard
- **Effective theme:** the page carries it. The shell view gets `DashboardTheme` (user → org → `stripe`), rendered as `data-dashboard-theme="<name>"` on `#paymux-react-root`; `adapter.ts` reads it into `page.dashboardTheme`.
- **`theme-apply.ts`:** fetches the manifest once per name, derives, and writes `:root{…}` / `.dark{…}` into one `<style>` with the page nonce, replaced on change.
- **Scheme:** `scheme: "dark"` sets the default when the user has no saved toggle. The user's toggle wins.
- **Cleanup:** remove the `minimal-dark` checks from `shell.tsx`.
- **Tests:** unit tests for apply (style content, nonce, no duplicate style tags); a shell test where a theme switch changes `--primary`.

### Task 5: Per-user choice
- **Migration:** `0034_user_theme.sql` on both databases.
- **Store:**
  - `SetUserTheme(ctx, tx, userID, name string)` (empty clears);
  - `UserTheme`;
  - `ThemeUsers(ctx, q, org, name) []string` (emails);
  - `RenameThemeRefs(ctx, tx, org, old, new)`, which updates the org setting and user rows.
- **Your account:** a "Theme" select (Organisation default (<name>) / each theme), `POST /dashboard/account/theme` with CSRF, audited.
- **Tests:** store tests on SQLite and Postgres; page tests (choice saved, effective theme rendered, other users unaffected).

### Task 6: Appearance page: cards and org default
- **Cards:** each theme shows a mini preview, built from the derived tokens of both schemes (lazy-load the derivation for previews, or the server sends the seeds and the client renders).
- **Card contents:** name, version, "Built-in" / "Custom", "Default" badge, user count.
- **Actions** (`SettingsWrite`): Set as default, Duplicate, Edit (custom only), Rename (custom), Delete (custom), Export.
- **New theme** button.
- **Tests:** page renders the cards; permissions (a viewer sees the cards without actions); set default works.

### Task 7: Editor
- **Routes:**
  - `GET /dashboard/appearance/themes/new?from=<name>` and `/themes/<name>/edit`: a server form (name, version, the seeds as `type=color`, dark seeds toggle, scheme).
  - `POST` creates or saves.
- **Validation:** name rules, collisions with bundled names, hex.
- **Saving:** atomic write to `data/themes/<name>/theme.json`, keeping any non-dashboard fields of the source theme; audit entry.
- **React enhancement (`theme-editor.tsx`, lazy):**
  - Live preview: apply the derived tokens to the whole dashboard while editing, restored on leave or cancel.
  - Light/dark preview switch.
  - Contrast table ✓/✗ with ratios; Save is disabled while any pair fails after nudging (nudging should make this rare).
- **Rename:** `POST /themes/<name>/rename`, using `RenameThemeRefs`; blocked if apps' customer-page theme uses the name (message lists the apps).
- **Delete:** `POST /themes/<name>/delete`, refused while in use (lists the default and users), recent-auth guarded like other sensitive actions.
- **Tests:** create, edit, rename (refs follow), delete-in-use refusal, bundled read-only, path traversal (`../`) rejected, unsaved-changes guard.

### Task 8: Export / import
- **Export:** `GET /dashboard/appearance/themes/<name>/export.json` (download).
- **Import:** `POST /dashboard/appearance/themes/import` (multipart, ≤ 64 KB), validated with the same rules. A name collision asks for a new name (form re-shown with an error).
- **Tests:** round-trip, oversize, invalid JSON, CSS/asset files in an import are ignored (only `theme.json` is accepted).

### Task 9: Verification and docs
- **Browser flows** (`frontend/dashboard/e2e/themes.spec.ts`):
  - an admin creates a theme from Paddle, changes the accent, saves, and sets it as default;
  - a user overrides it with Midnight;
  - the dark toggle still works;
  - deleting a theme in use is refused.
- **Screenshots** of the three bundled themes × light/dark (builder-verified).
- **Docs:** `docs/themes.md` (seed fields, derivation, overrides, scheme, editor, export/import, files on disk); link it from the README docs table.

## Review focus

1. **Name conflicts:** a custom theme named like a bundled one (`stripe`) must be refused, not shadow it silently.
2. **Missing theme:** a user's chosen theme is deleted on disk outside the app → falls back to the org default, then `stripe`, with no crash.
3. **Unusual seeds:** seeds with very light accents (yellow) must still give a readable primary button (button text flips to dark).
4. **Concurrent edits:** two admins editing the same theme → last write wins, but the file is never half-written (atomic write).
5. **Customer pages:** a theme used for customer pages (`tokens`) that also gets a `dashboard` block keeps rendering customer pages exactly as before.
