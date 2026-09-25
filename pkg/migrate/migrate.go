package migrate

import (
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockKey is an arbitrary, fixed identifier for the Postgres
// advisory lock that serializes concurrent Run() calls across processes —
// e.g. two gateway pods mid-rolling-update both booting and calling Run()
// at once. It is not a secret, just a fixed key both sides agree on.
const migrationLockKey = 847_291_003

// Run applies all SQL migration files from the given embed.FS in lexical order.
// It is idempotent: already-applied migrations (matched by filename + checksum) are skipped.
// A mismatch in checksum for an already-applied migration returns an error.
//
// The whole read-check-apply-record cycle is serialized across processes by
// a Postgres advisory lock held for the duration of Run: without it, two
// concurrent callers can both see a migration as unapplied, both execute its
// SQL, and race on the same _migrations INSERT — at best a duplicate-key
// error, at worst a non-idempotent DDL statement silently running twice.
func Run(ctx context.Context, pool *pgxpool.Pool, migrations embed.FS, dir string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration lock: %w", err)
	}
	defer conn.Release()

	// The lock is TRANSACTION-scoped, held by a transaction that spans the
	// run. It used to be a session-level pg_advisory_lock/unlock pair on this
	// connection, which assumed one client connection is one server session.
	// PgBouncer in transaction pooling — how the gateway reaches Postgres —
	// breaks that: each statement outside a transaction may run on a
	// different server connection, so the unlock could miss, and the lock
	// stayed held by an idle pooled server connection for good. Every later
	// start then waited in pg_advisory_lock until its liveness probe killed
	// it: a rolling restart that never finishes (found rehearsing an ingress
	// migration, 2026-09-25). A transaction keeps one server connection even
	// through PgBouncer, and its end — rollback here, on every path, or the
	// connection dropping — releases the lock.
	lockTx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration lock transaction: %w", err)
	}
	defer func() { //nolint:contextcheck
		// A fresh context: the outer one may be done by now, and the lock
		// must be released regardless.
		_ = lockTx.Rollback(context.Background())
	}()
	if _, err := lockTx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}

	if err := ensureMigrationsTable(ctx, pool); err != nil {
		return err
	}

	entries, err := collectFiles(migrations, dir)
	if err != nil {
		return err
	}

	applied, err := loadApplied(ctx, pool)
	if err != nil {
		return err
	}

	for _, e := range entries {
		content, err := fs.ReadFile(migrations, e)
		if err != nil {
			return fmt.Errorf("read %s: %w", e, err)
		}

		checksum := fmt.Sprintf("%x", sha256.Sum256(content))
		name := strings.TrimPrefix(e, dir+"/")

		if prev, ok := applied[name]; ok {
			if prev != checksum {
				return fmt.Errorf("migration %s checksum mismatch: expected %s got %s", name, prev, checksum)
			}
			continue // already applied
		}

		if _, err := pool.Exec(ctx, string(content)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}

		if _, err := pool.Exec(ctx,
			`INSERT INTO _migrations (name, checksum) VALUES ($1, $2)`,
			name, checksum,
		); err != nil {
			return fmt.Errorf("record %s: %w", name, err)
		}
	}

	return nil
}

func ensureMigrationsTable(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS _migrations (
			name       TEXT PRIMARY KEY,
			checksum   TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`)
	return err
}

func loadApplied(ctx context.Context, pool *pgxpool.Pool) (map[string]string, error) {
	rows, err := pool.Query(ctx, `SELECT name, checksum FROM _migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var name, checksum string
		if err := rows.Scan(&name, &checksum); err != nil {
			return nil, err
		}
		result[name] = checksum
	}
	return result, rows.Err()
}

func collectFiles(migrations embed.FS, dir string) ([]string, error) {
	var files []string
	entries, err := fs.ReadDir(migrations, dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			// path.Join cleans "." + "file.sql" → "file.sql" (avoids invalid "./file.sql")
			files = append(files, path.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}
