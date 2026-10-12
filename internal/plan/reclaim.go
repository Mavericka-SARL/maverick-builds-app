package plan

import "context"

// reclaim gives back the space deleted rows still hold in a tenant's own
// database, for an edition that limits storage. PostgreSQL keeps a deleted
// row's space in the table's files for later writes to reuse, so the
// database's size — what the limit is judged on — did not fall when data was
// deleted: a workspace over its limit could not get back under it by deleting
// a model. VACUUM FULL rewrites a table without that space. It locks the
// table while it does, which in a database a storage limit keeps small is
// brief, and SKIP_LOCKED leaves a table in use at that moment to the next
// sweep.
//
// It rewrites the tables rows were deleted from since the last time, when the
// workspace is over its limit or those rows are a tenth of what it holds — not
// for the steady trickle of deletes, whose space the next writes reuse anyway.
// What was compacted is remembered per gateway, so a restart compacts once
// more at most. Reports whether a table was rewritten.
func (e *Enforcer) reclaim(ctx context.Context, db DB, customerID string, over bool) (bool, error) {
	type table struct {
		oid           uint32
		name          string
		deleted, live int64
		filenode      uint32
	}
	rows, err := db.Query(ctx, `
		SELECT s.relid, quote_ident(s.schemaname) || '.' || quote_ident(s.relname), s.n_tup_del, s.n_live_tup, pg_relation_filenode(s.relid)
		FROM pg_stat_user_tables s JOIN pg_class c ON c.oid = s.relid
		WHERE c.relkind = 'r'`)
	if err != nil {
		return false, err
	}
	var tables []table
	for rows.Next() {
		var t table
		if err := rows.Scan(&t.oid, &t.name, &t.deleted, &t.live, &t.filenode); err != nil {
			rows.Close()
			return false, err
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}

	e.mu.Lock()
	seen := e.compacted[customerID]
	e.mu.Unlock()
	var dirty []table
	var since, live int64
	for _, t := range tables {
		live += t.live
		d := t.deleted - seen[t.oid]
		if d < 0 { // statistics were reset
			d = t.deleted
		}
		if d > 0 {
			dirty = append(dirty, t)
			since += d
		}
	}
	if len(dirty) == 0 || (!over && since*10 < live) {
		return false, nil
	}

	next := make(map[uint32]int64, len(seen)+len(dirty))
	for oid, n := range seen {
		next[oid] = n
	}
	rewrote := false
	for _, t := range dirty {
		if _, err := db.Exec(ctx, `VACUUM (FULL, SKIP_LOCKED) `+t.name); err != nil {
			return rewrote, err
		}
		// A rewritten table has a new file; a skipped one keeps its count
		// unseen, so the next sweep tries it again.
		var filenode uint32
		if err := db.QueryRow(ctx, `SELECT pg_relation_filenode($1)`, t.oid).Scan(&filenode); err != nil {
			return rewrote, err
		}
		if filenode != t.filenode {
			next[t.oid] = t.deleted
			rewrote = true
		}
	}
	e.mu.Lock()
	e.compacted[customerID] = next
	e.mu.Unlock()
	return rewrote, nil
}
