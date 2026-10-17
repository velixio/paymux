package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"github.com/paymux/paymux/internal/store/dialect"
)

const sqlitePragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

func openSQLite(ctx context.Context, path string) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("empty sqlite path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	unlock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*DB, error) { unlock(); return nil, err }

	escaped := (&url.URL{Path: path}).EscapedPath()
	writer, err := sql.Open("sqlite", "file:"+escaped+"?"+sqlitePragmas+"&_txlock=immediate")
	if err != nil {
		return fail(err)
	}
	writer.SetMaxOpenConns(1)
	if err := writer.PingContext(ctx); err != nil {
		writer.Close()
		return fail(fmt.Errorf("open %s: %w", path, err))
	}
	reader, err := sql.Open("sqlite", "file:"+escaped+"?"+sqlitePragmas)
	if err != nil {
		writer.Close()
		return fail(err)
	}
	reader.SetMaxOpenConns(4)
	return &DB{writer: writer, reader: reader, d: dialect.SQLite{}, unlock: unlock}, nil
}
