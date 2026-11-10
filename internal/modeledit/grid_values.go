package modeledit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// A metric's stored values are keyed by their full coordinate
// (fact_input.dim_members, {dimension id: member code}), and every reader
// matches it exactly: the grid's cells and the calculation key a metric by
// the dimensions of the grid it is on. A grid gaining a dimension, or a
// metric moved onto a grid with other dimensions, changes that coordinate,
// so the typed values move with it here — in the same transaction as the
// change — or the change is refused (2026-11-09; before, the values stayed
// keyed the old way and every view and formula found none of them).

// Codes of a grid change refused because of the values or postings it holds.
const (
	// CodeValuesNeedMember: typed values have no member of a dimension the
	// grid gains; the request names the member they belong to.
	CodeValuesNeedMember = "VALUES_NEED_MEMBER"
	// CodeValuesCannotCombine: values kept along a dimension the metric's
	// new grid has not got cannot be combined by the metric's own rule.
	CodeValuesCannotCombine = "VALUES_CANNOT_COMBINE"
	// CodePostingsDoNotFit: a posting into the grid's metrics would put its
	// values in no cell after the change.
	CodePostingsDoNotFit = "POSTINGS_DO_NOT_FIT"
)

// GridValuesError is a grid change refused because of the values or the
// postings its metrics hold. Callers answer it as a conflict with its Code.
type GridValuesError struct {
	Code    string
	Message string
	// DimensionIDs are the dimensions the values need a member of
	// (CodeValuesNeedMember), or the dimension they cannot be combined along.
	DimensionIDs []string
	// Count is how many typed values need a member.
	Count int
}

func (e *GridValuesError) Error() string { return e.Message }

// IsGridValuesError reports whether err is (or wraps) a GridValuesError.
func IsGridValuesError(err error) (*GridValuesError, bool) {
	var ge *GridValuesError
	ok := errors.As(err, &ge)
	return ge, ok
}

// GridValuesResult is what a grid change did to the stored values.
type GridValuesResult struct {
	// Moved is how many cells of stored typed values were given a member of
	// a dimension they lacked.
	Moved int64 `json:"moved"`
	// Combined is how many typed values were combined along a dimension the
	// metric no longer has, by the metric's own rule.
	Combined int `json:"combined"`
}

// AddGridDimension adds dimID to gridID inside tx. When the grid's metrics
// hold typed values, they belong to member (a code or a member id of dimID,
// a leaf that takes input); without one the add is refused
// (CodeValuesNeedMember), and so is a posting into the grid's metrics that
// maps no dimension of dimID's family (CodePostingsDoNotFit). The grid's time
// validation runs in the same transaction. Adding a dimension the grid has
// already changes nothing.
func AddGridDimension(ctx context.Context, tx pgx.Tx, gridID, dimID, member string) (GridValuesResult, error) {
	var res GridValuesResult
	var modelID, revisionID string
	if err := tx.QueryRow(ctx, `SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.grid_def WHERE id=$1::uuid`,
		gridID).Scan(&modelID, &revisionID); err != nil {
		return res, fmt.Errorf("grid not found: %w", err)
	}
	var already bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM model.grid_dimension WHERE grid_id=$1::uuid AND dimension_id=$2::uuid)`,
		gridID, dimID).Scan(&already); err != nil {
		return res, err
	}
	if already {
		return res, nil
	}
	if msg, err := crudapp.GridDimensionAddRefusal(ctx, tx, gridID, dimID); err != nil {
		return res, err
	} else if msg != "" {
		return res, &GridValuesError{Code: CodePostingsDoNotFit, Message: msg, DimensionIDs: []string{dimID}}
	}
	var lacking int
	if err := tx.QueryRow(ctx, `
		SELECT count(DISTINCT (fi.metric_id, fi.revision_id, fi.dim_members))
		FROM runtime.fact_input fi JOIN model.grid_metric gm ON gm.metric_id = fi.metric_id
		WHERE gm.grid_id = $1::uuid AND fi.source_ref IS NULL AND NOT fi.dim_members ? $2::text`,
		gridID, dimID).Scan(&lacking); err != nil {
		return res, err
	}
	code := ""
	if lacking > 0 {
		dimName := dimensionName(ctx, tx, dimID)
		if strings.TrimSpace(member) == "" {
			return res, &GridValuesError{Code: CodeValuesNeedMember, DimensionIDs: []string{dimID}, Count: lacking,
				Message: fmt.Sprintf("the grid's metrics hold %d values without %s: name the member of %s they belong to (member)",
					lacking, dimName, dimName)}
		}
		var err error
		if code, err = resolveValuesMember(ctx, tx, modelID, revisionID, dimID, member); err != nil {
			return res, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid,$2::uuid) ON CONFLICT DO NOTHING`,
		gridID, dimID); err != nil {
		return res, err
	}
	if code != "" {
		if err := giveMember(ctx, tx, `fi.metric_id IN (SELECT metric_id FROM model.grid_metric WHERE grid_id = $3::uuid)`, dimID, code, gridID); err != nil {
			return res, err
		}
		res.Moved = int64(lacking)
	}
	if err := metricformula.ValidateGridTime(ctx, tx, modelID, revisionID, gridID); err != nil {
		return res, err
	}
	return res, nil
}

