// Package db opens and holds the Postgres connection pool.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open returns a connection pool that has been checked with a real round trip.
//
// pgxpool.New is lazy: it will happily return a pool pointing at a database
// that does not exist. The Ping turns a configuration mistake into a startup
// failure instead of a confusing error on the first user request.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: parse DATABASE_URL: %w", err)
	}

	// Ten is the ceiling, and pgxpool queues anyone who arrives past it without
	// limit. The bound on that queue is repo's poolAcquireTimeout — this number
	// on its own decides only how many requests make progress, not how long the
	// rest wait.
	cfg.MaxConns = 10
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}
