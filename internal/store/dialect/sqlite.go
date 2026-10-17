package dialect

// SQLite is the SQLite dialect.
type SQLite struct{}

func (SQLite) Name() string           { return "sqlite" }
func (SQLite) Rebind(q string) string { return q }
func (SQLite) BlobType() string       { return "BLOB" }
func (SQLite) ForUpdate() string      { return "" }
func (SQLite) SkipLocked() string     { return "" }
func (SQLite) SupportsSnapshot() bool { return true }
