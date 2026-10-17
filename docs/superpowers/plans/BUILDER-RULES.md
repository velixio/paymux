# Builder rules (read before every task)

- **Spec:** `docs/superpowers/specs/2026-10-17-paymux-design.md` is the source of truth. Section numbers (§) refer to it.
- **Git:**
  - Work only in your own worktree/branch, and run `git merge main` before you start.
  - Commit aggressively in small steps: `test(x): add failing …`, then `feat(x): …`, `fix(x): …`, `refactor(x): …`. Never squash or amend.
  - Stage explicit paths only. Never run `git add -A`, and never stage `.claude/`, `data/`, `dist/` or `.env`.
  - **No `Co-Authored-By`, no "Generated with", no trailers of any kind. No real or private emails anywhere; use example.com.** Don't change git config.
- **Testing:** TDD (red, then green). Every DB test runs on both SQLite and Postgres via `storetest.Each`. Before you report, `bash scripts/ci.sh` must exit 0.
- **Migrations:**
  - Each lane owns one migration number, written in lockstep for **both** `migrations/sqlite/` and `migrations/postgres/`.
  - The schema parity test must pass.
  - Allowed types: BIGINT money and time (unix ms UTC), TEXT ids and JSON. Use BLOB for SQLite and BYTEA for Postgres where binary data is needed.
- **SQL placement:** all SQL lives in `internal/store` (new files are fine, e.g. `internal/store/payments.go`). Other packages use store methods. Policy values are read only through `internal/settings`.
- **Dependencies:** stdlib first. Any new dependency needs a line in `docs/dependencies.md` with the reason.
- **Scope:** if something in the spec is unclear and you can't decide sensibly, skip that piece, leave a `// TODO(spec): …` note, and mention it in your report. Don't stop.
- **Report** at the end: the commit list, the test summary, decisions made, and anything skipped.
