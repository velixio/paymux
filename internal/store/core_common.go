package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ErrConflict means a unique constraint or an optimistic version check failed.
var ErrConflict = errors.New("store: conflict")

// Queryer is the read surface shared by Tx and Querier, so one store function
// serves both read-only calls and calls inside a write transaction.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// IsUniqueViolation reports whether err is a unique-constraint failure on
// either dialect.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "UNIQUE constraint failed") || strings.Contains(m, "SQLSTATE 23505")
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
