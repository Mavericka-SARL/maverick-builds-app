// Package dbx names what a store runs its statements on: a connection pool,
// or a transaction. *pgxpool.Pool and pgx.Tx both satisfy DB (a pgx.Tx's
// Begin is a savepoint), so a store built on a transaction does exactly what
// it does on the pool — and leaves nothing behind when that transaction is
// rolled back. The AI Developer's proposal check runs a plan that way.
package dbx

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is a pool or a transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}
