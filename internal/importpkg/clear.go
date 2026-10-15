package importpkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// Clearing typed values in bulk: a metric's, or a grid's input metrics',
// across the revision or under chosen members. The only bulk clear was a
// full reload, which empties the whole revision; anything narrower went cell
// by cell (a tester asking the AI Developer to clear test data got a
// full-reload integration that never ran, 2026-10-15).

// ErrNothingToClear is a clear that names no input metric.
var ErrNothingToClear = errors.New("no input metric to clear")

// ClearRequest is what to clear.
type ClearRequest struct {
	ModelID, RevisionID, UserID string
	// MetricIDs are the input metrics to clear (a calculated metric holds no
	// typed values and is refused).
	MetricIDs []string
	// Members limits the clear: dimension id → member codes. A cell is
	// cleared when, on each listed dimension, its member is one of the codes
	// or under one of them. Empty clears every cell of the metrics.
	Members map[string][]string
	// DryRun counts what would be cleared and clears nothing.
	DryRun bool
}

// ClearResult is what a clear did (or, dry, would do).
type ClearResult struct {
	// Cells cleared: their typed values deleted, kept in the cells' history
	// with the reason "cleared".
	Cells int `json:"cells"`
	// Withheld counts cells left because the caller may not write them
	// (a hidden or read-only member).
	Withheld int `json:"withheld"`
	// Posted counts cells left because a table's records post their values:
	// they are the records', which would post them again.
	Posted int `json:"posted"`
	// MetricIDs are the metrics cleared, for the recalculation that follows.
	MetricIDs []string `json:"-"`
}

