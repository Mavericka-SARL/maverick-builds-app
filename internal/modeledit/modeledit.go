// Package modeledit holds the model edits whose side effects on stored data
// must be the same whichever door they come through — the developer
// console's endpoints and the AI Developer's tools.
//
// Each function here used to live in internal/gateway, where the AI
// Developer (internal/aiassistant, which gateway imports) could not call it.
// The assistant then either had no tool for the edit or wrote its own copy
// without the side effects: deleting a member without moving its facts to
// history, renaming a code without re-keying the facts filed under it, giving
// a leaf its first child without moving the leaf's values down to it. A
// validator shared through internal/metricformula stops the two paths
// disagreeing about what is allowed; this package stops them disagreeing
// about what an allowed edit does.
//
// Recalculation, audit and HTTP status codes stay with the callers: the
// console recalculates after each edit, while the assistant's edits land in
// a draft revision that is recalculated when it is promoted.
package modeledit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mavericks-engine/mavericks/internal/timedim"
)

// DB is what the edits need. The gateway's tenant handle and a pgxpool.Pool
// both satisfy it.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Affected is one (revision, metric) pair whose stored inputs an edit moved,
// for the caller to recalculate.
type Affected = struct{ RevisionID, MetricID string }

// MemberPeriod checks a member's period dates against its dimension. isTime
// reports a time dimension; dated a leaf period (both dates) as opposed to an
// aggregate period such as H1 or FY26 (no dates). Dates on a standard
// dimension's member are refused.
func MemberPeriod(ctx context.Context, q timedim.Querier, dimID, start, end string) (p timedim.Period, isTime, dated bool, err error) {
	cfg, err := timedim.LoadConfig(ctx, q, dimID)
	if err != nil {
		return timedim.Period{}, false, false, fmt.Errorf("dimension not found")
	}
	if cfg.Type != timedim.TypeTime {
		if start != "" || end != "" {
			return timedim.Period{}, false, false, &timedim.Error{Code: timedim.CodeInvalidTimeMember,
				Message: "period_start/period_end apply only to a time dimension's members"}
		}
		return timedim.Period{}, false, false, nil
	}
	if start == "" && end == "" {
		return timedim.Period{}, true, false, nil // aggregate period
	}
	if start == "" || end == "" {
		return timedim.Period{}, true, true, &timedim.Error{Code: timedim.CodeInvalidTimeMember,
			Message: "a leaf period needs both period_start and period_end (leave both empty for an aggregate period such as H1 or FY26)"}
	}
	ps, err := timedim.ParseDate(start)
	if err != nil {
		return timedim.Period{}, true, true, err
	}
	pe, err := timedim.ParseDate(end)
	if err != nil {
		return timedim.Period{}, true, true, err
	}
	p = timedim.Period{Start: ps, End: pe}
	if err := timedim.ValidatePeriod(cfg, p); err != nil {
		return timedim.Period{}, true, true, err
	}
	return p, true, true, nil
}

