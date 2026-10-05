package gateway

// The AI Developer's file import and export preview: the gateway side of
// aiassistant's preview_file_import, import_file_data and preview_export.
// A spreadsheet attached to the chat goes through the Import Wizard's own
// server pipeline — importpkg's parser and column map, ResolveRows,
// CommitImport (write guard), the plan's fact and storage limits,
// recalculation and audit — into the session's working revision; a
// dimension import through importDimensionMembersCSV, as a saved
// integration run does.

import (
	"cmp"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/dataexport"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// maxAIImportBytes is the largest attachment kept for import — the limit
// POST /api/import/upload sets on a file.
const maxAIImportBytes = 16 << 20

// attachedFile is an attachment parsed, reshaped and column-mapped for import.
type attachedFile struct {
	docID    string
	filename string
	sheet    string
	sheets   []string
	grid     [][]string // the sheet as read, header row included
	reshaped bool
	header   []string // after the reshape (the file's own when there is none)
	mapped   []string // after the column map
	rows     []importpkg.RawRow
}

func (h *handler) loadAttachedFile(ctx context.Context, sessionID string, req aiassistant.FileImportRequest) (*attachedFile, error) {
	store := aiassistant.NewDocumentStore(h.db.For(ctx))
	doc, raw, err := store.GetRaw(ctx, sessionID, strings.TrimSpace(req.File))
	if err != nil {
		var names []string
		if docs, lErr := store.ListDocuments(ctx, sessionID); lErr == nil {
			for _, d := range docs {
				if d.Importable {
					names = append(names, d.Filename)
				}
			}
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("no spreadsheet named %q is attached to this chat — ask the developer to attach the .xlsx or .csv file", req.File)
		}
		return nil, fmt.Errorf("no file named %q is attached to this chat — attached spreadsheets: %s", req.File, strings.Join(names, ", "))
	}
	if !doc.Importable || raw == nil {
		return nil, fmt.Errorf("%s cannot be imported: only .xlsx, .xlsm and .csv files up to 16 MB attached since file import was added are kept whole — ask the developer to attach it again", doc.Filename)
	}
	f := &attachedFile{docID: doc.ID, filename: doc.Filename, sheet: req.Sheet}
	if strings.HasSuffix(strings.ToLower(doc.Filename), ".xlsx") || strings.HasSuffix(strings.ToLower(doc.Filename), ".xlsm") {
		if f.sheets, err = importpkg.XLSXSheetNames(raw); err != nil {
			return nil, err
		}
		if f.sheet == "" && len(f.sheets) > 0 {
			f.sheet = f.sheets[0]
		}
	}
	if err := req.Reshape.Validate(); err != nil {
		return nil, err
	}
	delim, err := req.Reshape.Comma()
	if err != nil {
		return nil, err
	}
	if f.grid, err = importpkg.ReadGrid(doc.Filename, raw, req.Sheet, delim); err != nil {
		return nil, err
	}
	f.reshaped = !req.Reshape.IsZero()
	header, rows, err := importpkg.ShapeGrid(f.grid, req.Reshape)
	if err != nil {
		return nil, err
	}
	f.header = append([]string(nil), header...)
	if missing := importpkg.UnmatchedColumnMapKeys(header, req.ColumnMap); len(missing) > 0 {
		sort.Strings(missing)
		after := ""
		if f.reshaped {
			after = " after the reshape"
		}
		return nil, fmt.Errorf("column_map names column(s) %s that %s does not have%s — its columns are: %s",
			strings.Join(missing, ", "), f.describe(), after, strings.Join(header, ", "))
	}
	if f.mapped, err = importpkg.ApplyColumnMap(header, rows, req.ColumnMap); err != nil {
		return nil, err
	}
	f.rows = rows
	return f, nil
}

// sampleLines renders up to n rows for the assistant, each cell clipped, so
// it can see a sheet's layout (where the header is, what runs across).
func sampleLines(rows [][]string, n int) string {
	var sb strings.Builder
	for i, r := range rows {
		if i == n {
			fmt.Fprintf(&sb, "  … %d more row(s)\n", len(rows)-n)
			break
		}
		cells := r
		if len(cells) > 14 {
			cells = append(append([]string(nil), cells[:14]...), fmt.Sprintf("… %d more", len(r)-14))
		}
		clipped := make([]string, len(cells))
		for j, c := range cells {
			c = strings.TrimSpace(c)
			if len([]rune(c)) > 32 {
				c = string([]rune(c)[:31]) + "…"
			}
			clipped[j] = fmt.Sprintf("%q", c)
		}
		fmt.Fprintf(&sb, "  %d: %s\n", i+1, strings.Join(clipped, ", "))
	}
	return sb.String()
}

// mappedSample is the first n rows as an import reads them.
func (f *attachedFile) mappedSample(n int) [][]string {
	out := make([][]string, 0, n)
	for i, r := range f.rows {
		if i == n {
			break
		}
		line := make([]string, len(f.mapped))
		for j, c := range f.mapped {
			line[j] = r.Cells[c]
		}
		out = append(out, line)
	}
	return out
}

func (f *attachedFile) describe() string {
	if f.sheet != "" {
		return fmt.Sprintf("%s (sheet %q)", f.filename, f.sheet)
	}
	return f.filename
}

func (h *handler) modelOfRevision(ctx context.Context, revisionID string) (string, error) {
	var modelID string
	if err := h.db.QueryRow(ctx, `SELECT model_id::text FROM model.revision WHERE id=$1::uuid`, revisionID).Scan(&modelID); err != nil {
		return "", fmt.Errorf("working revision not found")
	}
	return modelID, nil
}

// modelNamesHint lists the names a grid import's columns may carry.
func (h *handler) modelNamesHint(ctx context.Context, q dbQuerier, modelID, revisionID string) string {
	var metrics, dims []string
	if rows, err := q.Query(ctx, `SELECT name FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND is_input ORDER BY name`, modelID, revisionID); err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				metrics = append(metrics, n)
			}
		}
		rows.Close()
	}
	if rows, err := q.Query(ctx, `SELECT name FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid ORDER BY name`, modelID, revisionID); err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				dims = append(dims, n)
			}
		}
		rows.Close()
	}
	return fmt.Sprintf("input metrics: %s; dimensions: %s", strings.Join(metrics, ", "), strings.Join(dims, ", "))
}

