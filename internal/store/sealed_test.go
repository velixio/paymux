package store_test

import (
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"testing"

	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/store"
)

// A master key rotation reseals the columns listed in store/sealed.go. A new
// encrypted column that is not listed there would become unreadable the day the
// old key is removed, so every *_enc column in the schema has to be in the list,
// and the list may name nothing else.
func TestEverySealedColumnIsListedForKeyRotation(t *testing.T) {
	re := regexp.MustCompile(`(?i)\b([a-z_]+_enc)\b\s+(?:BYTEA|BLOB)`)
	for _, dialect := range []string{"sqlite", "postgres"} {
		files, err := fs.Glob(migrations.FS, dialect+"/*.sql")
		if err != nil || len(files) == 0 {
			t.Fatalf("%s migrations: %v", dialect, err)
		}
		found := map[string]bool{}
		for _, f := range files {
			b, _ := fs.ReadFile(migrations.FS, f)
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				found[m[1]] = true
			}
			// ALTER TABLE ... ADD COLUMN x_enc BYTEA is matched by the same pattern.
		}
		var want []string
		for c := range found {
			want = append(want, c)
		}
		sort.Strings(want)
		got := store.SealedColumns()
		sort.Strings(got)
		if !slices.Equal(want, got) {
			t.Errorf("%s: columns in the schema %v, columns resealed %v", dialect, want, got)
		}
	}
}
