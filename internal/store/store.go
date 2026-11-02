// Package store is Blackbox's log store in Postgres (blackbox_db on sparkdb):
// the embedded migrations, the collector's writes, and the API's reads.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// Retention is how long records are kept.
	Retention = 30 * 24 * time.Hour

	// MaxBytes is the most the records may take; the oldest go first.
	MaxBytes = 1 << 30
)

// Store is a connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// Connect opens a pool for connString and checks that the database answers.
// key is the Cove key the string came from, for error messages.
func Connect(ctx context.Context, connString, key string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("database: %s isn't a valid connection string: %w", key, err)
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: %s: %w", key, err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: can't reach the database at %s: %w", key, err)
	}
	return &Store{pool: pool}, nil
}

// Ping checks that the database answers, giving up after 2 seconds.
func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.pool.Ping(ctx)
}

func (s *Store) Close() {
	s.pool.Close()
}

// IsBadData reports whether Postgres refused a value (a data exception), as
// opposed to failing: trying the same write again would fail the same way.
func IsBadData(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && len(pgErr.Code) == 5 && pgErr.Code[:2] == "22"
}