// importErrorLines formats up to max row errors for the assistant.
func importErrorLines(errs []map[string]any, limit int) string {
	var sb strings.Builder
	for i, e := range errs {
		if i == limit {
			fmt.Fprintf(&sb, "  … and %d more\n", len(errs)-limit)
			break
		}
		fmt.Fprintf(&sb, "  row %v, column %v: %v\n", e["row"], e["column"], e["message"])
	}
	return sb.String()
}

// resolveAttachedGrid runs ResolveRows: staged rows, or the problems.
func (h *handler) resolveAttachedGrid(ctx context.Context, q dbQuerier, modelID, revisionID, gridID string, f *attachedFile) ([]importpkg.StagingRow, []map[string]any, error) {
	if err := importpkg.CheckGridColumns(ctx, q, gridID, f.mapped); err != nil {
		return nil, nil, err
	}
	staged, importErrs, err := importpkg.ResolveRows(ctx, q, modelID, revisionID, f.mapped, f.rows)
	if err != nil {
		return nil, nil, fmt.Errorf("%w. Map each column to a model name or \"ignore\" in column_map, or add \"*\": \"ignore\" to drop every column it does not name — %s", err, h.modelNamesHint(ctx, q, modelID, revisionID))
	}
	errRows := make([]map[string]any, 0, len(importErrs))
	for _, e := range importErrs {
		errRows = append(errRows, map[string]any{"row": e.RowNumber, "column": e.Column, "message": e.Message})
	}
	return staged, errRows, nil
}

// dimensionCSV is the mapped file as the dimension importer reads it, with
// every column checked to be a dimension field — an attachment import is
// all-or-nothing, so a stray column is refused rather than ignored.
func dimensionCSV(f *attachedFile) (*csv.Reader, map[string]int, error) {
	cr, colIdx, err := rowsCSV(f.mapped, f.rows)
	if err != nil {
		return nil, nil, err
	}
	if _, ok := colIdx["label"]; !ok {
		if _, ok := colIdx["code"]; !ok {
			return nil, nil, fmt.Errorf("a dimension import needs a \"label\" or \"code\" column — map one in column_map (columns: %s)", strings.Join(f.header, ", "))
		}
	}
	for col := range colIdx {
		switch {
		case col == "code", col == "label", col == "parent_code", col == "period_start", col == "period_end", strings.HasPrefix(col, "property:"):
		default:
			return nil, nil, fmt.Errorf("column %q is not a dimension field — map it to code, label, parent_code, property:<name> (or period_start/period_end) or \"ignore\"", col)
		}
	}
	return cr, colIdx, nil
}

