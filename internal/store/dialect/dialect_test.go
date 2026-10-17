package dialect

import "testing"

func TestRebind(t *testing.T) {
	cases := []struct{ in, pg string }{
		{"SELECT ? , ?", "SELECT $1 , $2"},
		{"WHERE a = '?' AND b = ?", "WHERE a = '?' AND b = $1"},
		{"WHERE a = 'it''s ?' AND b = ?", "WHERE a = 'it''s ?' AND b = $1"},
	}
	for _, c := range cases {
		if got := (Postgres{}).Rebind(c.in); got != c.pg {
			t.Errorf("postgres Rebind(%q) = %q, want %q", c.in, got, c.pg)
		}
		if got := (SQLite{}).Rebind(c.in); got != c.in {
			t.Errorf("sqlite Rebind(%q) = %q, want unchanged", c.in, got)
		}
	}
}

func TestDialectDifferences(t *testing.T) {
	s, p := SQLite{}, Postgres{}
	if s.Name() != "sqlite" || p.Name() != "postgres" {
		t.Fatal("names")
	}
	if s.BlobType() != "BLOB" || p.BlobType() != "BYTEA" {
		t.Fatal("blob")
	}
	if s.ForUpdate() != "" || p.ForUpdate() != " FOR UPDATE" {
		t.Fatal("for update")
	}
	if s.SkipLocked() != "" || p.SkipLocked() != " FOR UPDATE SKIP LOCKED" {
		t.Fatal("skip locked")
	}
	if !s.SupportsSnapshot() || p.SupportsSnapshot() {
		t.Fatal("snapshot")
	}
}