// AttachGridMetric puts metricID on gridID inside tx and moves its typed
// values to the grid's coordinate:
//   - a dimension of the grid its values lack gets the member named in
//     members (dimension id → code or member id), refused without one
//     (CodeValuesNeedMember);
//   - a dimension its values carry that none of the metric's grids has is
//     combined away by the metric's own rule (owner decision 2026-11-09:
//     add them up): its aggregation rule across a standard dimension's
//     members, its time summary across a time dimension's periods. A rule
//     that cannot be applied to stored values (rate, formula, none, a text or
//     pick-list metric) refuses (CodeValuesCannotCombine). The values replaced
//     stay in the cell history with the reason.
//
// Every posting into the metric must fit the grid (CodePostingsDoNotFit), and
// the grid's time validation runs in the same transaction. actorID is who the
// combined values are entered by ("" for none).
func AttachGridMetric(ctx context.Context, tx pgx.Tx, gridID, metricID string, members map[string]string, actorID string) (GridValuesResult, error) {
	var res GridValuesResult
	var modelID, revisionID, gridName string
	if err := tx.QueryRow(ctx, `SELECT model_id::text, COALESCE(revision_id::text,''), name FROM model.grid_def WHERE id=$1::uuid`,
		gridID).Scan(&modelID, &revisionID, &gridName); err != nil {
		return res, fmt.Errorf("grid not found: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO model.grid_metric (grid_id, metric_id, sort_order)
		VALUES ($1::uuid,$2::uuid,(SELECT COALESCE(MAX(sort_order),0)+1 FROM model.grid_metric WHERE grid_id=$1::uuid))
		ON CONFLICT DO NOTHING`, gridID, metricID); err != nil {
		return res, err
	}
	if err := metricformula.ValidateGridTime(ctx, tx, modelID, revisionID, gridID); err != nil {
		return res, err
	}
	if err := crudapp.CheckMetricPostingsPlaced(ctx, tx, metricID); err != nil {
		if crudapp.IsMappingRefError(err) {
			return res, &GridValuesError{Code: CodePostingsDoNotFit, Message: err.Error()}
		}
		return res, err
	}

	// The dimensions the metric keeps: those of every grid it is on.
	var kept []string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(array_agg(DISTINCT gd.dimension_id::text), '{}')
		FROM model.grid_metric gm JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
		WHERE gm.metric_id = $1::uuid`, metricID).Scan(&kept); err != nil {
		return res, err
	}
	facts, err := latestTypedFacts(ctx, tx, metricID)
	if err != nil {
		return res, err
	}
	// Dimensions the values carry the metric no longer has, and dimensions
	// it has some value lacks (once those are combined away).
	var extra, lacking []string
	lackCount := map[string]int{}
	for _, f := range facts {
		for k := range f.dims {
			if !slices.Contains(kept, k) && !slices.Contains(extra, k) {
				extra = append(extra, k)
			}
		}
		for _, d := range kept {
			if _, ok := f.dims[d]; !ok {
				lackCount[d]++
				if !slices.Contains(lacking, d) {
					lacking = append(lacking, d)
				}
			}
		}
	}
	slices.Sort(extra)
	slices.Sort(lacking)

	// Every check before anything changes: refuse whole or apply whole.
	codes := map[string]string{}
	var unnamed []string
	unnamedCount := 0
	for _, d := range lacking {
		m := strings.TrimSpace(members[d])
		if m == "" {
			unnamed = append(unnamed, d)
			unnamedCount = max(unnamedCount, lackCount[d])
			continue
		}
		code, err := resolveValuesMember(ctx, tx, modelID, revisionID, d, m)
		if err != nil {
			return res, err
		}
		codes[d] = code
	}
	if len(unnamed) > 0 {
		names := make([]string, len(unnamed))
		for i, d := range unnamed {
			names[i] = dimensionName(ctx, tx, d)
		}
		return res, &GridValuesError{Code: CodeValuesNeedMember, DimensionIDs: unnamed, Count: unnamedCount,
			Message: fmt.Sprintf("the metric holds values without %s, which the grid %s has: name the member they belong to for each (members: {dimension id: member})",
				strings.Join(names, ", "), gridName)}
	}
	var combined []factRow
	if len(extra) > 0 {
		var reason string
		if combined, res.Combined, reason, err = combineAlong(ctx, tx, metricID, gridName, facts, extra); err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('mvx.delete_reason', $1, true)`, reason); err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM runtime.fact_input
			WHERE metric_id = $1::uuid AND source_ref IS NULL AND dim_members ?| $2::text[]`, metricID, extra); err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('mvx.delete_reason', '', true)`); err != nil {
			return res, err
		}
		// The values move to the grid's cells: carried, not entered, so the
		// metric's input rules (migration 173) do not refuse them.
		if _, err := tx.Exec(ctx, `SELECT set_config('mvx.skip_input_rules', 'on', true)`); err != nil {
			return res, err
		}
		for _, c := range combined {
			raw, _ := json.Marshal(c.dims)
			if _, err := tx.Exec(ctx, `
				INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by)
				VALUES ($1::uuid, NULLIF($2,'')::uuid, $3::uuid, $4::jsonb, $5, NULLIF($6,'')::uuid)`,
				modelID, c.revisionID, metricID, string(raw), c.value, actorID); err != nil {
				return res, err
			}
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('mvx.skip_input_rules', '', true)`); err != nil {
			return res, err
		}
	}
	if len(lacking) > 0 {
		// Cells (each coordinate once, not each entry of its history).
		if err := tx.QueryRow(ctx, `
			SELECT count(DISTINCT (revision_id, dim_members)) FROM runtime.fact_input
			WHERE metric_id = $1::uuid AND source_ref IS NULL AND NOT (dim_members ?& $2::text[])`,
			metricID, lacking).Scan(&res.Moved); err != nil {
			return res, err
		}
	}
	for _, d := range lacking {
		if err := giveMember(ctx, tx, `fi.metric_id = $3::uuid`, d, codes[d], metricID); err != nil {
			return res, err
		}
	}
	return res, nil
}