// newMemberCount is how many of a dimension file's rows would add a member
// (a code not in the dimension yet, or no code — one is generated).
func (h *handler) newMemberCount(ctx context.Context, q dbQuerier, dimensionID string, f *attachedFile) (added, updated int) {
	existing := map[string]bool{}
	if rows, err := q.Query(ctx, `SELECT code FROM model.dimension_member WHERE dimension_id=$1::uuid`, dimensionID); err == nil {
		for rows.Next() {
			var c string
			if rows.Scan(&c) == nil {
				existing[c] = true
			}
		}
		rows.Close()
	}
	seen := map[string]bool{}
	for _, r := range f.rows {
		code := strings.TrimSpace(r.Cells["code"])
		switch {
		case code == "":
			added++
		case existing[code]:
			updated++
		case !seen[code]:
			added++
		}
		seen[code] = true
	}
	return added, updated
}

// dbQuerier is what the import preview and checks read with: the pool, or
// the plan check's transaction, where earlier steps of a plan exist.
type dbQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (h *handler) aiPreviewFileImport(ctx context.Context, sessionID string, req aiassistant.FileImportRequest) (string, error) {
	return h.aiPreviewFileImportOn(ctx, h.db.For(ctx), sessionID, req)
}

func (h *handler) aiPreviewFileImportOn(ctx context.Context, q dbQuerier, sessionID string, req aiassistant.FileImportRequest) (string, error) {
	f, err := h.loadAttachedFile(ctx, sessionID, req)
	if err != nil {
		return "", err
	}
	modelID, err := h.modelOfRevision(ctx, req.RevisionID)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString(f.layout(req))

	if req.TargetType == "dimension" {
		if _, _, err := dimensionCSV(f); err != nil {
			return sb.String() + "Cannot import: " + err.Error(), nil
		}
		added, updated := h.newMemberCount(ctx, q, req.TargetID, f)
		fmt.Fprintf(&sb, "Would add %d member(s) and update %d existing one(s). Rows with neither code nor label are skipped.\n", added, updated)
		return sb.String(), nil
	}

	staged, errRows, err := h.resolveAttachedGrid(ctx, q, modelID, req.RevisionID, req.TargetID, f)
	if err != nil {
		return sb.String() + "Cannot import: " + err.Error() + "\n" + h.suggestColumnMap(ctx, q, req, f), nil
	}
	if len(errRows) > 0 {
		fmt.Fprintf(&sb, "%d value(s) fail validation — an import is all-or-nothing, so nothing would be imported until they are fixed:\n%s",
			len(errRows), importErrorLines(errRows, 15))
		return sb.String(), nil
	}
	if len(staged) == 0 {
		sb.WriteString("No values to import: no column holds a metric's values (map value columns to metric names, or a metric-name column to \"metric\" and the amounts to \"value\").\n")
		return sb.String(), nil
	}
	perMetric := map[string]int{}
	for _, s := range staged {
		perMetric[s.MetricID]++
	}
	var parts []string
	for id, n := range perMetric {
		var name string
		_ = q.QueryRow(ctx, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, id).Scan(&name)
		parts = append(parts, fmt.Sprintf("%s (%d)", name, n))
	}
	sort.Strings(parts)
	fmt.Fprintf(&sb, "No errors. Would import %d value(s): %s.\n", len(staged), strings.Join(parts, ", "))
	sb.WriteString(h.percentFractionWarning(ctx, q, staged))
	return sb.String(), nil
}

