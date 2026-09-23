package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/timedim"
)

// DBCommitter is the production Committer: pulls commit through the SAME
// pipeline as file uploads — importpkg.ResolveRows → stage → CommitImport
// (which enforces internal/writeguard and returns ErrWriteDenied) → the
// calculation scheduler; dimension targets go through the member-upsert
// semantics the CSV importer established; form targets through crudapp.
// Scheduled/worker runs execute under developer-role visibility by explicit
// decision — CommitImport's write guards (workflow locks, system-managed
// revisions) still apply.
type DBCommitter struct {
	Pool *pgxpool.Pool
	Log  zerolog.Logger
}

func (c *DBCommitter) CommitPull(ctx context.Context, def *Definition, header []string, rows [][]string, dryRun bool, runBy string) (written, skipped int, err error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	if runBy == "" && !dryRun {
		// Every write needs a real principal: manual runs carry the caller,
		// scheduled runs the developer who enabled the schedule.
		return 0, 0, fmt.Errorf("run has no acting principal")
	}
	switch def.Config.TargetType {
	case TargetGrid:
		return c.commitGrid(ctx, def, header, rows, dryRun, runBy)
	case TargetDimension:
		return c.commitDimension(ctx, def, header, rows, dryRun)
	case TargetForm:
		return c.commitForm(ctx, def, header, rows, dryRun, runBy)
	}
	return 0, 0, fmt.Errorf("unknown target type")
}

func (c *DBCommitter) commitGrid(ctx context.Context, def *Definition, header []string, rows [][]string, dryRun bool, runBy string) (int, int, error) {
	raw := make([]importpkg.RawRow, 0, len(rows))
	for i, r := range rows {
		cells := make(map[string]string, len(header))
		for j, h := range header {
			if j < len(r) {
				cells[h] = r[j]
			}
		}
		raw = append(raw, importpkg.RawRow{RowNumber: i + 1, Cells: cells})
	}
	staged, importErrs, err := importpkg.ResolveRows(ctx, c.Pool, def.ModelID, def.RevisionID, header, raw)
	if err != nil {
		return 0, 0, err
	}
	if len(importErrs) > 0 {
		// Connector semantics differ from file upload: skip bad records and
		// commit the rest (partial), because a scheduled sync must not stall
		// forever on one bad upstream record. The skip count is reported.
		bad := map[int]bool{}
		for _, e := range importErrs {
			bad[int(e.RowNumber)] = true
		}
		kept := staged[:0]
		for _, s := range staged {
			if !bad[int(s.RowNumber)] {
				kept = append(kept, s)
			}
		}
		staged = kept
	}
	if dryRun {
		return len(staged), len(importErrs), nil
	}
	if len(staged) == 0 {
		return 0, len(importErrs), nil
	}
	store := importpkg.NewStore(c.Pool)
	job, err := store.CreateImportJob(ctx, def.ModelID, def.RevisionID, "rest_api", runBy, nil)
	if err != nil {
		return 0, 0, err
	}
	if err := store.StageRows(ctx, job.Id, staged, nil); err != nil {
		return 0, 0, err
	}
	metricIDs, err := store.CommitImport(ctx, job.Id, def.ModelID, def.RevisionID, runBy, importpkg.ImportMode(def.Config.ImportMode))
	if err != nil {
		return 0, 0, err
	}
	if len(metricIDs) > 0 {
		sched := calculation.NewScheduler(c.Log, calculation.NewStore(c.Pool), nil)
		if rerr := sched.RecalcAffected(ctx, def.ModelID, def.RevisionID, metricIDs); rerr != nil {
			c.Log.Warn().Err(rerr).Msg("recalc after connector import")
		}
	}
	return len(staged), len(importErrs), nil
}

