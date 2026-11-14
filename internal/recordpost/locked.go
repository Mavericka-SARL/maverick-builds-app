package recordpost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// A period a workflow locks keeps the values posted into it: a record's
// value does not leave a locked cell, whether the record is changed, its
// status moves, its posting is changed or deleted, or its table is deleted.
// Until 2026-11-13 the lock kept values from being written into a locked
// cell but not from being taken out of one: a record moved to October, or a
// posting deleted and created again, withdrew September's released figures
// with nothing refused. The checks here read the locks as the grid draws
// them (writeguard.CellLocks): a workflow run awaiting approval or approved
// over the cell's members, or an approval of the whole revision waiting for
// its decision. A closed rule (metricformula.ClosedRule) is about input, not
// about what was approved, and does not keep a value in.

// lockedCell says why cell, where mapping i of tp holds a posting, is
// locked, or "" when it is not. The locks are read once per mapping, and
// only once a posting would leave a cell.
func (ps *Poster) lockedCell(ctx context.Context, tp *tablePosting, i int, cell map[string]string) (string, error) {
	cl, ok := tp.cellLocks[i]
	if !ok {
		m := tp.mappings[i]
		if err := RetryTransient(ctx, 3, func() error {
			var e error
			cl, e = writeguard.LoadCellLocks(ctx, ps.db(ctx), m.modelID, tp.revisions[i], m.targetMetricID)
			return e
		}); err != nil {
			return "", err
		}
		tp.cellLocks[i] = cl
	}
	return cl.Reason(cell), nil
}

// leavingRefusal is the refusal of a record change that takes the record's
// value out of the cell p it posts in now through mapping i — moved by a
// change of its values, or withdrawn by a status it does not post in —
// when a workflow locks that cell.
func (ps *Poster) leavingRefusal(ctx context.Context, tp *tablePosting, i int, p *plannedPosting) (int, string, error) {
	reason, err := ps.lockedCell(ctx, tp, i, p.members)
	if err != nil || reason == "" {
		return 0, "", err
	}
	return http.StatusForbidden, "this record's value is in a cell that cannot change: " + reason, nil
}

// postedNow is what recs post through m now, by record id: their posting
// rows.
func (ps *Poster) postedNow(ctx context.Context, m formMapping, recs []postingRecord) (map[string]*plannedPosting, error) {
	ids := make([]string, len(recs))
	for j, r := range recs {
		ids[j] = r.id
	}
	rows, err := ps.db(ctx).Query(ctx, `
		SELECT form_record_id::text, dim_members, COALESCE(posted_value, 0)::float8
		FROM runtime.form_record_posting WHERE mapping_id = $1::uuid AND form_record_id = ANY($2::uuid[])`, m.id, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*plannedPosting{}
	for rows.Next() {
		var id string
		var raw []byte
		p := &plannedPosting{}
		if err := rows.Scan(&id, &raw, &p.value); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &p.members)
		out[id] = p
	}
	return out, rows.Err()
}

// MappingChange is what a posting (model.form_metric_mapping) is changed
// to.
type MappingChange struct {
	TargetMetricID    string
	SourceField       string
	Aggregation       string
	PostingStatuses   []string
	DimensionMappings map[string]string
	RowFilter         []crudapp.RowCondition
	Live              bool
}

