package crudapp

// What a table-to-metric mapping (model.form_metric_mapping) may name: only
// its own model's table, grid, input metric and dimensions, in its own
// revision, and fields its table has. The developer API stored whatever ids
// a body named, so a mapping could name another tenant's table and post —
// or, by a backfill, read — that tenant's records.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// MappingRefs are the references a mapping holds.
type MappingRefs struct {
	FormID         string
	GridID         string // "" for none
	TargetMetricID string
	SourceField    string
	// DimensionMappings: dimension id → field name.
	DimensionMappings map[string]string
	// RowFilter: the conditions a record meets to post (row_filter.go).
	RowFilter []RowCondition
}

// MappingRefError is a reference a mapping may not hold; its message says
// which, without saying whether the id exists elsewhere.
type MappingRefError struct{ msg string }

func (e *MappingRefError) Error() string { return e.msg }

// IsMappingRefError reports whether err is a MappingRefError.
func IsMappingRefError(err error) bool {
	var m *MappingRefError
	return errors.As(err, &m)
}

func refErr(format string, args ...any) error {
	return &MappingRefError{msg: fmt.Sprintf(format, args...)}
}

// inRevision is the condition that row alias t belongs to model $1 and, when
// revision $2 is given, to that revision or to none (revision-global).
const inRevision = `t.model_id = $1::uuid AND ($2 = '' OR t.revision_id IS NULL OR t.revision_id::text = $2)`

