package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"github.com/velixio/paymux/internal/store/dialect"
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
	db, err := openSQLiteFile(ctx, path)
	if err != nil {
		unlock()
		return nil, err
	}
	db.unlock = unlock
	return db, nil
}

// openSQLiteFile opens a SQLite file with one writer and a read pool. It
// takes no data-dir lock: the caller holds it.
func openSQLiteFile(ctx context.Context, path string) (*DB, error) {
	// A new database file is the hub's user's alone (sealed secrets, customer
	// data); SQLite gives its -wal and -shm files the same mode. An existing
	// file keeps the mode the operator gave it.
	if f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		f.Close()
	} else if !os.IsExist(err) {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	escaped := (&url.URL{Path: path}).EscapedPath()
	writer, err := sql.Open("sqlite", "file:"+escaped+"?"+sqlitePragmas+"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	writer.SetMaxOpenConns(1)
	if err := writer.PingContext(ctx); err != nil {
		writer.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	reader, err := sql.Open("sqlite", "file:"+escaped+"?"+sqlitePragmas)
	if err != nil {
		writer.Close()
		return nil, err
	}
	reader.SetMaxOpenConns(4)
	return &DB{writer: writer, reader: reader, d: dialect.SQLite{}, path: path}, nil
}

func mkdir0700(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	return nil
}

// LooseFiles lists the SQLite files of db (the database, its -wal and -shm)
// that group or others can read or write. A file the hub creates is 0600, but
// one that existed keeps the mode the operator gave it. It is empty on
// PostgreSQL.
func (db *DB) LooseFiles() []string {
	if db.path == "" {
		return nil
	}
	var out []string
	for _, side := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(db.path + side); err == nil && fi.Mode().Perm()&0o077 != 0 {
			out = append(out, db.path+side)
		}
	}
	return out
}
