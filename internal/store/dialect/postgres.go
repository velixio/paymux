package dialect

import "strconv"

// Postgres is the PostgreSQL dialect.
type Postgres struct{}

func (Postgres) Name() string           { return "postgres" }
func (Postgres) BlobType() string       { return "BYTEA" }
func (Postgres) ForUpdate() string      { return " FOR UPDATE" }
func (Postgres) SkipLocked() string     { return " FOR UPDATE SKIP LOCKED" }
func (Postgres) SupportsSnapshot() bool { return false }

// Rebind rewrites '?' placeholders to $1..$n, ignoring '?' inside
// single-quoted string literals (” is an escaped quote).
func (Postgres) Rebind(q string) string {
	var b []byte
	n := 0
	inQuote := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'':
			inQuote = !inQuote // doubled '' toggles twice, staying correct
			b = append(b, c)
		case c == '?' && !inQuote:
			n++
			b = append(b, '$')
			b = strconv.AppendInt(b, int64(n), 10)
		default:
			b = append(b, c)
		}
	}
	return string(b)
}