// commitDimension upserts members: code (required), label, parent_code,
// property:* columns — the same semantics importDimensionMembersCSV
// established, including property declaration upserts.
func (c *DBCommitter) commitDimension(ctx context.Context, def *Definition, header []string, rows [][]string, dryRun bool) (int, int, error) {
	dimID := def.Config.TargetID
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	codeIdx, ok := col["code"]
	if !ok {
		// Fall back to the first mapped column as the code.
		codeIdx = 0
	}
	cfg, err := timedim.LoadConfig(ctx, c.Pool, dimID)
	if err != nil {
		return 0, 0, fmt.Errorf("dimension not found: %w", err)
	}
	if cfg.Type == timedim.TypeTime {
		return c.commitTimeDimension(ctx, dimID, cfg, col, codeIdx, rows, dryRun)
	}
	written, skipped := 0, 0
	for _, r := range rows {
		code := strings.TrimSpace(r[codeIdx])
		if code == "" {
			skipped++
			continue
		}
		if dryRun {
			written++
			continue
		}
		label := code
		if li, ok := col["label"]; ok && li < len(r) && strings.TrimSpace(r[li]) != "" {
			label = strings.TrimSpace(r[li])
		}
		var parentID *string
		if pi, ok := col["parent_code"]; ok && pi < len(r) && strings.TrimSpace(r[pi]) != "" {
			var pid string
			if err := c.Pool.QueryRow(ctx, `
				SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2
			`, dimID, strings.TrimSpace(r[pi])).Scan(&pid); err == nil {
				parentID = &pid
			}
		}
		props := map[string]string{}
		for name, idx := range col {
			if p, isProp := strings.CutPrefix(name, "property:"); isProp && idx < len(r) && r[idx] != "" {
				props[p] = r[idx]
				_, _ = c.Pool.Exec(ctx, `
					INSERT INTO model.dimension_property (dimension_id, name, data_type)
					VALUES ($1::uuid, $2, 'text') ON CONFLICT (dimension_id, name) DO NOTHING
				`, dimID, p)
			}
		}
		propJSON, _ := json.Marshal(props)
		if _, err := c.Pool.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, sort_order, properties)
			VALUES ($1::uuid, $2, $3, $4::uuid, (SELECT COALESCE(MAX(sort_order),0)+1 FROM model.dimension_member WHERE dimension_id=$1::uuid), $5::jsonb)
			ON CONFLICT (dimension_id, code) DO UPDATE SET
			    label=EXCLUDED.label,
			    parent_member_id=COALESCE(EXCLUDED.parent_member_id, model.dimension_member.parent_member_id),
			    properties=model.dimension_member.properties || EXCLUDED.properties
		`, dimID, code, label, parentID, string(propJSON)); err != nil {
			skipped++
			continue
		}
		written++
	}
	return written, skipped, nil
}

// commitTimeDimension is commitDimension for a time dimension: a row with
// period_start and period_end is a leaf period, a row without is an
// aggregate period (H1, FY26); parent_code works as on any hierarchy
// (parents listed first). The whole batch is validated and re-indexed
// through the shared timedim service in one transaction — a bad period set
// rejects the run rather than half-applying.
func (c *DBCommitter) commitTimeDimension(ctx context.Context, dimID string, cfg timedim.Config, col map[string]int, codeIdx int, rows [][]string, dryRun bool) (int, int, error) {
	startIdx, okS := col["period_start"]
	endIdx, okE := col["period_end"]
	if !okS || !okE {
		return 0, 0, &timedim.Error{Code: timedim.CodeInvalidTimeMember, Message: "a time dimension import needs period_start and period_end columns"}
	}
	parentIdx, hasParent := col["parent_code"]
	cell := func(r []string, i int) string {
		if i < 0 || i >= len(r) {
			return ""
		}
		return strings.TrimSpace(r[i])
	}
	type row struct {
		code, label, parent string
		start, end          *time.Time
	}
	var parsed []row
	var shapes []timedim.MemberShape
	for _, r := range rows {
		code := cell(r, codeIdx)
		if code == "" {
			continue
		}
		var start, end *time.Time
		if cell(r, startIdx) != "" || cell(r, endIdx) != "" {
			ps, err1 := timedim.ParseDate(cell(r, startIdx))
			pe, err2 := timedim.ParseDate(cell(r, endIdx))
			if err1 != nil || err2 != nil {
				return 0, 0, &timedim.Error{Code: timedim.CodeInvalidTimeMember, Message: fmt.Sprintf("member %q: period_start and period_end must be YYYY-MM-DD dates (both empty for an aggregate period)", code)}
			}
			start, end = &ps, &pe
		}
		label := code
		if li, ok := col["label"]; ok && cell(r, li) != "" {
			label = cell(r, li)
		}
		parent := ""
		if hasParent {
			parent = cell(r, parentIdx)
		}
		parsed = append(parsed, row{code: code, label: label, parent: parent, start: start, end: end})
		shapes = append(shapes, timedim.MemberShape{ID: code, Code: code, ParentID: parent, Start: start, End: end})
	}
	if dryRun {
		// Validate the file on its own so a dry run reports what a real run
		// would reject.
		if _, err := timedim.ValidateHierarchy(cfg, shapes); err != nil {
			return 0, 0, err
		}
		return len(parsed), len(rows) - len(parsed), nil
	}
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	for _, r := range parsed {
		var idx *int
		if r.start != nil {
			zero := 0
			idx = &zero
		}
		var parentID *string
		if r.parent != "" {
			var pid string
			if err := tx.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, dimID, r.parent).Scan(&pid); err != nil {
				return 0, 0, &timedim.Error{Code: timedim.CodeInvalidTimeMember, Message: fmt.Sprintf("member %q: parent %q not found (list parents before their children)", r.code, r.parent)}
			}
			parentID = &pid
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, parent_member_id)
			VALUES ($1::uuid, $2, $3, $4::date, $5::date, $6, $7::uuid)
			ON CONFLICT (dimension_id, code) DO UPDATE SET
			    label=EXCLUDED.label, period_start=EXCLUDED.period_start, period_end=EXCLUDED.period_end, time_index=EXCLUDED.time_index,
			    parent_member_id=COALESCE(EXCLUDED.parent_member_id, model.dimension_member.parent_member_id)
		`, dimID, r.code, r.label, r.start, r.end, idx, parentID); err != nil {
			return 0, 0, fmt.Errorf("member %q: %w", r.code, err)
		}
	}
	if err := timedim.ValidateAndReindex(ctx, tx, dimID); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return len(parsed), len(rows) - len(parsed), nil
}

