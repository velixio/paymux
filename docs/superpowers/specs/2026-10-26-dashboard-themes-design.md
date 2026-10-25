# Dashboard themes: design

*Status: approved scope (full, with editor) · 2026-10-26 · builds on `2026-10-25-dashboard-redesign-design.md`*

## Why

The dashboard uses one fixed palette (A · Stripe-like). The user wants:
- the other two approved looks as real choices (B · Paddle-like: teal accent, dark sidebar, warm off-white; C · Midnight: lime accent, near-black);
- every theme in light **and** dark;
- each person able to pick their own theme;
- making and managing themes to be easy.

## Decisions (made with the user)

1. **Bundled themes:** `stripe` (the current look, the built-in default), `paddle`, `midnight`. Each has light and dark versions.
2. **Who chooses:** the organisation sets a **default** (an admin setting, as today), and **each user can override it** for themselves. The resolution order is user choice → org default → `stripe`.
3. **A theme is a few seed colours.** `theme.json` gains a `dashboard` block with seeds (`accent`, `background`, `sidebar`, `text`, optional `sidebar_text`) for light, and optionally separate seeds for dark. Everything else (hover and selected states, borders, muted text, chart and focus colours, the dark version when no dark seeds are given) is **derived**. An `overrides` map can set any individual dashboard token for advanced use.
4. **Status colours are fixed** in every theme (green, red, amber, blue, slate), so a status means the same everywhere.
5. **Readability is enforced:** derived text colours are nudged until every text pair reaches **4.5:1**. The editor shows ✓/✗ per pair, and bundled themes are checked in tests.
6. **Managed in the dashboard** (Appearance page, needs `SettingsWrite`):
   - theme cards with a live mini preview;
   - **New**, **Duplicate**, **Edit** (colour pickers + a live preview of the real dashboard), **Rename**, **Delete**;
   - **Set as org default**;
   - **Export** (download `theme.json`) and **Import** (upload).

   Bundled themes are read-only: duplicate one to change it.
7. **Storage:**
   - Custom themes are files in `data/themes/<name>/theme.json`; the hub already loads and live-reloads that folder.
   - The per-user choice is a new nullable `users.theme` column (migration on SQLite and Postgres).
8. **Customer pages never change.** Dashboard seeds and derived tokens apply to the React dashboard only. Customer-page tokens (`tokens` in `theme.json`) keep their current meaning.
9. **The light/dark toggle works with every theme.** The `minimal-dark` name check in `shell.tsx` is removed. A theme may declare `"scheme": "dark"` to default to dark, but the user's toggle still wins.

## Non-goals

- No editing of customer-page themes in the editor: only the `dashboard` block. Existing `tokens`, `font_stack` and css files are kept as they are when a theme is duplicated or edited.
- No custom fonts in the editor (font stack stays as today).
- No per-app dashboard themes. Per-app themes remain for customer pages only.

## Anti-goals

- A theme can't make text unreadable.
- A theme can't change what a status colour means.
- A theme can't break customer pages.
- Editing a theme can't silently break users who chose it:
  - **Rename** updates every reference (org default and user choices). App customer-page settings that use the name block the rename, with a clear message.
  - **Delete** of a theme in use is refused, with a list of who uses it.

## Constraints

- **Contracts:** keep everything from the redesign spec (the data contract via the Go HTML views, CSP, no fixed hex colours in dashboard CSS, test-first development, small commits, no attribution trailers).
- **One derivation:** the seed → token derivation is written **once**, in TypeScript (pure module), and used both by the dashboard and by the editor's live preview. Go validates seeds (hex colours, allowed names) and serves the manifest; it does not derive.
- **Theme writes** are atomic (temp file + rename), audited (audit log), and limited to `data/themes/` with a validated name.
- **Bundle budget:** the editor is lazy-loaded.

## Left to the builder

- The colour maths. Suggested approach: OKLCH or HSL mixing for hover, border and muted tones; dark derived by inverting lightness while keeping hue; contrast nudging by stepping lightness.
- Exact URLs, file layout and component structure, within the plan's interfaces.
