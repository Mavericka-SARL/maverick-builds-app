package plan

import (
	"context"
	"fmt"
)

// EnsureRevisionParts makes the parts of every revision that lacks some
// (runtime.revisions_without_parts, migration 177): a revision's results
// are kept in its own part, made outside the transaction that created the
// revision, and until then sit in the table's default part. The
// calculation makes a revision's parts before its pass; this catches the
// rest. A revision whose parts could not take their locks within two
// seconds is tried again at the next compaction. Returns the revisions
// still without their parts.
func EnsureRevisionParts(ctx context.Context, db DB) (int, error) {
	var ready bool
	if err := db.QueryRow(ctx, `SELECT to_regprocedure('runtime.revisions_without_parts()') IS NOT NULL`).Scan(&ready); err != nil || !ready {
		return 0, err
	}
	rows, err := db.Query(ctx, `SELECT r::text FROM runtime.revisions_without_parts() r`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	missing := 0
	var firstErr error
	for _, id := range ids {
		var done bool
		if err := db.QueryRow(ctx, `SELECT runtime.add_revision_parts($1::uuid)`, id).Scan(&done); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("parts of revision %s: %w", id, err)
			}
			missing++
			continue
		}
		if !done {
			missing++
		}
	}
	return missing, firstErr
}

// DropGoneRevisionParts drops the parts deleted revisions left in the tables
// kept one part per revision (runtime.calc_result, migration 176). Deleting
// a revision empties its parts in its own transaction (TRUNCATE, which gives
// the disk back at once); the empty part itself is dropped here, after the
// delete has committed, because a drop locks the whole table for a moment:
// runtime.drop_revision_part waits a second at most, and a part it could not
// drop is tried again at the next compaction. A part whose revision's delete
// could not empty it (another session held it) goes here with its rows.
//
// Every database, shared or a tenant's own: a deleted revision's parts hold
// nothing anyone else needs. A database whose migrations predate 176 has
// nothing to drop. Returns the bytes the dropped parts held.
func DropGoneRevisionParts(ctx context.Context, db DB) (int64, error) {
	var ready bool
	if err := db.QueryRow(ctx, `SELECT to_regprocedure('runtime.drop_revision_part(regclass)') IS NOT NULL`).Scan(&ready); err != nil || !ready {
		return 0, err
	}
	rows, err := db.Query(ctx, `SELECT part::text, pg_total_relation_size(part) FROM runtime.gone_revision_parts()`)
	if err != nil {
		return 0, err
	}
	type part struct {
		name  string
		bytes int64
	}
	var parts []part
	for rows.Next() {
		var p part
		if err := rows.Scan(&p.name, &p.bytes); err != nil {
			rows.Close()
			return 0, err
		}
		parts = append(parts, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	// One statement per part: each drop holds the table's lock only for
	// itself, never for the parts dropped after it.
	var freed int64
	var firstErr error
	for _, p := range parts {
		var dropped bool
		if err := db.QueryRow(ctx, `SELECT runtime.drop_revision_part(to_regclass($1))`, p.name).Scan(&dropped); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("drop %s: %w", p.name, err)
			}
			continue
		}
		if dropped {
			freed += p.bytes
		}
	}
	return freed, firstErr
}
