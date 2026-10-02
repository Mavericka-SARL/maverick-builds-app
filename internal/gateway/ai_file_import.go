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
	"fmt"
	"net/http"
	"sort"
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

// attachedFile is an attachment parsed and column-mapped for import.
type attachedFile struct {
	filename string
	sheet    string
	sheets   []string
	header   []string // as in the file
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
	f := &attachedFile{filename: doc.Filename, sheet: req.Sheet}
	if strings.HasSuffix(strings.ToLower(doc.Filename), ".xlsx") || strings.HasSuffix(strings.ToLower(doc.Filename), ".xlsm") {
		if f.sheets, err = importpkg.XLSXSheetNames(raw); err != nil {
			return nil, err
		}
		if f.sheet == "" && len(f.sheets) > 0 {
			f.sheet = f.sheets[0]
		}
	}
	header, rows, err := importpkg.ParseTabularFile(doc.Filename, raw, req.Sheet)
	if err != nil {
		return nil, err
	}
	f.header = append([]string(nil), header...)
	if missing := importpkg.UnmatchedColumnMapKeys(header, req.ColumnMap); len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("column_map names column(s) %s that %s does not have — its columns are: %s",
			strings.Join(missing, ", "), f.describe(), strings.Join(header, ", "))
	}
	if f.mapped, err = importpkg.ApplyColumnMap(header, rows, req.ColumnMap); err != nil {
		return nil, err
	}
	f.rows = rows
	return f, nil
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
func (h *handler) modelNamesHint(ctx context.Context, modelID, revisionID string) string {
	var metrics, dims []string
	if rows, err := h.db.Query(ctx, `SELECT name FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND is_input ORDER BY name`, modelID, revisionID); err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				metrics = append(metrics, n)
			}
		}
		rows.Close()
	}
	if rows, err := h.db.Query(ctx, `SELECT name FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid ORDER BY name`, modelID, revisionID); err == nil {
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
func (h *handler) resolveAttachedGrid(ctx context.Context, modelID, revisionID string, f *attachedFile) ([]importpkg.StagingRow, []map[string]any, error) {
	staged, importErrs, err := importpkg.ResolveRows(ctx, h.db.For(ctx), modelID, revisionID, f.mapped, f.rows)
	if err != nil {
		return nil, nil, fmt.Errorf("%w. Map each column to a model name or \"ignore\" in column_map — %s", err, h.modelNamesHint(ctx, modelID, revisionID))
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
func (h *handler) newMemberCount(ctx context.Context, dimensionID string, f *attachedFile) (added, updated int) {
	existing := map[string]bool{}
	if rows, err := h.db.Query(ctx, `SELECT code FROM model.dimension_member WHERE dimension_id=$1::uuid`, dimensionID); err == nil {
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

func (h *handler) aiPreviewFileImport(ctx context.Context, sessionID string, req aiassistant.FileImportRequest) (string, error) {
	f, err := h.loadAttachedFile(ctx, sessionID, req)
	if err != nil {
		return "", err
	}
	modelID, err := h.modelOfRevision(ctx, req.RevisionID)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "File %s: %d data row(s); columns: %s\n", f.describe(), len(f.rows), strings.Join(f.header, ", "))
	if len(f.sheets) > 1 {
		fmt.Fprintf(&sb, "Workbook sheets: %s (pass \"sheet\" to choose another)\n", strings.Join(f.sheets, ", "))
	}
	if len(req.ColumnMap) > 0 {
		fmt.Fprintf(&sb, "Columns after column_map: %s\n", strings.Join(f.mapped, ", "))
	}

	if req.TargetType == "dimension" {
		if _, _, err := dimensionCSV(f); err != nil {
			return sb.String() + "Cannot import: " + err.Error(), nil
		}
		added, updated := h.newMemberCount(ctx, req.TargetID, f)
		fmt.Fprintf(&sb, "Would add %d member(s) and update %d existing one(s). Rows with neither code nor label are skipped.\n", added, updated)
		return sb.String(), nil
	}

	staged, errRows, err := h.resolveAttachedGrid(ctx, modelID, req.RevisionID, f)
	if err != nil {
		return sb.String() + "Cannot import: " + err.Error(), nil
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
		_ = h.db.QueryRow(ctx, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, id).Scan(&name)
		parts = append(parts, fmt.Sprintf("%s (%d)", name, n))
	}
	sort.Strings(parts)
	fmt.Fprintf(&sb, "No errors. Would import %d value(s): %s.\n", len(staged), strings.Join(parts, ", "))
	return sb.String(), nil
}

func (h *handler) aiImportFile(ctx context.Context, act *actor, sessionID string, req aiassistant.FileImportRequest) (string, error) {
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
			added, _ := h.newMemberCount(ctx, req.TargetID, f)
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
	staged, errRows, err := h.resolveAttachedGrid(ctx, modelID, req.RevisionID, f)
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
	return fmt.Sprintf("Imported %d value(s) from %s into %d metric(s) (mode %s)", len(staged), f.describe(), len(metricIDs), mode), nil
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
func (h *handler) aiReadHooks(r *http.Request, sessionID string) aiassistant.ReadHooks {
	return aiassistant.ReadHooks{
		PreviewFileImport: func(ctx context.Context, req aiassistant.FileImportRequest) (string, error) {
			return h.aiPreviewFileImport(ctx, sessionID, req)
		},
		PreviewExport: func(ctx context.Context, revisionID, gridID, name string, spec dataexport.Spec) (string, error) {
			return h.aiPreviewExport(r.WithContext(ctx), gridID, name, spec)
		},
	}
}
