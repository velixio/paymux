// Package dialect holds every SQL difference between SQLite and Postgres.
package dialect

// Dialect abstracts the per-database SQL differences.
type Dialect interface {
	Name() string               // "sqlite" | "postgres"
	Rebind(query string) string // '?' -> '$1..$n' for postgres
	BlobType() string           // "BLOB" | "BYTEA"
	ForUpdate() string          // "" | " FOR UPDATE"
	SkipLocked() string         // "" | " FOR UPDATE SKIP LOCKED"
	SupportsSnapshot() bool     // sqlite true
}