// WriteTimeMember inserts (memberID == "") or updates one time member — a
// dated leaf period (dated=true) or an aggregate period (no dates, no
// ordinal) — and re-validates and re-indexes the whole dimension in the same
// transaction, so an invalid period rolls the write back.
func WriteTimeMember(ctx context.Context, db DB, dimID, memberID, code, label string, p timedim.Period, dated bool, parentID *string) (string, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	var start, end *time.Time
	var idx *int
	if dated {
		start, end = &p.Start, &p.End
		zero := 0
		idx = &zero
	}
	id := memberID
	if memberID == "" {
		if err := tx.QueryRow(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, parent_member_id)
			VALUES ($1::uuid, $2, $3, $4::date, $5::date, $6, $7::uuid) RETURNING id::text
		`, dimID, code, label, start, end, idx, parentID).Scan(&id); err != nil {
			return "", err
		}
	} else if _, err := tx.Exec(ctx, `
		UPDATE model.dimension_member SET code=$2, label=$3, period_start=$4::date, period_end=$5::date, time_index=$6, parent_member_id=$7::uuid
		WHERE id=$1::uuid AND dimension_id=$8::uuid
	`, memberID, code, label, start, end, idx, parentID, dimID); err != nil {
		return "", err
	}
	if err := timedim.ValidateAndReindex(ctx, tx, dimID); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

// PeriodLabel is the label a generated period gets.
func PeriodLabel(granularity string, p timedim.Period) string {
	switch granularity {
	case timedim.GranMonth:
		return p.Start.Format("Jan 2006")
	case timedim.GranDay, timedim.GranWeek:
		return p.Start.Format("2 Jan 2006")
	default:
		return p.Code
	}
}

// InsertPeriods adds generated periods to a time dimension inside tx —
// codes the dimension already has are skipped — and re-validates and
// re-indexes the dimension. It returns how many were added.
func InsertPeriods(ctx context.Context, tx pgx.Tx, dimID, granularity string, periods []timedim.Period, parentID *string) (int, error) {
	created := 0
	for _, p := range periods {
		tag, err := tx.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, parent_member_id)
			VALUES ($1::uuid, $2, $3, $4::date, $5::date, 0, $6::uuid)
			ON CONFLICT (dimension_id, code) DO NOTHING
		`, dimID, p.Code, PeriodLabel(granularity, p), p.Start, p.End, parentID)
		if err != nil {
			return 0, err
		}
		created += int(tag.RowsAffected())
	}
	if err := timedim.ValidateAndReindex(ctx, tx, dimID); err != nil {
		return 0, err
	}
	return created, nil
}

