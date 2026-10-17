package store

import (
	"context"
	"database/sql"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/paymux/paymux/internal/store/dialect"
)

func openPostgres(ctx context.Context, databaseURL string) (*DB, error) {
	pool, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(10)
	if err := pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{writer: pool, reader: pool, d: dialect.Postgres{}}, nil
}