// MappingChangeRefusal says why changing posting mappingID to `to` may not
// go ahead — it would change values the posting holds in cells a workflow
// locks — or "" when it may. Another metric takes every value out of the
// metric it is in, and puts them into the other one's cells; another
// aggregation adds the records up again; a change of the value field, the
// dimensions, the posting statuses or the row filter moves or withdraws the
// postings of the records it reaches, each compared with what the record
// posts after the change. A posting switched off posts and withdraws
// nothing. db is a pool, or the transaction an AI Developer proposal is
// checked in.
func MappingChangeRefusal(ctx context.Context, db dbx.DB, mappingID string, to MappingChange) (string, error) {
	var cur MappingChange
	var formID, modelID, revisionID, name string
	var dimRaw, filterRaw []byte
	err := db.QueryRow(ctx, `
		SELECT m.form_id::text, m.model_id::text, COALESCE(m.revision_id::text, ''), m.name,
		       COALESCE(m.target_metric_id::text, ''), COALESCE(m.source_field, ''), COALESCE(m.aggregation, ''),
		       COALESCE(m.posting_statuses, '{}'), COALESCE(m.dimension_mappings, '{}'::jsonb), m.live_posting, m.row_filter
		FROM model.form_metric_mapping m WHERE m.id = $1::uuid`, mappingID).Scan(
		&formID, &modelID, &revisionID, &name, &cur.TargetMetricID, &cur.SourceField, &cur.Aggregation,
		&cur.PostingStatuses, &dimRaw, &cur.Live, &filterRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the posting: %w", err)
	}
	_ = json.Unmarshal(dimRaw, &cur.DimensionMappings)
	_ = json.Unmarshal(filterRaw, &cur.RowFilter)
	if !to.Live {
		return "", nil
	}
	if to.TargetMetricID != cur.TargetMetricID {
		n, reason, err := writeguard.LockedPosted(ctx, db, []string{mappingID}, to.TargetMetricID)
		if err != nil || n > 0 {
			return lockedRefusal(fmt.Sprintf("Moving the posting “%s” to another metric", name), n, reason), err
		}
		// Its records' postings are summed into the new metric: one whose
		// cell there is locked would be written into it.
		n, reason, err = postingsIn(ctx, db, mappingID, modelID, revisionID, to.TargetMetricID)
		if err != nil || n > 0 {
			return lockedRefusal(fmt.Sprintf("Moving the posting “%s” to another metric", name), n, reason), err
		}
	}
	if aggregationOf(to.Aggregation) != aggregationOf(cur.Aggregation) {
		n, reason, err := writeguard.LockedPosted(ctx, db, []string{mappingID}, "")
		if err != nil || n > 0 {
			return lockedRefusal(fmt.Sprintf("Changing how the posting “%s” adds up its records", name), n, reason), err
		}
	}
	if cur.Live && to.SourceField == cur.SourceField && slices.Equal(to.PostingStatuses, cur.PostingStatuses) &&
		maps.Equal(to.DimensionMappings, cur.DimensionMappings) && sameRowFilter(to.RowFilter, cur.RowFilter) {
		return "", nil
	}
	if revisionID == "" {
		revisionID = activeRevision(ctx, db, modelID)
	}
	cl, err := writeguard.LoadCellLocks(ctx, db, modelID, revisionID, to.TargetMetricID)
	if err != nil || cl.None() {
		return "", err
	}
	form, err := crudapp.NewStoreOn(db).GetForm(ctx, formID)
	if err != nil {
		return "", err
	}
	rows, err := db.Query(ctx, `
		SELECT p.dim_members, COALESCE(p.posted_value, 0)::float8, COALESCE(r.status::text, ''), r.data
		FROM runtime.form_record_posting p JOIN runtime.form_record r ON r.id = p.form_record_id
		WHERE p.mapping_id = $1::uuid`, mappingID)
	if err != nil {
		return "", fmt.Errorf("read the posting's records: %w", err)
	}
	type posted struct {
		now    plannedPosting
		status string
		data   map[string]any
	}
	var locked []posted
	reason := ""
	for rows.Next() {
		var p posted
		var dims, data []byte
		if err := rows.Scan(&dims, &p.now.value, &p.status, &data); err != nil {
			rows.Close()
			return "", err
		}
		_ = json.Unmarshal(dims, &p.now.members)
		if r := cl.Reason(p.now.members); r != "" {
			_ = json.Unmarshal(data, &p.data)
			locked = append(locked, p)
			if reason == "" {
				reason = r
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(locked) == 0 {
		return "", err
	}
	// Each record as the changed posting would post it, once the rows are
	// read: on a transaction one statement runs at a time.
	m := formMapping{
		id: mappingID, sourceField: to.SourceField, targetMetricID: to.TargetMetricID, aggregation: to.Aggregation,
		rowFilter: to.RowFilter, postingStatuses: to.PostingStatuses, dimMappings: to.DimensionMappings, modelID: modelID,
	}
	period, hasStatus := periodResolverOn(ctx, db), form.HasStatus()
	moved := 0
	for _, p := range locked {
		after := m.planned(hasStatus, p.status, p.data, false, period)
		if after != nil && len(m.emptyDims(p.data)) > 0 {
			after = nil
		}
		if !samePosting(&p.now, after) {
			moved++
		}
	}
	if moved > 0 {
		return lockedRefusal(fmt.Sprintf("Changing the posting “%s”", name), moved, reason), nil
	}
	return "", nil
}

// postingsIn counts mappingID's records whose posting cell is locked in
// metricID, and says why the first is.
func postingsIn(ctx context.Context, db dbx.DB, mappingID, modelID, revisionID, metricID string) (int, string, error) {
	if revisionID == "" {
		revisionID = activeRevision(ctx, db, modelID)
	}
	cl, err := writeguard.LoadCellLocks(ctx, db, modelID, revisionID, metricID)
	if err != nil || cl.None() {
		return 0, "", err
	}
	rows, err := db.Query(ctx, `SELECT DISTINCT dim_members::text FROM runtime.form_record_posting WHERE mapping_id = $1::uuid`, mappingID)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	n, reason := 0, ""
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return 0, "", err
		}
		var cell map[string]string
		_ = json.Unmarshal([]byte(raw), &cell)
		if r := cl.Reason(cell); r != "" {
			n++
			if reason == "" {
				reason = r
			}
		}
	}
	return n, reason, rows.Err()
}

// activeRevision is the revision a revision-global (legacy) posting posts
// into, as postingRevision reads it.
func activeRevision(ctx context.Context, db dbx.DB, modelID string) string {
	var rev string
	_ = db.QueryRow(ctx, `
		SELECT COALESCE(
			m.active_revision_id::text,
			(SELECT s.id::text FROM model.revision s WHERE s.model_id=m.id ORDER BY s.created_at DESC LIMIT 1)
		) FROM core.model m WHERE m.id=$1::uuid`, modelID).Scan(&rev)
	return rev
}

// aggregationOf is an aggregation as RecomputeFactInput reads it: none is
// a sum.
func aggregationOf(a string) string {
	if a == "" {
		return "sum"
	}
	return a
}

func sameRowFilter(a, b []crudapp.RowCondition) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return string(ja) == string(jb)
}