// giveMember files every typed value matched by where (with $3) that lacks
// dimension dimID under member code, in the facts and in their cell history.
// An UPDATE archives nothing (the trigger archives deletes), so the history
// is re-keyed the same way and follows the cell.
func giveMember(ctx context.Context, tx pgx.Tx, where, dimID, code, arg string) error {
	for _, table := range []string{"runtime.fact_input", "runtime.fact_input_history"} {
		if _, err := tx.Exec(ctx, `
			UPDATE `+table+` fi SET dim_members = fi.dim_members || jsonb_build_object($1::text, $2::text)
			WHERE fi.source_ref IS NULL AND NOT fi.dim_members ? $1::text AND `+where, dimID, code, arg); err != nil {
			return err
		}
	}
	return nil
}

// resolveValuesMember resolves member (a code or a member id of dimID) to
// the code the values are filed under, holding it to the rules of a typed
// value: a leaf of the dimension that is not calculated
// (writeguard.ResolveWriteMembers answers a *writeguard.MemberError).
func resolveValuesMember(ctx context.Context, tx pgx.Tx, modelID, revisionID, dimID, member string) (string, error) {
	code := strings.TrimSpace(member)
	_ = tx.QueryRow(ctx, `SELECT code FROM model.dimension_member WHERE dimension_id=$1::uuid AND id::text=$2`, dimID, code).Scan(&code)
	if _, err := writeguard.ResolveWriteMembers(ctx, tx, modelID, revisionID, map[string]string{dimID: code}); err != nil {
		return "", err
	}
	return code, nil
}

