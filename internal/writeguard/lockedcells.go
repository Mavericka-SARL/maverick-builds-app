package writeguard

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// ReadDB is what reading locks needs: a pool, or the transaction an AI
// Developer proposal is checked in.
type ReadDB interface {
	Querier
	QueryRower
}

// CellLocks are the locks over one metric of a revision, read once to test
// many cells: each workflow run awaiting approval or approved over some of
// its members (WorkflowLockScopes, as the grid draws them read-only), and a
// revision-wide approval waiting for its decision (RevisionLockReason).
type CellLocks struct {
	scopes   []LockScope
	revision string
}

// LoadCellLocks reads the locks over metricID in revisionID of modelID.
func LoadCellLocks(ctx context.Context, db ReadDB, modelID, revisionID, metricID string) (*CellLocks, error) {
	var name string
	if err := db.QueryRow(ctx, `SELECT name FROM model.metric_def WHERE id = $1::uuid`, metricID).Scan(&name); errors.Is(err, pgx.ErrNoRows) {
		return &CellLocks{}, nil
	} else if err != nil {
		return nil, fmt.Errorf("read the locked metric: %w", err)
	}
	scopes, err := WorkflowLockScopes(ctx, db, modelID, []LockMetric{{ID: metricID, Name: name}})
	if err != nil {
		return nil, fmt.Errorf("read workflow locks: %w", err)
	}
	c := &CellLocks{scopes: scopes}
	locked, reason, err := RevisionLockReason(ctx, db, modelID, revisionID, []string{metricID})
	if err != nil {
		return nil, fmt.Errorf("read the revision's approval: %w", err)
	}
	if locked {
		c.revision = reason
	}
	return c, nil
}

// None reports that no cell of the metric is locked.
func (c *CellLocks) None() bool {
	return c == nil || len(c.scopes) == 0 && c.revision == ""
}

// Reason says why cell (dimension id → member code) is locked — "Sep 2026
// is locked: …" — or "" when it is not.
func (c *CellLocks) Reason(cell map[string]string) string {
	if c == nil {
		return ""
	}
	if c.revision != "" {
		return c.revision
	}
	for _, s := range c.scopes {
		if s.Covers(cell) {
			return s.Reason
		}
	}
	return ""
}

// Covers reports whether cell (dimension id → member code) is inside s:
// for every field whose dimensions the cell has, its member on one of them
// is covered, and at least one field's are in the cell — the grid's
// cellLock (PlanningGrid.tsx).
func (s LockScope) Covers(cell map[string]string) bool {
	present := false
	for _, field := range s.Fields {
		fieldPresent, match := false, false
		for dimID, codes := range field {
			code, ok := cell[dimID]
			if !ok {
				continue
			}
			fieldPresent = true
			if slices.Contains(codes, code) {
				match = true
			}
		}
		if !fieldPresent {
			continue
		}
		present = true
		if !match {
			return false
		}
	}
	return present
}

// LockedPosted counts the values a table's postings hold in locked cells —
// runtime.fact_input rows whose source_ref is one of sourceRefs (posting
// ids) — and says why the first is locked: what deleting or changing those
// postings would take out of a period a workflow froze. exceptMetricID
// leaves that metric's values out (a posting moved to it keeps them).
//
// A developer's delete of a posting or of its table, and a retarget, used to
// withdraw a released period's values with nothing refused: the lock kept
// records from changing them, not the posting behind them (2026-11-13).
func LockedPosted(ctx context.Context, db ReadDB, sourceRefs []string, exceptMetricID string) (n int, reason string, err error) {
	if len(sourceRefs) == 0 {
		return 0, "", nil
	}
	rows, err := db.Query(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text, ''), metric_id::text, dim_members
		FROM runtime.fact_input
		WHERE source_ref = ANY($1::uuid[]) AND ($2 = '' OR metric_id <> NULLIF($2, '')::uuid)
		ORDER BY revision_id, metric_id, dim_members::text`, sourceRefs, exceptMetricID)
	if err != nil {
		return 0, "", fmt.Errorf("read posted values: %w", err)
	}
	type posted struct {
		key  [3]string // model, revision, metric
		cell map[string]string
	}
	var all []posted
	for rows.Next() {
		var p posted
		if err := rows.Scan(&p.key[0], &p.key[1], &p.key[2], &p.cell); err != nil {
			rows.Close()
			return 0, "", err
		}
		all = append(all, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	// Read after the rows are closed: on a transaction one statement runs
	// at a time.
	locks := map[[3]string]*CellLocks{}
	for _, p := range all {
		cl, ok := locks[p.key]
		if !ok {
			if cl, err = LoadCellLocks(ctx, db, p.key[0], p.key[1], p.key[2]); err != nil {
				return 0, "", err
			}
			locks[p.key] = cl
		}
		if r := cl.Reason(p.cell); r != "" {
			n++
			if reason == "" {
				reason = r
			}
		}
	}
	return n, reason, nil
}

// LockedPostedRefusal is the refusal of change (e.g. "Deleting the posting
// X") when it would change n posted values in locked cells for reason.
func LockedPostedRefusal(change string, n int, reason string) string {
	values := "1 posted value"
	if n != 1 {
		values = fmt.Sprintf("%d posted values", n)
	}
	return fmt.Sprintf("%s would change %s in a locked period (%s). Reopen the run that locks it first.", change, values, reason)
}
