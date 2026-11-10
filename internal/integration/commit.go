package integration

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	importpkgv1 "github.com/mavericks-engine/mavericks/gen/go/importpkg/v1"
	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/recordpost"
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

func (c *DBCommitter) CommitPull(ctx context.Context, def *Definition, header []string, rows [][]string, opts PullOptions) (PullOutcome, error) {
	fileName, dryRun, runBy := opts.FileName, opts.DryRun, opts.RunBy
	// A model link whose source holds no value at all still commits: it
	// clears what the link wrote before.
	if len(rows) == 0 && (!opts.Reconcile || def.Config.TargetType != TargetGrid) {
		return PullOutcome{}, nil
	}
	var written, skipped int
	var err error
	started := time.Now()
	if runBy == "" && !dryRun {
		// Every write needs a real principal: manual runs carry the caller,
		// scheduled runs the developer who enabled the schedule.
		return PullOutcome{}, fmt.Errorf("run has no acting principal")
	}
	// The last line before a write: never write into another model's row.
	if err := checkDefinitionOwnership(ctx, c.Pool, def); err != nil {
		return PullOutcome{}, err
	}
	switch def.Config.TargetType {
	case TargetGrid:
		return c.commitGrid(ctx, def, header, rows, fileName, started, opts)
	case TargetDimension:
		written, skipped, err = c.commitDimension(ctx, def, header, rows, dryRun)
		if !dryRun {
			c.recordJob(ctx, def, "dimension", fileName, runBy, started, written, skipped, err)
		}
		if err == nil && written > 0 && !dryRun {
			// Member codes, parents and properties are what LOOKUP, PARENT
			// and dim.property read; those references have no
			// calc_dependency edges, so recompute the metrics that make
			// them explicitly (contract C8), as the gateway's CSV import
			// does.
			sched := calculation.NewScheduler(c.Log, calculation.NewStore(c.Pool))
			if rerr := sched.RecalcDimensionDependents(ctx, def.Config.TargetID); rerr != nil {
				c.Log.Warn().Err(rerr).Msg("recalc after connector dimension import")
			}
		}
		return PullOutcome{Written: written, Skipped: skipped}, err
	case TargetForm:
		written, skipped, err = c.commitForm(ctx, def, header, rows, dryRun, runBy)
		if !dryRun {
			c.recordJob(ctx, def, "table", fileName, runBy, started, written, skipped, err)
		}
		return PullOutcome{Written: written, Skipped: skipped}, err
	}
	return PullOutcome{}, fmt.Errorf("unknown target type")
}

// jobSource is how Recent Imports names a connector's run.
func jobSource(def *Definition) string {
	switch def.Config.Protocol {
	case ProtocolSFTP:
		return "sftp"
	case ProtocolModel:
		return "model_link"
	default:
		return "rest_api"
	}
}