// ClearValues deletes the typed values req names that req.UserID may write,
// in one transaction. It refuses a metric the caller may not write, a
// system-managed revision, and cells a running approval has locked.
func ClearValues(ctx context.Context, pool *pgxpool.Pool, req ClearRequest) (*ClearResult, error) {
	if len(req.MetricIDs) == 0 {
		return nil, ErrNothingToClear
	}
	if sm, err := writeguard.SystemManaged(ctx, pool, req.RevisionID); err != nil {
		return nil, err
	} else if sm {
		return nil, fmt.Errorf("%w: this revision is system-managed and read-only", ErrWriteDenied)
	}
	for _, id := range req.MetricIDs {
		var input bool
		if err := pool.QueryRow(ctx, `
			SELECT is_input FROM model.metric_def
			WHERE id::text = $1 AND model_id = $2::uuid AND (revision_id = $3::uuid OR revision_id IS NULL)`,
			id, req.ModelID, req.RevisionID).Scan(&input); err != nil {
			return nil, fmt.Errorf("metric %s is not in this model's revision", id)
		}
		if !input {
			return nil, fmt.Errorf("metric %s is calculated: it holds no typed values to clear", id)
		}
		access, err := writeguard.MetricAccess(ctx, pool, req.UserID, id)
		if err != nil {
			return nil, err
		}
		if access == "hidden" || access == "read" {
			return nil, fmt.Errorf("%w: a metric to clear is outside your access scope", ErrWriteDenied)
		}
	}

	// The cells to clear, as one condition: these metrics, typed (not
	// posted from records), under the chosen members.
	args := []any{req.ModelID, req.RevisionID, req.MetricIDs}
	where := `fi.model_id = $1::uuid AND fi.revision_id = $2::uuid AND fi.metric_id::text = ANY($3::text[])`
	dims := make([]string, 0, len(req.Members))
	for d := range req.Members {
		dims = append(dims, d)
	}
	sort.Strings(dims)
	for _, dimID := range dims {
		codes, err := codesUnder(ctx, pool, dimID, req.Members[dimID])
		if err != nil {
			return nil, err
		}
		args = append(args, dimID, codes)
		where += fmt.Sprintf(` AND fi.dim_members->>$%d = ANY($%d::text[])`, len(args)-1, len(args))
	}
	// What the caller may not write: hidden and read-only members (and what
	// is under them), as a full reload spares them.
	restrictedMembers, _, err := restrictedIDs(ctx, pool, req.ModelID, req.RevisionID, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("resolve restricted members: %w", err)
	}
	args = append(args, restrictedMembers)
	writable := fmt.Sprintf(`NOT EXISTS (
		SELECT 1 FROM jsonb_each_text(fi.dim_members) AS dm(dim_id, code)
		JOIN model.dimension_member m ON m.dimension_id::text = dm.dim_id AND m.code = dm.code
		WHERE m.id::text = ANY($%d::text[]))`, len(args))

	out := &ClearResult{MetricIDs: req.MetricIDs}
	if err := pool.QueryRow(ctx, `
		SELECT count(DISTINCT (fi.metric_id, fi.dim_members)) FILTER (WHERE fi.source_ref IS NULL AND `+writable+`),
		       count(DISTINCT (fi.metric_id, fi.dim_members)) FILTER (WHERE fi.source_ref IS NULL AND NOT `+writable+`),
		       count(DISTINCT (fi.metric_id, fi.dim_members)) FILTER (WHERE fi.source_ref IS NOT NULL)
		FROM runtime.fact_input fi WHERE `+where, args...).Scan(&out.Cells, &out.Withheld, &out.Posted); err != nil {
		return nil, fmt.Errorf("count the cells to clear: %w", err)
	}
	if out.Cells == 0 || req.DryRun {
		return out, nil
	}

	// A running approval's lock covers the members its cells name.
	memberIDs, err := clearedMemberIDs(ctx, pool, where+` AND fi.source_ref IS NULL AND `+writable, args)
	if err != nil {
		return nil, err
	}
	if reason, err := writeguard.CheckWriteMetrics(ctx, pool, req.ModelID, req.RevisionID, req.UserID, memberIDs, req.MetricIDs); err != nil {
		return nil, fmt.Errorf("write guard: %w", err)
	} else if reason != "" {
		return nil, fmt.Errorf("%w: %s", ErrWriteDenied, reason)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	// The archive trigger keeps every deleted row in the cell's history.
	if _, err := tx.Exec(ctx, `SET LOCAL mvx.delete_reason = 'cleared'`); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM runtime.fact_input fi WHERE `+where+` AND fi.source_ref IS NULL AND `+writable, args...); err != nil {
		return nil, fmt.Errorf("clear: %w", err)
	}
	// Hand the row counts to the statistics now, so the storage re-measure
	// that follows sees them gone.
	if _, err := tx.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// codesUnder is codes and every member under them in dimID, refusing a
// code the dimension does not have.
func codesUnder(ctx context.Context, pool *pgxpool.Pool, dimID string, codes []string) ([]string, error) {
	if len(codes) == 0 {
		return nil, fmt.Errorf("dimension %s: name at least one member", dimID)
	}
	rows, err := pool.Query(ctx, `
		WITH RECURSIVE under AS (
		    SELECT id, code FROM model.dimension_member WHERE dimension_id::text = $1 AND code = ANY($2::text[])
		    UNION
		    SELECT m.id, m.code FROM model.dimension_member m JOIN under u ON m.parent_member_id = u.id
		)
		SELECT code FROM under`, dimID, codes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]bool{}
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		found[c] = true
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var missing []string
	for _, c := range codes {
		if !found[c] {
			missing = append(missing, strconv.Quote(c))
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("dimension %s has no member %s", dimID, strings.Join(missing, ", "))
	}
	return out, nil
}

// clearedMemberIDs is the ids of the members the cells matching where name.
func clearedMemberIDs(ctx context.Context, pool *pgxpool.Pool, where string, args []any) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT m.id::text
		FROM runtime.fact_input fi
		CROSS JOIN LATERAL jsonb_each_text(fi.dim_members) AS dm(dim_id, code)
		JOIN model.dimension_member m ON m.dimension_id::text = dm.dim_id AND m.code = dm.code
		WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ParseMembers reads a clear's member filter from JSON ({"dimension id":
// ["code", …]}); null or {} is none.
func ParseMembers(raw json.RawMessage) (map[string][]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string][]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf(`members is {"<dimension id>": ["<member code>", …]}`)
	}
	return m, nil
}
