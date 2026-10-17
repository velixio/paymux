# Dependencies

Third-party dependencies, one line each with the reason. None yet (stdlib only).

- `modernc.org/sqlite`: pure-Go SQLite; keeps `CGO_ENABLED=0`; required by Litestream library mode.
- `github.com/jackc/pgx/v5`: maintained pure-Go Postgres driver, used via its `database/sql` adapter (`stdlib`).
- `golang.org/x/crypto` — argon2id password hashing for dashboard users (Go team maintained).