func dimensionName(ctx context.Context, tx pgx.Tx, dimID string) string {
	name := dimID
	_ = tx.QueryRow(ctx, `SELECT name FROM model.dimension_def WHERE id=$1::uuid`, dimID).Scan(&name)
	return name
}

// factRow is one typed value at its coordinate.
type factRow struct {
	revisionID string
	dims       map[string]string
	value      float64
	text       bool
}

// latestTypedFacts reads the metric's typed values: the latest entry at each
// coordinate of each revision, as every reader takes them.
func latestTypedFacts(ctx context.Context, tx pgx.Tx, metricID string) ([]factRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (revision_id, dim_members) COALESCE(revision_id::text,''), dim_members::text, value::float8, text_value IS NOT NULL
		FROM runtime.fact_input
		WHERE metric_id = $1::uuid AND source_ref IS NULL
		ORDER BY revision_id, dim_members, entered_at DESC, id DESC`, metricID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []factRow
	for rows.Next() {
		var f factRow
		var raw string
		if err := rows.Scan(&f.revisionID, &raw, &f.value, &f.text); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &f.dims); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// combineAlong combines facts along the dimensions extra by the metric's own
// rule, as the grid totals the metric across them: the standard dimensions
// first, by its aggregation rule over every value at the same remaining
// coordinate (and period); then a time dimension, by its time summary over
// the periods in order. It answers the combined values, how many values went
// into them, and the reason the cell history keeps for the values replaced.
func combineAlong(ctx context.Context, tx pgx.Tx, metricID, gridName string, facts []factRow, extra []string) ([]factRow, int, string, error) {
	var label, aggRule, timeSummary, format string
	var picklist bool
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(label,''), name), COALESCE(agg_rule,''), COALESCE(time_summary,'sum'), COALESCE(format,''),
		       picklist_dimension_id IS NOT NULL
		FROM model.metric_def WHERE id=$1::uuid`, metricID).Scan(&label, &aggRule, &timeSummary, &format, &picklist); err != nil {
		return nil, 0, "", err
	}
	var timeDims, stdDims []string
	for _, d := range extra {
		var isTime bool
		_ = tx.QueryRow(ctx, `SELECT dimension_type = 'time' FROM model.dimension_def WHERE id=$1::uuid`, d).Scan(&isTime)
		if isTime {
			timeDims = append(timeDims, d)
		} else {
			stdDims = append(stdDims, d)
		}
	}
	refuse := func(d, why string) error {
		return &GridValuesError{Code: CodeValuesCannotCombine, DimensionIDs: []string{d},
			Message: fmt.Sprintf("%s holds values per %s, which the grid %s has not got, and %s: clear those values, or put %s on a grid with %s",
				label, dimensionName(ctx, tx, d), gridName, why, label, dimensionName(ctx, tx, d))}
	}
	for _, f := range facts {
		if f.text || picklist {
			return nil, 0, "", refuse(extra[0], "its values are text or members, which cannot be combined")
		}
	}
	if len(stdDims) > 0 {
		switch rollup.AggRule(aggRule) {
		case "", rollup.AggSum, rollup.AggAverage, rollup.AggCount:
		default:
			return nil, 0, "", refuse(stdDims[0], fmt.Sprintf("its aggregation rule %q gives no total that stored values can be combined into", aggRule))
		}
	}
	if len(timeDims) > 1 {
		return nil, 0, "", refuse(timeDims[1], "values cannot be combined along two time dimensions at once")
	}
	if len(timeDims) == 1 && timeSummary == "none" {
		return nil, 0, "", refuse(timeDims[0], "its time summary is none, so its periods have no total")
	}

	without := func(dims map[string]string, drop []string) map[string]string {
		out := make(map[string]string, len(dims))
		for k, v := range dims {
			if !slices.Contains(drop, k) {
				out[k] = v
			}
		}
		return out
	}
	key := func(rev string, dims map[string]string) string {
		raw, _ := json.Marshal(dims) // map keys marshal sorted
		return rev + "|" + string(raw)
	}
	n := 0
	carrying := make([]factRow, 0, len(facts))
	for _, f := range facts {
		if slices.ContainsFunc(extra, func(d string) bool { _, ok := f.dims[d]; return ok }) {
			carrying = append(carrying, f)
			n++
		}
	}
	// 1. Standard dimensions: one value per remaining coordinate, period kept.
	type group struct {
		row  factRow
		vals []float64
	}
	step := func(in []factRow, drop []string, combine func([]factRow) float64) []factRow {
		groups := map[string]*group{}
		var order []string
		for _, f := range in {
			d := without(f.dims, drop)
			k := key(f.revisionID, d)
			g, ok := groups[k]
			if !ok {
				g = &group{row: factRow{revisionID: f.revisionID, dims: d}}
				groups[k] = g
				order = append(order, k)
			}
			g.vals = append(g.vals, f.value)
		}
		out := make([]factRow, 0, len(order))
		for _, k := range order {
			g := groups[k]
			members := make([]factRow, len(g.vals))
			for i, v := range g.vals {
				members[i] = factRow{value: v}
			}
			g.row.value = combine(members)
			out = append(out, g.row)
		}
		return out
	}
	rows := carrying
	if len(stdDims) > 0 {
		rows = step(rows, stdDims, func(m []factRow) float64 {
			vals := make([]float64, len(m))
			for i, r := range m {
				vals[i] = r.value
			}
			return rollup.CombineAgg(vals, rollup.AggRule(aggRule))
		})
	}
	// 2. A time dimension: the time summary over the periods in order.
	if len(timeDims) == 1 {
		td := timeDims[0]
		index := map[string]int{}
		prows, err := tx.Query(ctx, `
			SELECT m.code, COALESCE(m.time_index, 0) FROM model.dimension_member m
			WHERE m.dimension_id = $1::uuid
			  AND NOT EXISTS (SELECT 1 FROM model.dimension_member c WHERE c.parent_member_id = m.id)`, td)
		if err != nil {
			return nil, 0, "", err
		}
		for prows.Next() {
			var code string
			var idx int
			if err := prows.Scan(&code, &idx); err != nil {
				prows.Close()
				return nil, 0, "", err
			}
			index[code] = idx
		}
		prows.Close()
		periods := len(index)
		// Sorted by period before grouping, so each group's values are in
		// time order for first, last and the running summaries.
		slices.SortStableFunc(rows, func(a, b factRow) int { return index[a.dims[td]] - index[b.dims[td]] })
		var refusal error
		rows = step(rows, []string{td}, func(m []factRow) float64 {
			vals := make([]float64, len(m))
			for i, r := range m {
				vals[i] = r.value
			}
			v, ok := calculation.TimeSummaryOver(timeSummary, vals, max(periods, len(vals)))
			if !ok && refusal == nil {
				refusal = refuse(td, fmt.Sprintf("its time summary %q gives no total here", timeSummary))
			}
			return v
		})
		if refusal != nil {
			return nil, 0, "", refusal
		}
	}
	names := make([]string, len(extra))
	for i, d := range extra {
		names[i] = dimensionName(ctx, tx, d)
	}
	reason := fmt.Sprintf("combined along %s when %s moved to the grid %s", strings.Join(names, ", "), label, gridName)
	return rows, n, reason, nil
}