func (c *DBCommitter) commitForm(ctx context.Context, def *Definition, header []string, rows [][]string, dryRun bool, runBy string) (int, int, error) {
	store := crudapp.NewStore(c.Pool)
	written, skipped := 0, 0
	for _, r := range rows {
		data := make(map[string]any, len(header))
		empty := true
		for i, h := range header {
			if i < len(r) && r[i] != "" {
				data[h] = r[i]
				empty = false
			}
		}
		if empty {
			skipped++
			continue
		}
		if dryRun {
			written++
			continue
		}
		if _, err := store.CreateRecord(ctx, def.Config.TargetID, runBy, data); err != nil {
			skipped++
			continue
		}
		written++
	}
	return written, skipped, nil
}

// LoadPushRows reads the push source rows.
//   - dimension: one row per member (code/label/parent_code + property:*).
//   - form: one row per form record (its data keys).
//   - grid: one wide row per dim-combo over the mapped metrics, reading the
//     grid's deterministic latest-wins facts.
func (c *DBCommitter) LoadPushRows(ctx context.Context, def *Definition) ([]map[string]string, error) {
	switch def.Config.TargetType {
	case TargetDimension:
		return c.loadDimensionRows(ctx, def)
	case TargetForm:
		return c.loadFormRows(ctx, def)
	case TargetGrid:
		return c.loadGridRows(ctx, def)
	}
	return nil, fmt.Errorf("unknown target type")
}