// layout describes the file as read and as the import will read it: the
// sheet's first rows, then — after the reshape and column map — its columns
// and first rows. It is what lets the assistant see a layout to reshape.
func (f *attachedFile) layout(req aiassistant.FileImportRequest) string {
	var sb strings.Builder
	if len(f.sheets) > 1 {
		fmt.Fprintf(&sb, "Workbook sheets: %s (pass \"sheet\" to choose another)\n", strings.Join(f.sheets, ", "))
	}
	fmt.Fprintf(&sb, "%s as read, first rows:\n%s", f.describe(), sampleLines(f.grid, 8))
	if f.reshaped {
		fmt.Fprintf(&sb, "After the reshape: %d row(s); columns: %s\n", len(f.rows), strings.Join(f.header, ", "))
	} else {
		fmt.Fprintf(&sb, "%d data row(s) under the header (row 1); columns: %s\n", len(f.rows), strings.Join(f.header, ", "))
		if s := importpkg.SuggestReshape(f.grid); s != nil {
			js, _ := json.Marshal(s)
			fmt.Fprintf(&sb, "This sheet is laid out for people. Suggested \"reshape\": %s — then map the columns it leaves "+
				"(the dimension columns to their dimensions, \"Period\" to the time dimension, \"Value\" to the metric or "+
				"\"value\" beside a \"metric\" constant, totals and notes to \"ignore\") and preview again.\n", js)
		}
	}
	if len(req.ColumnMap) > 0 {
		fmt.Fprintf(&sb, "Columns after column_map: %s\n", strings.Join(f.mapped, ", "))
	}
	if f.reshaped || len(req.ColumnMap) > 0 {
		fmt.Fprintf(&sb, "First rows as the import reads them:\n%s", sampleLines(f.mappedSample(5), 5))
	}
	return sb.String()
}

func (h *handler) aiImportFile(ctx context.Context, act *actor, sessionID string, req aiassistant.FileImportRequest) (string, error) {
	q := h.db.For(ctx)
	f, err := h.loadAttachedFile(ctx, sessionID, req)
	if err != nil {
		return "", err
	}
	modelID, err := h.modelOfRevision(ctx, req.RevisionID)
	if err != nil {
		return "", err
	}
	var appID string
	_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
	audit := func(imported int) {
		meta := map[string]string{"source": "ai_assistant", "session_id": sessionID, "filename": f.filename,
			"rows_imported": fmt.Sprintf("%d", imported), "target_type": req.TargetType, "target_id": req.TargetID}
		if f.sheet != "" {
			meta["sheet"] = f.sheet
		}
		resType, resID := req.TargetType, req.TargetID
		if req.IntegrationID != "" {
			meta["integration_id"] = req.IntegrationID
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryDataChange, EventType: auditlog.EventImportUploaded,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ApplicationID: appID, ResourceType: resType, ResourceID: resID, RevisionID: req.RevisionID,
			Metadata: meta,
		})
	}
	fail := func(err error) (string, error) {
		if req.IntegrationID != "" {
			h.recordIntegrationRun(ctx, req.IntegrationID, act.UserID, 0, 0, "error", err.Error())
		}
		return "", err
	}

	if req.TargetType == "dimension" {
		cr, colIdx, err := dimensionCSV(f)
		if err != nil {
			return fail(err)
		}
		if cid := h.customerOfModel(ctx, modelID); cid != "" && h.plans != nil {
			added, _ := h.newMemberCount(ctx, q, req.TargetID, f)
			if err := h.plans.CheckMembers(ctx, h.db.For(ctx), cid, req.TargetID, added); err != nil {
				return fail(err)
			}
		}
		imported, errs, err := h.importDimensionMembersCSV(ctx, req.TargetID, cr, colIdx)
		if err != nil {
			return fail(err)
		}
		if req.IntegrationID != "" {
			h.recordIntegrationRun(ctx, req.IntegrationID, act.UserID, imported, errs, "success", "AI import of "+f.describe())
		}
		audit(imported)
		var dimName string
		_ = h.db.QueryRow(ctx, `SELECT name FROM model.dimension_def WHERE id=$1::uuid`, req.TargetID).Scan(&dimName)
		msg := fmt.Sprintf("Imported %d member(s) from %s into dimension '%s'", imported, f.describe(), dimName)
		if errs > 0 {
			msg += fmt.Sprintf(" (%d row(s) skipped: no code or label)", errs)
		}
		return msg, nil
	}

	// Grid: the same checks and commit as POST /api/import/upload, into
	// the working revision.
	systemManaged, err := writeguard.SystemManaged(ctx, h.db.For(ctx), req.RevisionID)
	if err != nil {
		return fail(fmt.Errorf("check system-managed: %w", err))
	}
	if systemManaged {
		return fail(fmt.Errorf("this revision is system-managed and read-only"))
	}
	staged, errRows, err := h.resolveAttachedGrid(ctx, q, modelID, req.RevisionID, req.TargetID, f)
	if err != nil {
		return fail(err)
	}
	if len(errRows) > 0 {
		return fail(fmt.Errorf("import rejected: %d value(s) in %s failed validation, nothing was imported:\n%s",
			len(errRows), f.describe(), importErrorLines(errRows, 10)))
	}
	if len(staged) == 0 {
		return fail(fmt.Errorf("%s holds no values for any metric", f.describe()))
	}
	mode := importpkg.ImportMode(cmp.Or(req.ImportMode, string(importpkg.ModeReplace)))
	metricIDs, err := h.commitGridRows(ctx, act, modelID, req.RevisionID, staged, mode, "ai_attachment")
	if err != nil {
		return fail(err)
	}
	if req.IntegrationID != "" {
		h.recordIntegrationRun(ctx, req.IntegrationID, act.UserID, len(staged), 0, "success", "AI import of "+f.describe())
	}
	audit(len(staged))
	return fmt.Sprintf("Imported %d value(s) from %s into %d metric(s) (mode %s)", len(staged), f.describe(), len(metricIDs), mode) +
		h.percentFractionWarning(ctx, q, staged), nil
}

