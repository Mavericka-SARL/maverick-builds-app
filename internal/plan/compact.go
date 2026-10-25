package plan

import (
	"context"
	"fmt"
)

// Compact gives deleted rows' disk back to the server. PostgreSQL keeps a
// deleted row's space in its table's files for later writes; VACUUM FULL
// rewrites a table without it. Only in a tenant's own database (the usage
// sweep and the gateway call it there): a shared database's tables hold
// every tenant at once.
//
// A table is compacted when rows were deleted or updated in it since it
// was last compacted (runtime.table_compaction) and its files exceed its
// live data (runtime.live_tables) by at least a megabyte — and, unless
// force, by a fifth of their size, so the sweep leaves a large table alone
// for a small gain.
//
// Forced — straight after a model, application, revision or metric is
// deleted — a table is rewritten (VACUUM FULL), which locks it while it
// runs: about a second at a Playground workspace's size, longer for a large
// table. SKIP_LOCKED leaves a table in use at that moment to the next
// sweep. The sweep itself vacuums (VACUUM): the space becomes free for the
// table's next rows and an empty end of its files goes back, and nothing
// waits for it. It used to rewrite as well, and a model in use qualified
// at almost every sweep — calculated results are rewritten as people type
// — so every five minutes, and at every start, its grids and recalculations
// stood still for the five seconds the rewrite of runtime.calc_result took
// (2026-10-20). Returns the bytes given back.
func Compact(ctx context.Context, db DB, force bool) (int64, error) {
	type table struct {
		oid            uint32
		name           string
		changed, files int64
		filenode       uint32
	}
	minShare := 0.2
	if force {
		minShare = 0
	}
	rows, err := db.Query(ctx, `
		SELECT l.relid, quote_ident(n.nspname) || '.' || quote_ident(c.relname),
		       s.n_tup_del + s.n_tup_upd - s.n_tup_hot_upd, l.files, pg_relation_filenode(l.relid)
		FROM runtime.live_tables() l
		JOIN pg_class c ON c.oid = l.relid JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_stat_user_tables s ON s.relid = l.relid
		LEFT JOIN runtime.table_compaction t ON t.relid = l.relid
		WHERE pg_has_role(c.relowner, 'MEMBER')
		  AND s.n_tup_del + s.n_tup_upd - s.n_tup_hot_upd <> COALESCE(t.changed, 0)
		  AND l.files - l.bytes >= 1048576
		  AND l.files - l.bytes >= $1 * l.files
		ORDER BY l.files - l.bytes DESC`, minShare)
	if err != nil {
		return 0, err
	}
	var tables []table
	for rows.Next() {
		var t table
		if err := rows.Scan(&t.oid, &t.name, &t.changed, &t.files, &t.filenode); err != nil {
			rows.Close()
			return 0, err
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	// The sweep's VACUUM runs without parallel workers: they share their
	// work through /dev/shm, which a container gets at 64 MB, and a VACUUM
	// of a large table asked for maintenance_work_mem there and failed —
	// on the Playground calc_result was not vacuumed, and the sweep stopped
	// at it, for days (2026-10-25). A table that fails no longer stops the
	// sweep: the others are still compacted, and the first error returned.
	var freed int64
	var firstErr error
	for _, t := range tables {
		stmt := `VACUUM (SKIP_LOCKED, PARALLEL 0) `
		if force {
			stmt = `VACUUM (FULL, SKIP_LOCKED) `
		}
		if _, err := db.Exec(ctx, stmt+t.name); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("compact %s: %w", t.name, err)
			}
			continue
		}
		// A rewritten table has a new file; a skipped one is tried again.
		var filenode uint32
		var files int64
		if err := db.QueryRow(ctx, `SELECT pg_relation_filenode($1), pg_total_relation_size($1)`, t.oid).Scan(&filenode, &files); err != nil {
			return freed, err
		}
		if files < t.files {
			freed += t.files - files
		}
		// Only a rewrite is recorded: a vacuumed table is still one a
		// forced compaction rewrites (a delete's, straight after the sweep
		// its re-measure runs), and the sweep vacuums it again while it
		// qualifies — nearly free when nothing changed since.
		if !force || filenode == t.filenode {
			continue
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO runtime.table_compaction (relid, changed) VALUES ($1, $2)
			ON CONFLICT (relid) DO UPDATE SET changed = EXCLUDED.changed`, t.oid, t.changed); err != nil {
			return freed, err
		}
	}
	return freed, firstErr
}

// CompactTenant is Compact for customerID's own database, and nothing in a
// shared one.
func CompactTenant(ctx context.Context, db DB, customerID string, force bool) (int64, error) {
	dedicated, err := dedicatedTo(ctx, db, customerID)
	if err != nil || !dedicated {
		return 0, err
	}
	freed, err := Compact(ctx, db, force)
	if err != nil {
		return freed, fmt.Errorf("compact tenant %s: %w", customerID, err)
	}
	return freed, nil
}
