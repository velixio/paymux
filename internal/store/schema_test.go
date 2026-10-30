package store_test

import (
	"reflect"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestDescribeSchemaNormalises(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		fsys := both(map[string]string{"0001_t.sql": `CREATE TABLE t (
			id TEXT PRIMARY KEY, n INTEGER NOT NULL, big BIGINT, tag TEXT NOT NULL, UNIQUE (tag));
			CREATE INDEX t_n ON t (n, big);
			CREATE UNIQUE INDEX t_part ON t (big) WHERE big IS NOT NULL;`})
		if err := db.Migrate(ctx, fsys); err != nil {
			t.Fatal(err)
		}
		got, err := db.DescribeSchema(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := store.Schema{Tables: map[string]store.Table{"t": {
			Columns: map[string]store.Column{
				"id": {Type: "text", NotNull: true}, "n": {Type: "int", NotNull: true},
				"big": {Type: "int"}, "tag": {Type: "text", NotNull: true},
			},
			Indexes: map[string]store.Index{
				"primary":    {Columns: []string{"id"}, Unique: true},
				"unique:tag": {Columns: []string{"tag"}, Unique: true},
				"t_n":        {Columns: []string{"n", "big"}},
				"t_part":     {Columns: []string{"big"}, Unique: true, Partial: true},
			},
		}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got  %+v\nwant %+v", got, want)
		}
	})
}