// aiPreviewExport renders an export spec for the assistant, as text.
func (h *handler) aiPreviewExport(r *http.Request, gridID, name string, spec dataexport.Spec) (string, error) {
	ctx := r.Context()
	g, _, gridRev, err := dataexport.LoadGrid(ctx, h.db.For(ctx), gridID)
	if err != nil {
		return "", err
	}
	if problems := dataexport.Validate(spec, g); len(problems) > 0 {
		return "The spec has problems — fix all of them:\n- " + strings.Join(problems, "\n- "), nil
	}
	t, err := h.renderExport(r, gridID, gridRev, spec)
	if err != nil {
		return "", err
	}
	if name == "" {
		name = g.Name
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "File %s (%s): %d row(s)\n", dataexport.FileName(spec, name), dataexport.Describe(spec), len(t.Rows))
	fmt.Fprintf(&sb, "Columns: %s\n", strings.Join(t.Header, " | "))
	for _, w := range t.Warnings {
		fmt.Fprintf(&sb, "Warning: %s\n", w)
	}
	rows := t.TextRows(10)
	if len(rows) == 0 {
		sb.WriteString("No rows: the grid has no values matching this spec yet.\n")
	} else {
		sb.WriteString("First rows:\n")
		for _, row := range rows {
			sb.WriteString("  " + strings.Join(row, " | ") + "\n")
		}
	}
	return sb.String(), nil
}

// aiReadHooks binds the read tools' gateway operations to this chat request.
func (h *handler) aiReadHooks(r *http.Request, sessionID, modelID, userID string) aiassistant.ReadHooks {
	return aiassistant.ReadHooks{
		PreviewFileImport: func(ctx context.Context, req aiassistant.FileImportRequest) (string, error) {
			if len(req.AfterSteps) > 0 {
				return h.aiPreviewAfterSteps(ctx, sessionID, modelID, userID, req)
			}
			return h.aiPreviewFileImport(ctx, sessionID, req)
		},
		PreviewExport: func(ctx context.Context, revisionID, gridID, name string, spec dataexport.Spec) (string, error) {
			return h.aiPreviewExport(r.WithContext(ctx), gridID, name, spec)
		},
		PrepareConversion: func(ctx context.Context, req aiassistant.FileImportRequest) (string, error) {
			return h.aiPrepareConversion(ctx, sessionID, req)
		},
		ReadAttachedSheet: func(ctx context.Context, file, sheet string, fromRow, toRow int) (string, error) {
			return h.aiReadAttachedSheet(ctx, sessionID, file, sheet, fromRow, toRow)
		},
	}
}