// DeleteMember deletes a member and, for a time dimension, closes the gap in
// time_index so positions stay dense from zero.
func DeleteMember(ctx context.Context, db DB, dimID, memberID string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	// The member's input facts go with it, kept in fact_input_history with
	// the reason 'member_deleted' (the re-parent path does the same). Left
	// behind, they were still served as cells for a code no row shows and
	// counted by every reader that sums fact rows. dim_members is keyed by
	// dimension ID and a dimension row belongs to one revision, so only the
	// member's own revision is touched. A parent's children are not deleted
	// (ON DELETE SET NULL), so their facts stay.
	var code, modelID string
	err = tx.QueryRow(ctx, `
		SELECT m.code, d.model_id::text
		FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
		WHERE m.id=$1::uuid AND m.dimension_id=$2::uuid
	`, memberID, dimID).Scan(&code, &modelID)
	if err == nil {
		// A pick-list cell holding the member would name nothing.
		if perr := CheckMemberDeletable(ctx, tx, dimID, code); perr != nil {
			return perr
		}
	}
	switch {
	case err == nil:
		filter, _ := json.Marshal(map[string]string{dimID: code})
		if _, err := tx.Exec(ctx, `SET LOCAL mvx.delete_reason = 'member_deleted'`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM runtime.fact_input WHERE model_id=$1::uuid AND dim_members @> $2::jsonb
		`, modelID, string(filter)); err != nil {
			return err
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM model.dimension_member WHERE id=$1::uuid AND dimension_id=$2::uuid`, memberID, dimID); err != nil {
		return err
	}
	cfg, err := timedim.LoadConfig(ctx, tx, dimID)
	if err != nil {
		return err
	}
	if cfg.Type == timedim.TypeTime {
		// Removing a period may open a gap in a regular calendar; the
		// remaining members still reindex densely so time functions keep a
		// consistent order. The gap itself is reported when the next member
		// is written.
		if _, err := tx.Exec(ctx, `
			WITH ordered AS (
				SELECT id, row_number() OVER (ORDER BY period_start, period_end, code) - 1 AS idx
				FROM model.dimension_member WHERE dimension_id=$1::uuid AND period_start IS NOT NULL
			)
			UPDATE model.dimension_member m SET time_index = o.idx FROM ordered o
			WHERE o.id = m.id AND m.time_index IS DISTINCT FROM o.idx
		`, dimID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RekeyMemberCode carries a member's code change into everything that files
// data under the code rather than the member id. runtime.fact_input and
// calc_result key dim_members by {dimension_id: member CODE}: a rename left
// alone would orphan every existing row under the old code — not deleted,
// just unreachable by any current-code query. widget_props holds member codes
// in three places (SYNC-02: facts were re-keyed but widgets kept the old
// code, so a chart's saved context or a pinned KPI fell back to defaults).
// Paths are keyed by dimension id, so no other model's rows can match.
//
// Each statement is attempted; the errors of those that failed are joined.
// The rename itself is not undone by a failure here.
func RekeyMemberCode(ctx context.Context, db DB, dimID, oldCode, newCode string) error {
	if newCode == "" || newCode == oldCode {
		return nil
	}
	var errs []error
	for _, s := range []struct{ q, tag string }{
		{`UPDATE runtime.fact_input
		  SET dim_members = jsonb_set(dim_members, ARRAY[$1::text], to_jsonb($2::text))
		  WHERE dim_members->>$1 = $3`, "fact_input"},
		{`UPDATE runtime.calc_result
		  SET dim_members = jsonb_set(dim_members, ARRAY[$1::text], to_jsonb($2::text))
		  WHERE dim_members->>$1 = $3`, "calc_result"},
		{`UPDATE model.dashboard_widget
		  SET widget_props = jsonb_set(widget_props, ARRAY['default_view','filter_sel',$1::text], to_jsonb($2::text))
		  WHERE widget_props #>> ARRAY['default_view','filter_sel',$1::text] = $3`, "widget default_view.filter_sel"},
		{`UPDATE model.dashboard_widget
		  SET widget_props = jsonb_set(widget_props, ARRAY['chart','context_defaults',$1::text], to_jsonb($2::text))
		  WHERE widget_props #>> ARRAY['chart','context_defaults',$1::text] = $3`, "widget chart.context_defaults"},
		{`UPDATE model.dashboard_widget
		  SET widget_props = jsonb_set(widget_props, ARRAY['kpi_scope','member_code'], to_jsonb($2::text))
		  WHERE widget_props->'kpi_scope'->>'dimension_id' = $1 AND widget_props->'kpi_scope'->>'member_code' = $3`, "widget kpi_scope"},
	} {
		if _, err := db.Exec(ctx, s.q, dimID, newCode, oldCode); err != nil {
			errs = append(errs, fmt.Errorf("re-key %s: %w", s.tag, err))
		}
	}
	// Pick-list cells holding the member store its code's key.
	if err := rekeyPicklistValues(ctx, db, dimID, oldCode, newCode); err != nil {
		errs = append(errs, err)
	}
	// The dimension's calculated members name it in their formulas.
	if err := RewriteMemberFormulas(ctx, db, dimID, oldCode, newCode); err != nil {
		errs = append(errs, fmt.Errorf("re-key calculated member formulas: %w", err))
	}
	return errors.Join(errs...)
}

// SplitParentFacts moves existing fact_input rows from a parent member to its
// first child when that parent gains its very first child. The parent's
// stored values transfer in full to the sole child so the aggregate (rollup)
// remains identical. It returns the distinct (revision_id, metric_id) pairs
// that were affected so the caller can recalculate.
func SplitParentFacts(ctx context.Context, db DB, dimID, parentMemberID, childCode, modelID string) ([]Affected, error) {
	var parentCode string
	if err := db.QueryRow(ctx,
		`SELECT code FROM model.dimension_member WHERE id=$1::uuid`, parentMemberID,
	).Scan(&parentCode); err != nil {
		return nil, err
	}

	filterBytes, _ := json.Marshal(map[string]string{dimID: parentCode})
	parentFilter := string(filterBytes)

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	_, err = tx.Exec(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by, text_value)
		SELECT model_id, revision_id, metric_id,
		       jsonb_set(dim_members, ARRAY[$1::text], to_jsonb($2::text)),
		       value::float8,
		       entered_by, text_value
		FROM (
			SELECT DISTINCT ON (revision_id, metric_id, dim_members)
			    model_id, revision_id, metric_id, dim_members, value, entered_by, text_value
			FROM runtime.fact_input
			WHERE model_id=$3::uuid AND dim_members @> $4::jsonb
			ORDER BY revision_id, metric_id, dim_members, entered_at DESC, id DESC
		) latest
	`, dimID, childCode, modelID, parentFilter)
	if err != nil {
		return nil, err
	}

	// Collect affected (revision_id, metric_id) pairs before deleting.
	affRows, err := tx.Query(ctx, `
		SELECT DISTINCT revision_id::text, metric_id::text
		FROM runtime.fact_input
		WHERE model_id=$1::uuid AND dim_members @> $2::jsonb AND revision_id IS NOT NULL
	`, modelID, parentFilter)
	if err != nil {
		return nil, err
	}
	var affected []Affected
	for affRows.Next() {
		var r, m string
		if scanErr := affRows.Scan(&r, &m); scanErr == nil {
			affected = append(affected, Affected{RevisionID: r, MetricID: m})
		}
	}
	affRows.Close()

	_, _ = tx.Exec(ctx, `SET LOCAL mvx.delete_reason = 'member_reparented'`)
	_, err = tx.Exec(ctx, `
		DELETE FROM runtime.fact_input
		WHERE model_id=$1::uuid AND dim_members @> $2::jsonb
	`, modelID, parentFilter)
	if err != nil {
		return nil, err
	}

	return affected, tx.Commit(ctx)
}

// SplitIfFirstChild runs SplitParentFacts when parentMemberID has exactly one
// child — the member just placed under it. It reports whether a split ran.
func SplitIfFirstChild(ctx context.Context, db DB, dimID, parentMemberID, childCode string) ([]Affected, string, bool, error) {
	var childCount int
	_ = db.QueryRow(ctx,
		`SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid AND parent_member_id=$2::uuid`,
		dimID, parentMemberID).Scan(&childCount)
	if childCount != 1 {
		return nil, "", false, nil
	}
	var modelID string
	_ = db.QueryRow(ctx, `SELECT model_id::text FROM model.dimension_def WHERE id=$1::uuid`, dimID).Scan(&modelID)
	affected, err := SplitParentFacts(ctx, db, dimID, parentMemberID, childCode, modelID)
	return affected, modelID, err == nil, err
}

// DropWidgetsReferencing removes the dashboard widgets that point at a
// deleted metric, grid or integration. ref_id has no foreign key, so a widget
// over a deleted resource showed a confident 0 or failed to load forever.
// A chart names its metrics inside widget_props, not in ref_id: the id is
// dropped from every chart's list, and a chart left plotting nothing is
// removed; a grid widget's chosen metric_ids lose it too. The errors of the statements that failed are joined.
func DropWidgetsReferencing(ctx context.Context, db DB, refID string) error {
	if refID == "" {
		return nil
	}
	var errs []error
	if _, err := db.Exec(ctx, `DELETE FROM model.dashboard_widget WHERE ref_id = $1`, refID); err != nil {
		errs = append(errs, fmt.Errorf("drop widgets referencing %s: %w", refID, err))
	}
	// Two statements, not one CTE: Postgres will not update and delete the
	// same row in one statement (only one of the two silently happens).
	rows, err := db.Query(ctx, `
		UPDATE model.dashboard_widget
		SET widget_props = jsonb_set(widget_props, '{chart,metric_ids}', (widget_props->'chart'->'metric_ids') - $1::text)
		WHERE widget_props->'chart'->'metric_ids' ? $1::text
		RETURNING id::text, jsonb_array_length(widget_props->'chart'->'metric_ids')`, refID)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("drop %s from chart widgets: %w", refID, err))...)
	}
	var emptied []string
	for rows.Next() {
		var id string
		var left int
		if rows.Scan(&id, &left) == nil && left == 0 {
			emptied = append(emptied, id)
		}
	}
	rows.Close()
	// A grid widget's chosen metrics lose the id too; one left with none
	// shows all of its grid's again.
	if _, err := db.Exec(ctx, `
		UPDATE model.dashboard_widget
		SET widget_props = CASE WHEN jsonb_array_length((widget_props->'metric_ids') - $1::text) = 0
		                        THEN widget_props - 'metric_ids'
		                        ELSE jsonb_set(widget_props, '{metric_ids}', (widget_props->'metric_ids') - $1::text) END
		WHERE jsonb_typeof(widget_props->'metric_ids') = 'array' AND widget_props->'metric_ids' ? $1::text`, refID); err != nil {
		errs = append(errs, fmt.Errorf("drop %s from grid widgets: %w", refID, err))
	}
	if len(emptied) > 0 {
		if _, err := db.Exec(ctx, `DELETE FROM model.dashboard_widget WHERE id::text = ANY($1)`, emptied); err != nil {
			errs = append(errs, fmt.Errorf("drop charts left without metrics: %w", err))
		}
	}
	return errors.Join(errs...)
}
