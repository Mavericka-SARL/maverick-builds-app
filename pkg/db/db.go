package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// connectTimeout bounds how long Connect will keep retrying. Long enough to
// outlast a database or connection pooler that is still coming up beside this
// service, short enough that a genuinely wrong DATABASE_URL surfaces during a
// deploy rather than looking like a hang.
const connectTimeout = 90 * time.Second

// Connect opens a pgxpool using the given DATABASE_URL and verifies
// connectivity, retrying until the database answers or connectTimeout passes.
//
// The retry is the point. Every service calls this at boot and treats failure
// as fatal, so a single refused connection ends the process — and on a fresh
// rollout that is a coin flip, not an error: the gateway raced pgbouncer on
// 2026-08-19 and exited with
//
//	ping database: dial tcp 10.43.19.68:5433: connect: connection refused
//
// Kubernetes restarted it and the second attempt worked, so the only visible
// trace was a restart count. That self-healing is real but it is not free:
// CrashLoopBackOff's delay grows with each attempt, so a pooler that takes a
// minute to come up turns a few seconds of waiting into minutes of downtime.
// Waiting here costs nothing when the database is already up.
//
// Only the reachability check is retried. A malformed URL or a rejected
// credential fails immediately, because no amount of waiting fixes either.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	cfg.MaxConns = 25
	cfg.MinConns = 2

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pingWithRetry(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// pingWithRetry backs off from 250ms to 5s, which reaches the deadline in
// about two dozen attempts rather than hammering a database that is busy
// starting up.
func pingWithRetry(ctx context.Context, pool *pgxpool.Pool) error {
	deadline := time.Now().Add(connectTimeout)
	delay := 250 * time.Millisecond
	const maxDelay = 5 * time.Second

	var lastErr error
	for attempt := 1; ; attempt++ {
		if err := pool.Ping(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}

		// A cancelled context means the caller is shutting down; retrying
		// would ignore that, and every further attempt fails the same way.
		if ctx.Err() != nil {
			return fmt.Errorf("ping database: %w", ctx.Err())
		}
		if remaining := time.Until(deadline); remaining <= 0 {
			return fmt.Errorf("ping database: gave up after %s and %d attempts: %w", connectTimeout, attempt, lastErr)
		} else if delay > remaining {
			delay = remaining
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("ping database: %w", ctx.Err())
		case <-time.After(delay):
		}
		if delay < maxDelay {
			delay *= 2
		}
	}
}