// aiReadAttachedSheet renders rows of one sheet of a workbook attached to
// the chat (read_attached_sheet).
func (h *handler) aiReadAttachedSheet(ctx context.Context, sessionID, file, sheet string, fromRow, toRow int) (string, error) {
	doc, raw, err := aiassistant.NewDocumentStore(h.db.For(ctx)).GetRaw(ctx, sessionID, strings.TrimSpace(file))
	if err != nil {
		return "", fmt.Errorf("no file named %q is attached to this chat", file)
	}
	name := strings.ToLower(doc.Filename)
	if raw == nil || (!strings.HasSuffix(name, ".xlsx") && !strings.HasSuffix(name, ".xlsm")) {
		return "", fmt.Errorf("%s is not a workbook kept whole (.xlsx or .xlsm attached since file import was added) — its text is under Attached documents", doc.Filename)
	}
	return aiassistant.ExtractXLSXSheet(raw, sheet, fromRow, toRow)
}

// percentFractionWarning names the Percentage metrics whose every staged
// value lies between -1 and 1. A Percentage metric stores percent units
// (5.6 shows as 5.6%, and formulas divide by 100); a spreadsheet stores the
// fraction 0.056, which would show as 0.06% and be read as 0.056%.
func (h *handler) percentFractionWarning(ctx context.Context, q dbQuerier, staged []importpkg.StagingRow) string {
	maxAbs := map[string]float64{}
	for _, s := range staged {
		v := s.Value
		if v < 0 {
			v = -v
		}
		if v > maxAbs[s.MetricID] {
			maxAbs[s.MetricID] = v
		}
	}
	var names []string
	for id, m := range maxAbs {
		if m == 0 || m > 1 {
			continue
		}
		var name, format string
		if q.QueryRow(ctx, `SELECT name, format FROM model.metric_def WHERE id=$1::uuid`, id).Scan(&name, &format) == nil && format == "percentage" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return fmt.Sprintf("\nWARNING: every value for %s is a fraction (at most 1). A Percentage metric stores percent units (5.6 = 5.6%%) "+
		"and formulas divide it by 100 — add \"scale\": {\"<the value column>\": 100} to the reshape unless these really are values under 1%%.\n",
		strings.Join(names, ", "))
}

// suggestColumnMap proposes a column_map for a grid import whose columns do
// not all name the model: a column named like one of the grid's dimensions
// maps to it, the "Period" column of a suggested unpivot to the grid's time
// dimension, a metric name stays, and anything else is ignored — except the
// amounts, which only the assistant can attribute to a metric. Each failed
// preview costs the assistant an LLM call; the live run spent its session's
// budget mapping "FY", "Source / Note" and "P&L Line" one at a time.
func (h *handler) suggestColumnMap(ctx context.Context, q dbQuerier, req aiassistant.FileImportRequest, f *attachedFile) string {
	if req.TargetType != "grid" || req.TargetID == "" {
		return ""
	}
	dims := map[string]string{} // lower name -> name
	timeDim := ""
	rows, err := q.Query(ctx, `SELECT d.name, d.dimension_type = 'time' FROM model.grid_dimension gd
		JOIN model.dimension_def d ON d.id = gd.dimension_id WHERE gd.grid_id = $1::uuid`, req.TargetID)
	if err != nil {
		return ""
	}
	for rows.Next() {
		var name string
		var isTime bool
		if rows.Scan(&name, &isTime) == nil {
			dims[strings.ToLower(name)] = name
			if isTime {
				timeDim = name
			}
		}
	}
	rows.Close()
	metrics := map[string]bool{}
	mrows, err := q.Query(ctx, `SELECT lower(m.name) FROM model.grid_metric gm JOIN model.metric_def m ON m.id = gm.metric_id WHERE gm.grid_id = $1::uuid`, req.TargetID)
	if err == nil {
		for mrows.Next() {
			var n string
			if mrows.Scan(&n) == nil {
				metrics[n] = true
			}
		}
		mrows.Close()
	}
	// Worked out from the grid, not echoed from the map the assistant sent:
	// its own mapping is kept only where it names something on the grid.
	onGrid := func(target string) bool {
		l := strings.ToLower(target)
		return dims[l] != "" || metrics[l] || l == "metric" || l == "value"
	}
	suggested := map[string]string{}
	var amounts []string
	for _, col := range f.header {
		key := strings.ToLower(strings.TrimSpace(col))
		sent := req.ColumnMap[col]
		switch {
		case dims[key] != "":
			suggested[col] = dims[key]
		case (key == "period" || key == "month") && timeDim != "":
			suggested[col] = timeDim
		case sent != "" && sent != "ignore" && onGrid(sent):
			suggested[col] = sent
		case metrics[key]:
		case key == "metric" || key == "value":
			amounts = append(amounts, col)
		default:
			suggested[col] = "ignore"
		}
	}
	js, _ := json.Marshal(suggested)
	msg := fmt.Sprintf("Suggested \"column_map\" for this grid: %s", js)
	if len(amounts) > 0 {
		msg += fmt.Sprintf(" — and map %s to the metric these amounts are (or keep \"value\" with a \"metric\" constant in the reshape)", strings.Join(amounts, ", "))
	}
	return msg + ".\n"
}

// recallCleanPreview finds this session's latest preview_file_import of the
// file's sheet whose result reported no errors, and returns its target,
// reshape and column map as the assistant wrote them (names resolve again
// on import).
func (h *handler) recallCleanPreview(ctx context.Context, sessionID, file, sheet string) (aiassistant.FileImportRequest, bool) {
	msgs, err := aiassistant.NewChatStore(h.db.For(ctx)).ListMessages(ctx, sessionID)
	if err != nil {
		return aiassistant.FileImportRequest{}, false
	}
	results := map[string]string{}
	for _, m := range msgs {
		if m.Role == "tool" {
			results[m.ToolCallID] = m.Content
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		for _, tc := range msgs[i].ToolCalls {
			if tc.Name != "preview_file_import" || !strings.Contains(results[tc.ID], "No errors.") {
				continue
			}
			var p struct {
				File       string             `json:"file"`
				Sheet      string             `json:"sheet"`
				TargetType string             `json:"target_type"`
				TargetID   string             `json:"target_id"`
				Reshape    *importpkg.Reshape `json:"reshape"`
				ColumnMap  map[string]string  `json:"column_map"`
			}
			if json.Unmarshal(tc.Arguments, &p) != nil || !strings.EqualFold(p.File, file) || !strings.EqualFold(p.Sheet, sheet) {
				continue
			}
			return aiassistant.FileImportRequest{TargetType: p.TargetType, TargetID: p.TargetID, Reshape: p.Reshape, ColumnMap: p.ColumnMap}, true
		}
	}
	return aiassistant.FileImportRequest{}, false
}

// aiPreviewAfterSteps previews an import into what a plan's earlier steps
// create: the steps run as the plan check runs them, in a transaction that
// is rolled back, and the preview reads that transaction. Without it a file
// could not be previewed into a grid or dimension the same proposal makes,
// so loading data took a proposal of its own.
func (h *handler) aiPreviewAfterSteps(ctx context.Context, sessionID, modelID, userID string, req aiassistant.FileImportRequest) (string, error) {
	tx, err := h.db.For(ctx).Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	check, created, err := h.runStepsOn(ctx, tx, sessionID, modelID, req.RevisionID, userID, req.AfterSteps)
	if err != nil {
		return "", err
	}
	if len(check.problems) > 0 {
		return "Nothing previewed: the after_steps would fail as they are —\n- " + strings.Join(check.problems, "\n- "), nil
	}
	target := req.TargetID
	if m := afterStepRef.FindStringSubmatch(target); m != nil {
		n, _ := strconv.Atoi(m[1])
		if n < 1 || n > len(created) || created[n-1] == "" {
			return "", fmt.Errorf("target_id %q names no step of after_steps that creates something", target)
		}
		target = created[n-1]
	}
	table := "model.grid_def"
	if req.TargetType == "dimension" {
		table = "model.dimension_def"
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM `+table+` WHERE revision_id=$1::uuid AND (id::text=$2 OR lower(name)=lower($2)) LIMIT 1`,
		req.RevisionID, strings.TrimSpace(target)).Scan(&id); err != nil {
		return "", fmt.Errorf("%s %q is neither in the working revision nor created by after_steps", req.TargetType, target)
	}
	req.TargetID = id
	out, err := h.aiPreviewFileImportOn(ctx, tx, sessionID, req)
	if err != nil {
		return "", err
	}
	note := fmt.Sprintf("(Previewed after the %d step(s) of after_steps, in a dry run: nothing was kept. Propose those steps and the import together.)\n", len(req.AfterSteps))
	if len(check.unchecked) > 0 {
		note += fmt.Sprintf("(Step(s) %v could not run in a dry run; the preview does not include them.)\n", check.unchecked)
	}
	return note + out, nil
}

var afterStepRef = regexp.MustCompile(`^\s*<created in step (\d+)>\s*$`)