// recordJob adds a dimension's or a table's sync to Recent Imports, as the
// gateway's file imports are — best-effort: the line must never fail the run
// it describes. A grid's sync is its own staged job (commitGrid). These were
// only in the integration's run history (2026-10-23).
func (c *DBCommitter) recordJob(ctx context.Context, def *Definition, targetType, fileName, runBy string, started time.Time, written, skipped int, err error) {
	table := "model.dimension_def"
	if targetType == "table" {
		table = "model.form_def"
	}
	j := importpkg.FinishedJob{Source: jobSource(def), UserID: runBy, Rows: written, ErrorRows: skipped,
		Details: importpkg.JobDetails{TargetType: targetType, FileName: fileName, StartedAt: started}}
	_ = c.Pool.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text, ''), COALESCE(NULLIF(label, ''), name)
		FROM `+table+` WHERE id::text = $1`, def.Config.TargetID).Scan(&j.ModelID, &j.RevisionID, &j.Details.TargetName)
	if j.ModelID == "" {
		return
	}
	if err != nil {
		j.Failed, j.Rows = true, 0
		j.Errors = []*importpkgv1.ImportError{{Message: err.Error()}}
	}
	if _, rerr := importpkg.NewStore(c.Pool).RecordJob(ctx, j); rerr != nil {
		c.Log.Warn().Err(rerr).Str("integration", def.ID).Msg("record a connector's import job")
	}
}

func (c *DBCommitter) commitGrid(ctx context.Context, def *Definition, header []string, rows [][]string, fileName string, started time.Time, opts PullOptions) (PullOutcome, error) {
	dryRun, runBy := opts.DryRun, opts.RunBy
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
	var staged []importpkg.StagingRow
	var importErrs []*importpkgv1.ImportError
	if len(raw) > 0 {
		// Every dimension of the grid mapped, and no other, as a file run must
		// (integration_file_run.go): a pull naming fewer stored values keyed
		// without a grid dimension, read by no cell and no formula, and the run
		// reported success.
		if err := importpkg.CheckGridColumns(ctx, c.Pool, def.Config.TargetID, header); err != nil {
			return PullOutcome{}, err
		}
		var err error
		if staged, importErrs, err = importpkg.ResolveRows(ctx, c.Pool, def.ModelID, def.RevisionID, header, raw); err != nil {
			return PullOutcome{}, err
		}
	}
	out := PullOutcome{}
	// A model link's run clears what its source emptied (importpkg
	// CommitOptions) unless a record failed to resolve: then a missing cell
	// may be that failure, not an emptied value.
	reconcile := opts.Reconcile && def.Config.Protocol == ProtocolModel
	if reconcile && len(importErrs) > 0 {
		reconcile = false
		out.NotReconciled = fmt.Sprintf("%d record(s) failed: nothing the source emptied was cleared", len(importErrs))
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
	out.Skipped = len(importErrs)
	if dryRun {
		out.Written = len(staged)
		return out, nil
	}
	if len(staged) == 0 && !reconcile {
		return out, nil
	}
	store := importpkg.NewStore(c.Pool)
	job, err := store.CreateImportJob(ctx, def.ModelID, def.RevisionID, jobSource(def), runBy, nil)
	if err != nil {
		return PullOutcome{}, err
	}
	// Its line in Recent Imports names the grid, the mode and an SFTP
	// source's file.
	var gridName string
	_ = c.Pool.QueryRow(ctx, `SELECT name FROM model.grid_def WHERE id::text = $1`, def.Config.TargetID).Scan(&gridName)
	if err := store.DescribeJob(ctx, job.Id, importpkg.JobDetails{
		TargetType: "grid", TargetName: gridName, Mode: string(cmp.Or(def.Config.ImportMode, ModeIncremental)),
		FileName: fileName, StartedAt: started,
	}); err != nil {
		c.Log.Warn().Err(err).Msg("describe a connector's import job")
	}
	if len(staged) == 0 {
		err = store.StageNothing(ctx, job.Id)
	} else {
		err = store.StageRows(ctx, job.Id, staged, nil)
	}
	if err != nil {
		return PullOutcome{}, err
	}
	commitOpts := importpkg.CommitOptions{Reconcile: reconcile}
	if def.Config.Protocol == ProtocolModel {
		// The rows a link writes name it, in every revision of the model.
		commitOpts.LinkRef = def.LinkID
	}
	metricIDs, cleared, err := store.CommitImportWith(ctx, job.Id, def.ModelID, def.RevisionID, runBy, importpkg.ImportMode(def.Config.ImportMode), commitOpts)
	if err != nil {
		return PullOutcome{}, err
	}
	if len(metricIDs) > 0 {
		sched := calculation.NewScheduler(c.Log, calculation.NewStore(c.Pool))
		if rerr := sched.RecalcAffected(ctx, def.ModelID, def.RevisionID, metricIDs); rerr != nil {
			c.Log.Warn().Err(rerr).Msg("recalc after connector import")
		}
	}
	out.Written, out.Cleared = len(staged), cleared
	return out, nil
}

// commitDimension upserts members: code (required), label, parent_code,
// property:* columns — the same semantics importDimensionMembersCSV
// established, including property declaration upserts.
func (c *DBCommitter) commitDimension(ctx context.Context, def *Definition, header []string, rows [][]string, dryRun bool) (int, int, error) {
	dimID := def.Config.TargetID
	col := map[string]int{}
	for i, h := range header {
		key := strings.ToLower(strings.TrimSpace(h))
		// A property column keeps its name as written: the pull makes it
		// the property's label (metricformula.DeclareImportedProperties).
		if strings.HasPrefix(key, "property:") {
			key = "property:" + strings.TrimSpace(strings.TrimSpace(h)[len("property:"):])
		}
		col[key] = i
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
	// Each property:<name> column fills the property declared under that
	// name whatever its case, or under its identifier; any other column is
	// declared as its identifier, as text — the normalisation the CSV member
	// import shares (metricformula.DeclareImportedProperties). The header is
	// lower-cased above, and used to be declared as written: "factor" next
	// to the developer's "Factor", holding the pulled values where no
	// formula read them.
	var headers []string
	headerIdx := map[string]int{}
	for name, idx := range col {
		if p, isProp := strings.CutPrefix(name, "property:"); isProp && strings.TrimSpace(p) != "" {
			headers = append(headers, strings.TrimSpace(p))
			headerIdx[strings.TrimSpace(p)] = idx
		}
	}
	slices.Sort(headers)
	var names map[string]string
	var declared []string
	if dryRun {
		names, _, err = metricformula.ImportedPropertyNames(ctx, c.Pool, dimID, headers)
	} else {
		names, declared, err = metricformula.DeclareImportedProperties(ctx, c.Pool, dimID, headers)
	}
	if err != nil {
		return 0, 0, err
	}
	types, err := metricformula.DeclaredPropertyTypes(ctx, c.Pool, dimID)
	if err != nil {
		return 0, 0, err
	}
	// The members this pull gives a value under each property it declared:
	// the others holding one are values from before, which formulas read
	// again now.
	wroteValue := map[string][]string{}
	written, skipped := 0, 0
rowLoop:
	for _, r := range rows {
		code := strings.TrimSpace(r[codeIdx])
		if code == "" {
			skipped++
			continue
		}
		props := map[string]string{}
		replaced := []string{}
		for _, header := range headers {
			idx := headerIdx[header]
			if idx >= len(r) || r[idx] == "" {
				continue
			}
			name := names[header]
			// A value its number or date property cannot read skips the
			// row, as the connector's other bad rows are skipped.
			if metricformula.PropertyValueError(name, types[name], r[idx]) != nil {
				skipped++
				continue rowLoop
			}
			props[name] = r[idx]
			replaced = append(replaced, strings.ToLower(name))
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
		propJSON, _ := json.Marshal(props)
		if _, err := c.Pool.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, sort_order, properties)
			VALUES ($1::uuid, $2, $3, $4::uuid, (SELECT COALESCE(MAX(sort_order),0)+1 FROM model.dimension_member WHERE dimension_id=$1::uuid), $5::jsonb)
			ON CONFLICT (dimension_id, code) DO UPDATE SET
			    label=EXCLUDED.label,
			    parent_member_id=COALESCE(EXCLUDED.parent_member_id, model.dimension_member.parent_member_id),
			    properties=`+metricformula.MergeImportedPropertiesSQL("$6")+`
		`, dimID, code, label, parentID, string(propJSON), replaced); err != nil {
			c.Log.Warn().Err(err).Str("dimension_id", dimID).Str("code", code).Msg("connector dimension pull: member skipped")
			skipped++
			continue
		}
		for name := range props {
			wroteValue[name] = append(wroteValue[name], code)
		}
		written++
	}
	// A property the pull declared again brings back the values members
	// held under its name; nothing clears them unless the developer asks
	// (the property's Clear values), so the run's log says how many.
	for _, name := range declared {
		n, err := metricformula.ValuesFromBefore(ctx, c.Pool, dimID, name, wroteValue[name])
		if err != nil {
			return written, skipped, err
		}
		if note := metricformula.PreexistingValuesNote(name, n); note != "" {
			c.Log.Warn().Str("dimension_id", dimID).Str("property", name).Int("members", n).
				Msg("connector dimension pull: " + note + " — clear them with Clear values on the property if they are stale")
		}
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

// ErrModelLinkTableNeedsKey refuses a model link into a table that would
// add the source's rows again at every run: the link reads the source's
// whole rows each time, and a table that keeps no dimension adds a loaded
// row as a new record unless the load replaces by a field (replace_by). A
// run of two source rows left 4 records after two runs (2026-11-09).
var ErrModelLinkTableNeedsKey = errors.New("a model link into a table needs \"When a run brings rows again\" set to replace the records with the same value of a field: " +
	"each run reads the source's whole rows, which would otherwise be added again as new records")

// CheckModelLinkTable is the rule for a model link into a table: it replaces
// by a field, unless the table keeps a dimension, whose records a load
// already adds and updates by their code. Any other connector, or a model
// link into a grid or a dimension, passes.
func CheckModelLinkTable(cfg *Config, form *crudapp.FormDef) error {
	if cfg == nil || cfg.Protocol != ProtocolModel || cfg.TargetType != TargetForm || form == nil {
		return nil
	}
	if strings.TrimSpace(cfg.ReplaceBy) == "" && !form.KeepsDimension() {
		return ErrModelLinkTableNeedsKey
	}
	return nil
}

// commitForm creates a record of the target table for each row through
// recordpost.ImportRows — the checks and posting a record saved by hand
// gets: its values, where it may post within the run principal's access,
// the next auto-code, its posting into metrics and the rules a new record
// fires. A row that fails is skipped and counted, as a grid target's is.
// The run's principal is the developer who set the connector up, so a row
// may name any status a table with a status field has.
func (c *DBCommitter) commitForm(ctx context.Context, def *Definition, header []string, rows [][]string, dryRun bool, runBy string) (int, int, error) {
	form, err := crudapp.NewStore(c.Pool).GetForm(ctx, def.Config.TargetID)
	if err != nil {
		return 0, 0, err
	}
	if err := CheckModelLinkTable(def.Config, form); err != nil {
		return 0, 0, err
	}
	var appID, revisionID string
	if err := c.Pool.QueryRow(ctx, `
		SELECT m.application_id::text, COALESCE(fd.revision_id::text, '')
		FROM model.form_def fd JOIN core.model m ON m.id = fd.model_id
		WHERE fd.id = $1::uuid`, form.ID).Scan(&appID, &revisionID); err != nil {
		return 0, 0, err
	}
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
	newStatus := ""
	if form.HasStatus() {
		newStatus = crudapp.StatusDraft
	}
	written, rowErrs, err := recordpost.OnPool(c.Pool, c.Log).ImportRows(ctx, recordpost.ImportRequest{
		Form: form, CreatorID: runBy, Header: header, Rows: raw, NewStatus: newStatus,
		StatusRefusal: func(string) string { return "" },
		AppID:         appID, RevisionID: revisionID,
		DryRun: dryRun, Inline: true, Partial: true,
		// A run replacing by a field is whole or nothing: a period whose
		// rows partly failed would otherwise lose the records they replace.
		ReplaceBy: def.Config.ReplaceBy,
	})
	if err != nil {
		return written, len(rowErrs), err
	}
	for i, e := range rowErrs {
		if i == 5 {
			c.Log.Warn().Int("more", len(rowErrs)-i).Str("integration", def.ID).Msg("connector rows skipped")
			break
		}
		c.Log.Warn().Int("row", e.Row).Str("column", e.Column).Str("problem", e.Message).Str("integration", def.ID).Msg("connector row skipped")
	}
	return written, len(rowErrs), nil
}

// LoadPushRows reads the push source rows.
//   - dimension: one row per member (code/label/parent_code + property:*).
//   - form: one row per form record (its data keys).
//   - grid: one wide row per dim-combo over the mapped metrics, reading the
//     grid's deterministic latest-wins facts.
func (c *DBCommitter) LoadPushRows(ctx context.Context, def *Definition) ([]map[string]string, error) {
	// The last line before a read that leaves the process: never push
	// another model's rows.
	if err := checkDefinitionOwnership(ctx, c.Pool, def); err != nil {
		return nil, err
	}
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
