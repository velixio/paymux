# Dependencies

Third-party dependencies, one line each with the reason.

- `modernc.org/sqlite`: pure-Go SQLite; keeps `CGO_ENABLED=0`; required by Litestream library mode.
- `github.com/jackc/pgx/v5`: maintained pure-Go Postgres driver, used via its `database/sql` adapter (`stdlib`).
- `golang.org/x/crypto` — argon2id password hashing for dashboard users (Go team maintained).
- `github.com/go-pdf/fpdf` (MIT): pure-Go PDF writer (maintained fork of gofpdf) with UTF-8 TrueType embedding and subsetting, a fixed creation date and sorted catalog for byte-deterministic invoice PDFs. Used by `internal/invoice/render`.
- `golang.org/x/term` (BSD-3, Go team): echo-free password prompt for `paymux user create`. Used by `internal/cli`.
- Noto Sans Regular and Bold (SIL OFL 1.1, license in `internal/invoice/render/fonts/OFL.txt`): embedded with `go:embed` because it has the rupee sign U+20B9; subset into each PDF. Not a Go module.
- OpenID Connect single sign-on (`internal/oidc`) adds no module: the authorization code flow with PKCE, discovery, JWKS and RS256/ES256 id_token verification are about 400 lines of standard library (`crypto/rsa`, `crypto/ecdsa`, `net/http`), which keeps the verified surface small and avoids pulling `golang.org/x/oauth2` and a JOSE library for one flow.
