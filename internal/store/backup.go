package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrBackupUnsupported means the database cannot be backed up by Paymux
// itself (PostgreSQL): use pg_dump.
var ErrBackupUnsupported = errors.New("store: this database cannot be backed up by Paymux")

// Backup writes a consistent copy of the database to dest. On SQLite it is
// VACUUM INTO: a standalone database file, taken from a read transaction, so
// it is safe while writes continue within this process. dest must not exist
// and is created with mode 0600. On PostgreSQL it returns ErrBackupUnsupported
// with advice to use pg_dump.
func (db *DB) Backup(ctx context.Context, dest string) error {
	if db.d.Name() != "sqlite" {
		return fmt.Errorf("%w: back up PostgreSQL with pg_dump, for example: pg_dump --format=custom \"$PAYMUX_DATABASE_URL\" > paymux.dump",
			ErrBackupUnsupported)
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("store: backup target %s already exists; refusing to overwrite it", abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return err
	}
	if _, err := db.writer.ExecContext(ctx, "VACUUM INTO ?", abs); err != nil {
		_ = os.Remove(abs)
		return fmt.Errorf("store: backup to %s: %w", abs, err)
	}
	return os.Chmod(abs, 0o600)
}