func (c *DBCommitter) loadDimensionRows(ctx context.Context, def *Definition) ([]map[string]string, error) {
	rows, err := c.Pool.Query(ctx, `
		SELECT m.code, m.label, COALESCE(pm.code,''), COALESCE(m.properties,'{}'::jsonb)::text
		FROM model.dimension_member m
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE m.dimension_id=$1::uuid ORDER BY m.sort_order
	`, def.Config.TargetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var code, label, parent, propsRaw string
		if err := rows.Scan(&code, &label, &parent, &propsRaw); err != nil {
			return nil, err
		}
		row := map[string]string{"code": code, "label": label, "parent_code": parent}
		props := map[string]string{}
		_ = json.Unmarshal([]byte(propsRaw), &props)
		for k, v := range props {
			row["property:"+k] = v
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (c *DBCommitter) loadFormRows(ctx context.Context, def *Definition) ([]map[string]string, error) {
	rows, err := c.Pool.Query(ctx, `
		SELECT data::text FROM runtime.form_record WHERE form_id=$1::uuid ORDER BY created_at
	`, def.Config.TargetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var doc map[string]any
		_ = json.Unmarshal([]byte(raw), &doc)
		row := map[string]string{}
		for k, v := range doc {
			row[k] = valueToString(v)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (c *DBCommitter) loadGridRows(ctx context.Context, def *Definition) ([]map[string]string, error) {
	// Metric/dimension names for the grid.
	dimName := map[string]string{}
	drows, err := c.Pool.Query(ctx, `
		SELECT gd.dimension_id::text, d.name FROM model.grid_dimension gd
		JOIN model.dimension_def d ON d.id = gd.dimension_id WHERE gd.grid_id=$1::uuid
	`, def.Config.TargetID)
	if err != nil {
		return nil, err
	}
	for drows.Next() {
		var id, name string
		_ = drows.Scan(&id, &name)
		dimName[id] = name
	}
	drows.Close()

	type metric struct{ id, name string }
	var metrics []metric
	mrows, err := c.Pool.Query(ctx, `
		SELECT m.id::text, m.name FROM model.grid_metric gm
		JOIN model.metric_def m ON m.id = gm.metric_id
		WHERE gm.grid_id=$1::uuid AND m.is_input ORDER BY gm.sort_order
	`, def.Config.TargetID)
	if err != nil {
		return nil, err
	}
	for mrows.Next() {
		var m metric
		_ = mrows.Scan(&m.id, &m.name)
		metrics = append(metrics, m)
	}
	mrows.Close()

	// Deterministic latest-wins per (metric, combo) — the same ordering
	// every reader uses (entered_at DESC, id DESC).
	combos := map[string]map[string]string{} // comboKey -> row
	for _, m := range metrics {
		rows, err := c.Pool.Query(ctx, `
			SELECT dim_members::text, value::float8 FROM (
				SELECT DISTINCT ON (dim_members) dim_members, value
				FROM runtime.fact_input
				WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
				  AND source_ref IS NULL AND dim_members != '{}'::jsonb
				ORDER BY dim_members, entered_at DESC, id DESC
			) t
		`, def.ModelID, def.RevisionID, m.id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var dm string
			var v float64
			if err := rows.Scan(&dm, &v); err != nil {
				rows.Close()
				return nil, err
			}
			row, ok := combos[dm]
			if !ok {
				row = map[string]string{}
				var members map[string]string
				_ = json.Unmarshal([]byte(dm), &members)
				for dimID, code := range members {
					if n, known := dimName[dimID]; known {
						row[n] = code
					}
				}
				combos[dm] = row
			}
			row[m.name] = valueToString(v)
		}
		rows.Close()
	}
	out := make([]map[string]string, 0, len(combos))
	for _, row := range combos {
		out = append(out, row)
	}
	return out, nil
}