func lockedRefusal(change string, n int, reason string) string {
	if n == 0 {
		return ""
	}
	return writeguard.LockedPostedRefusal(change, n, reason)
}

// LockedPostingsRefusal is the refusal of deleting the postings
// mappingIDs — one posting, or every posting of a table being deleted —
// when they hold values in cells a workflow locks; "" when they hold none.
// change names what is deleted ("Deleting the posting “X”").
func LockedPostingsRefusal(ctx context.Context, db dbx.DB, mappingIDs []string, change string) (string, error) {
	n, reason, err := writeguard.LockedPosted(ctx, db, mappingIDs, "")
	if err != nil {
		return "", err
	}
	return lockedRefusal(change, n, reason), nil
}

// PostingDeleteRefusal is LockedPostingsRefusal for deleting posting
// mappingID.
func PostingDeleteRefusal(ctx context.Context, db dbx.DB, mappingID string) (string, error) {
	var name string
	if err := db.QueryRow(ctx, `SELECT name FROM model.form_metric_mapping WHERE id = $1::uuid`, mappingID).Scan(&name); errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return LockedPostingsRefusal(ctx, db, []string{mappingID}, fmt.Sprintf("Deleting the posting “%s”", name))
}

// TableDeleteRefusal is LockedPostingsRefusal for deleting table formID
// with every posting of its records.
func TableDeleteRefusal(ctx context.Context, db dbx.DB, formID string) (string, error) {
	var label string
	var ids []string
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(f.label, ''), f.name),
		       COALESCE(array_agg(m.id::text) FILTER (WHERE m.id IS NOT NULL), '{}')
		FROM model.form_def f LEFT JOIN model.form_metric_mapping m ON m.form_id = f.id
		WHERE f.id = $1::uuid GROUP BY f.id`, formID).Scan(&label, &ids); errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return LockedPostingsRefusal(ctx, db, ids, fmt.Sprintf("Deleting the table “%s”", label))
}