// CheckMappingRefs says what a mapping of model modelID in revision
// revisionID ("" for a revision-global one) names that is not its own: the
// table, the grid, the target metric (an input metric) and every dimension
// must be the model's, in that revision; the source field and every mapped
// field must be fields of the table. nil when all are.
func CheckMappingRefs(ctx context.Context, db dbx.DB, modelID, revisionID string, m MappingRefs) error {
	found := func(table, id, extra string) (bool, error) {
		if uuid.Validate(id) != nil {
			return false, nil
		}
		var ok bool
		err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` t WHERE t.id = $3::uuid AND `+inRevision+extra+`)`,
			modelID, revisionID, id).Scan(&ok)
		return ok, err
	}

	if uuid.Validate(m.FormID) != nil {
		return refErr("form_id names no table of this model's revision")
	}
	var fieldsJSON []byte
	err := db.QueryRow(ctx, `SELECT t.fields FROM model.form_def t WHERE t.id = $3::uuid AND `+inRevision,
		modelID, revisionID, m.FormID).Scan(&fieldsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		// A table of another revision of the model: say so, so the caller
		// names that revision (a draft's table was reported as missing).
		var elsewhere bool
		if db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM model.form_def WHERE id = $2::uuid AND model_id = $1::uuid)`,
			modelID, m.FormID).Scan(&elsewhere) == nil && elsewhere {
			return refErr("form_id names a table of another revision of this model: pass that revision's revision_id")
		}
		return refErr("form_id names no table of this model's revision")
	}
	if err != nil {
		return err
	}
	var fields []FormField
	_ = json.Unmarshal(fieldsJSON, &fields)
	has := func(name string) bool {
		return slices.ContainsFunc(fields, func(f FormField) bool { return f.Name == name })
	}

	if ok, err := found("model.metric_def", m.TargetMetricID, " AND t.is_input"); err != nil {
		return err
	} else if !ok {
		return refErr("target_metric_id names no input metric of this model's revision")
	}
	if m.GridID != "" {
		if ok, err := found("model.grid_def", m.GridID, ""); err != nil {
			return err
		} else if !ok {
			return refErr("grid_id names no grid of this model's revision")
		}
	}
	if m.SourceField != "" && !has(m.SourceField) {
		return refErr("source_field %q is no field of the table", m.SourceField)
	}
	dims := make([]string, 0, len(m.DimensionMappings))
	for d := range m.DimensionMappings {
		dims = append(dims, d)
	}
	sort.Strings(dims)
	for _, dimID := range dims {
		if ok, err := found("model.dimension_def", dimID, ""); err != nil {
			return err
		} else if !ok {
			return refErr("dimension_mappings names %q, which is no dimension of this model's revision", dimID)
		}
		if field := m.DimensionMappings[dimID]; !has(field) {
			return refErr("dimension_mappings maps a dimension to %q, which is no field of the table", field)
		}
	}
	if err := checkRowFilter(ctx, db, fields, m.RowFilter); err != nil {
		return err
	}
	return checkMappedDimensionsPlaced(ctx, db, m, dims)
}

// checkMappedDimensionsPlaced holds a mapping to its target metric's grid
// (2026-11-07). A mapped dimension of no family of the grid's (the posting
// kept its values per member of it: the grid showed their sum, every formula
// read 0 — an AI build took Sites off its balance-sheet grid and four
// postings kept mapping it, so closing stock read 0 in each closed month),
// and a grid dimension no mapped dimension supplies (the values landed in no
// cell: counts into a grid with Scenario mapped by month only), are refused.
// A family is dimensions linked by parent_dimension_id or
// source_dimension_id: staff posts into a departments grid. An unplaced
// metric has no dimensions to check yet.
func checkMappedDimensionsPlaced(ctx context.Context, db dbx.DB, m MappingRefs, dims []string) error {
	var metricName string
	var placed []string
	if err := db.QueryRow(ctx, `
		SELECT md.name, COALESCE((SELECT array_agg(DISTINCT gd.dimension_id::text)
		         FROM model.grid_metric gm JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
		         WHERE gm.metric_id = md.id), '{}')
		FROM model.metric_def md WHERE md.id = $1::uuid`, m.TargetMetricID).Scan(&metricName, &placed); err != nil {
		return err
	}
	if len(placed) == 0 {
		return nil
	}
	family, names, err := dimensionFamilies(ctx, db, m.TargetMetricID)
	if err != nil {
		return err
	}
	same := func(a, b string) bool { return a == b || (family[a] != "" && family[a] == family[b]) }
	for _, dimID := range dims {
		if !slices.ContainsFunc(placed, func(g string) bool { return same(dimID, g) }) {
			return refErr("dimension_mappings maps %s, which %s's grid does not have: its values would be kept per %s and formulas, reading %s by its grid's dimensions, would find none — map only the grid's dimensions, or add %s to the grid first",
				names[dimID], metricName, names[dimID], metricName, names[dimID])
		}
	}
	for _, g := range placed {
		if !slices.ContainsFunc(dims, func(d string) bool { return same(d, g) }) {
			return refErr("dimension_mappings leaves %s unmapped, which %s's grid has: the records name no %s, so their values would land in no cell — map a field to %s, or post into a metric on a grid without it",
				names[g], metricName, names[g], names[g])
		}
	}
	return nil
}

// dimensionFamilies groups the dimensions of metricID's model revision that
// parent_dimension_id or source_dimension_id link (id → family root), with
// every dimension's name.
func dimensionFamilies(ctx context.Context, db dbx.DB, metricID string) (map[string]string, map[string]string, error) {
	rows, err := db.Query(ctx, `
		SELECT d.id::text, d.name, COALESCE(d.parent_dimension_id::text,''), COALESCE(d.source_dimension_id::text,'')
		FROM model.dimension_def d JOIN model.metric_def md ON md.id = $1::uuid
		WHERE d.model_id = md.model_id AND (md.revision_id IS NULL OR d.revision_id IS NULL OR d.revision_id = md.revision_id)`, metricID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	root := map[string]string{}
	names := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if r, ok := root[x]; ok && r != x {
			root[x] = find(r)
			return root[x]
		}
		root[x] = x
		return x
	}
	var links [][2]string
	for rows.Next() {
		var id, name, parent, source string
		if err := rows.Scan(&id, &name, &parent, &source); err != nil {
			return nil, nil, err
		}
		names[id] = name
		find(id)
		for _, o := range []string{parent, source} {
			if o != "" {
				links = append(links, [2]string{id, o})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, l := range links {
		root[find(l[0])] = find(l[1])
	}
	family := map[string]string{}
	for id := range names {
		family[id] = find(id)
	}
	return family, names, nil
}

// GridDimensionPostedRefusal says why dimID may not leave grid gridID ("" when
// it may): a posting into one of the grid's metrics still maps it — or a
// dimension of its family no other grid dimension stands for (staff, once
// departments goes) — and would keep storing values per member of it that no
// formula reads. The posting is changed (or deleted) first; then the
// dimension goes.
func GridDimensionPostedRefusal(ctx context.Context, db dbx.DB, gridID, dimID string) (string, error) {
	rows, err := db.Query(ctx, `
		SELECT COALESCE(NULLIF(fm.name,''), fm.id::text), md.id::text, md.name,
		       ARRAY(SELECT jsonb_object_keys(fm.dimension_mappings))
		FROM model.form_metric_mapping fm
		JOIN model.grid_metric gm ON gm.metric_id = fm.target_metric_id AND gm.grid_id = $1::uuid
		JOIN model.metric_def md ON md.id = fm.target_metric_id
		ORDER BY 1`, gridID)
	if err != nil {
		return "", err
	}
	type posting struct {
		name, metricID, metric string
		dims                   []string
	}
	var postings []posting
	for rows.Next() {
		var p posting
		if err := rows.Scan(&p.name, &p.metricID, &p.metric, &p.dims); err != nil {
			rows.Close()
			return "", err
		}
		postings = append(postings, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(postings) == 0 {
		return "", err
	}
	var gridDims []string
	if err := db.QueryRow(ctx, `SELECT COALESCE(array_agg(dimension_id::text), '{}') FROM model.grid_dimension WHERE grid_id = $1::uuid`,
		gridID).Scan(&gridDims); err != nil {
		return "", err
	}
	family, dimNames, err := dimensionFamilies(ctx, db, postings[0].metricID)
	if err != nil {
		return "", err
	}
	same := func(a, b string) bool { return a == b || (family[a] != "" && family[a] == family[b]) }
	// A mapped dimension still stands for one of the grid's other dimensions.
	kept := func(d string) bool {
		return slices.ContainsFunc(gridDims, func(g string) bool { return g != dimID && same(d, g) })
	}
	var names []string
	for _, p := range postings {
		if slices.ContainsFunc(p.dims, func(d string) bool { return same(d, dimID) && !kept(d) }) {
			names = append(names, fmt.Sprintf("%q (into %s)", p.name, p.metric))
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	dimName := dimNames[dimID]
	return fmt.Sprintf("the grid keeps %s: %s still map it — update those postings' dimension_mappings (and backfill them) or delete them first, else their values stay per %s where no formula reads them",
		dimName, strings.Join(names, ", "), dimName), nil
}

// RemoveGridDimensionSQL takes dimension $2 off grid $1 and, in the same
// statement, deletes the values the grid's metrics keep along it — typed
// cells and calculated results alike (owner decision, 2026-11-07: "values on
// this dimension must be removed"). Left, the grid summed them into its cells
// while every formula, reading the metrics by the grid's dimensions, found
// none. Nothing is deleted when the dimension is not on the grid; a posting
// that maps it refuses the removal first (GridDimensionPostedRefusal), and the
// cell history keeps what the cells held.
const RemoveGridDimensionSQL = `
	WITH on_grid AS (
		SELECT 1 FROM model.grid_dimension WHERE grid_id = $1::uuid AND dimension_id = $2::uuid
	), typed AS (
		DELETE FROM runtime.fact_input fi USING model.grid_metric gm
		WHERE gm.grid_id = $1::uuid AND fi.metric_id = gm.metric_id AND fi.dim_members ? $2::text
		  AND EXISTS (SELECT 1 FROM on_grid)
	), calculated AS (
		DELETE FROM runtime.calc_result cr USING model.grid_metric gm
		WHERE gm.grid_id = $1::uuid AND cr.metric_id = gm.metric_id AND cr.dim_members ? $2::text
		  AND EXISTS (SELECT 1 FROM on_grid)
	)
	DELETE FROM model.grid_dimension WHERE grid_id = $1::uuid AND dimension_id = $2::uuid`

// GridDimensionAddRefusal says why dimID may not join grid gridID ("" when
// it may): a posting into one of the grid's metrics maps no dimension of
// dimID's family, so once the grid has it the posting's values would land in
// no cell (its records name no member of it). A posting cannot be changed to
// map it first — checkMappedDimensionsPlaced refuses a dimension the grid
// lacks — so it is deleted, the dimension added, and the posting created
// again mapping a field to it; its records then post again.
func GridDimensionAddRefusal(ctx context.Context, db dbx.DB, gridID, dimID string) (string, error) {
	rows, err := db.Query(ctx, `
		SELECT COALESCE(NULLIF(fm.name,''), fm.id::text), md.id::text, md.name,
		       ARRAY(SELECT jsonb_object_keys(fm.dimension_mappings))
		FROM model.form_metric_mapping fm
		JOIN model.grid_metric gm ON gm.metric_id = fm.target_metric_id AND gm.grid_id = $1::uuid
		JOIN model.metric_def md ON md.id = fm.target_metric_id
		WHERE NOT EXISTS (SELECT 1 FROM model.grid_dimension gd WHERE gd.grid_id = $1::uuid AND gd.dimension_id = $2::uuid)
		ORDER BY 1`, gridID, dimID)
	if err != nil {
		return "", err
	}
	type posting struct {
		name, metricID, metric string
		dims                   []string
	}
	var postings []posting
	for rows.Next() {
		var p posting
		if err := rows.Scan(&p.name, &p.metricID, &p.metric, &p.dims); err != nil {
			rows.Close()
			return "", err
		}
		postings = append(postings, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(postings) == 0 {
		return "", err
	}
	family, dimNames, err := dimensionFamilies(ctx, db, postings[0].metricID)
	if err != nil {
		return "", err
	}
	same := func(a, b string) bool { return a == b || (family[a] != "" && family[a] == family[b]) }
	var names []string
	for _, p := range postings {
		if !slices.ContainsFunc(p.dims, func(d string) bool { return same(d, dimID) }) {
			names = append(names, fmt.Sprintf("%q (into %s)", p.name, p.metric))
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	dimName := dimNames[dimID]
	return fmt.Sprintf("%s map no %s: once the grid has %s their values would land in no cell — delete those postings, add %s, then create them again mapping a field to %s (their records post again)",
		strings.Join(names, ", "), dimName, dimName, dimName, dimName), nil
}

// CheckMetricPostingsPlaced holds every posting into metricID to the grid
// the metric is on now, as saving a posting does (checkMappedDimensionsPlaced):
// a metric attached to a grid whose dimensions its postings do not map would
// keep values no cell reads.
func CheckMetricPostingsPlaced(ctx context.Context, db dbx.DB, metricID string) error {
	rows, err := db.Query(ctx, `
		SELECT COALESCE(NULLIF(name,''), id::text), ARRAY(SELECT jsonb_object_keys(dimension_mappings))
		FROM model.form_metric_mapping WHERE target_metric_id = $1::uuid ORDER BY 1`, metricID)
	if err != nil {
		return err
	}
	type posting struct {
		name string
		dims []string
	}
	var postings []posting
	for rows.Next() {
		var p posting
		if err := rows.Scan(&p.name, &p.dims); err != nil {
			rows.Close()
			return err
		}
		postings = append(postings, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range postings {
		if err := checkMappedDimensionsPlaced(ctx, db, MappingRefs{TargetMetricID: metricID}, p.dims); err != nil {
			return refErr("the posting %q does not fit this grid: %v", p.name, err)
		}
	}
	return nil
}
