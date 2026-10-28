package gateway

// The file-import side of POST /api/integrations/{id}/run, shared by every
// saved file integration — Excel/CSV ("csv_import") and Google Sheets — and,
// for the commit itself, by the AI Developer's attachment import:
//
//   - a run's file arrives as CSV text or as a workbook (xlsx_base64 + sheet),
//     so a business user's dashboard button can send the .xlsx they have;
//   - the integration's saved reshape (importpkg.Reshape) turns a sheet laid
//     out for people — a title above the header, months across, total rows —
//     into importable rows first;
//   - the integration's saved column_map is applied in the Import Wizard's
//     vocabulary (importpkg.ApplyColumnMap), so a file holding metric and
//     member NAMES runs — not only the metric-UUID CSV the wizard builds;
//   - a grid run resolves names, checks members are leaves, commits through
//     the write guard and the plan's limits, recalculates, and records the run.
//
// Before this, csv_import grid runs staged the raw metric_id cell as the
// metric, checked no member, ignored the saved import_mode, and took CSV
// only; Google Sheets syncs skipped the plan limits and kept no run history.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	importpkgv1 "github.com/mavericks-engine/mavericks/gen/go/importpkg/v1"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// maxRunBodyBytes bounds a run's request: a 16 MB workbook, base64-encoded.
const maxRunBodyBytes = 24 << 20

// parseRunFile reads the file a csv_import run carries: "csv" text, or
// "xlsx_base64" with an optional "sheet" — by default the sheet the
// integration saved (savedSheet), else the first — reshaped as the
// integration saved (nil = as is). A dashboard's Integration button sends
// no sheet, so a loader saved from a workbook's second sheet read its first
// (2026-10-28). Workbook cells are read as stored,
// so a formatted "1,234.50" arrives as 1234.5. fileName is the person's
// file as the request names it ("file_name"), for Recent Imports.
func parseRunFile(w http.ResponseWriter, r *http.Request, reshape *importpkg.Reshape, savedSheet string) (header []string, rows []importpkg.RawRow, fileName string, err error) {
	var body struct {
		CSV        string `json:"csv"`
		XLSXBase64 string `json:"xlsx_base64"`
		Sheet      string `json:"sheet"`
		// RowLines is the line in the person's file of each data row of
		// csv, when csv was written out of it (the Import Wizard's):
		// every answer about a row names that line (importpkg.RenumberRows).
		RowLines []int  `json:"row_lines"`
		FileName string `json:"file_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRunBodyBytes)).Decode(&body); err != nil {
		return nil, nil, "", fmt.Errorf("a run needs the file: \"csv\" text or \"xlsx_base64\" (%v)", err)
	}
	switch {
	case body.XLSXBase64 != "":
		raw, err := base64.StdEncoding.DecodeString(body.XLSXBase64)
		if err != nil {
			return nil, nil, "", fmt.Errorf("xlsx_base64: %w", err)
		}
		sheet := body.Sheet
		if sheet == "" {
			sheet = savedSheet
		}
		header, rows, err = importpkg.ReadShaped("run.xlsx", raw, sheet, reshape)
		return header, rows, body.FileName, err
	case body.CSV != "":
		header, rows, err = importpkg.ShapeCSV([]byte(body.CSV), reshape)
		importpkg.RenumberRows(rows, body.RowLines)
		return header, rows, body.FileName, err
	}
	return nil, nil, "", fmt.Errorf("csv or xlsx_base64 field required")
}

// rowsCSV re-serializes mapped rows for the form and dimension importers,
// which read CSV keyed by lower-case logical column names.
func rowsCSV(header []string, rows []importpkg.RawRow) (*csv.Reader, map[string]int, error) {
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write(header)
	for _, r := range rows {
		rec := make([]string, len(header))
		for i, col := range header {
			rec[i] = r.Cells[col]
		}
		_ = cw.Write(rec)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, nil, err
	}
	cr := csv.NewReader(&buf)
	if _, err := cr.Read(); err != nil {
		return nil, nil, fmt.Errorf("file header: %w", err)
	}
	colIdx, err := fileColumnIndex(header)
	if err != nil {
		return nil, nil, err
	}
	return cr, colIdx, nil
}

// fileColumnIndex maps each lower-case column name of a file's header to its
// position, refusing a name the header holds twice: the index kept the last
// one, so a file with two code columns imported each member's parent as its
// code — members relabelled, the new ones never made — and said it had
// imported every row.
func fileColumnIndex(header []string) (map[string]int, error) {
	colIdx := make(map[string]int, len(header))
	for i, col := range header {
		key := strings.ToLower(strings.TrimSpace(col))
		if key == "" {
			continue
		}
		if prev, dup := colIdx[key]; dup {
			return nil, fmt.Errorf("columns %d and %d are both named %q — a file names each column once", prev+1, i+1, strings.TrimSpace(col))
		}
		colIdx[key] = i
	}
	return colIdx, nil
}

// commitGridRows writes resolved rows into a revision the way every grid
// import does: the plan's fact and storage limits, one import job (described
// by details, when it has any), the write guard (CommitImport refuses
// restricted members, metrics and locked scopes with
// importpkg.ErrWriteDenied), and a recalculation afterwards.
func (h *handler) commitGridRows(ctx context.Context, act *actor, modelID, revisionID string, staged []importpkg.StagingRow, mode importpkg.ImportMode, source string, details importpkg.JobDetails) ([]string, error) {
	if cid := h.customerOfModel(ctx, modelID); cid != "" && h.plans != nil {
		if err := h.plans.CheckFactRows(ctx, h.db.For(ctx), cid, modelID, len(staged)); err != nil {
			return nil, err
		}
		if err := h.plans.CheckStorage(ctx, h.db.For(ctx), cid); err != nil {
			return nil, err
		}
	}
	store := importpkg.NewStore(h.db.For(ctx))
	job, err := store.CreateImportJob(ctx, modelID, revisionID, source, act.UserID, nil)
	if err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}
	if details != (importpkg.JobDetails{}) {
		h.describeImportJob(ctx, store, job.Id, details)
	}
	if err := store.StageRows(ctx, job.Id, staged, nil); err != nil {
		return nil, fmt.Errorf("stage: %w", err)
	}
	metricIDs, err := store.CommitImport(ctx, job.Id, modelID, revisionID, act.UserID, mode)
	if err != nil {
		if errors.Is(err, importpkg.ErrWriteDenied) {
			return nil, err
		}
		return nil, fmt.Errorf("commit: %w", err)
	}
	if len(metricIDs) > 0 {
		h.recalcAfterImport(ctx, modelID, revisionID, source+" import", metricIDs)
	}
	return metricIDs, nil
}

// runGridIntegration runs a file integration into its grid's revision: the
// file whole, or — when a row has a problem — none of it, every such row
// detailed (row, column, message). The run is recorded in the integration's
// history either way, and a refused one in Recent Imports as the Import
// Wizard's is; a dry run (the Import Wizard's Validate step) only answers
// what the run would do.
func (h *handler) runGridIntegration(w http.ResponseWriter, ctx context.Context, act *actor, intID, modelID, revisionID string, header []string, rows []importpkg.RawRow, mode importpkg.ImportMode, source string, details importpkg.JobDetails, dryRun bool) {
	refused := func(errorRows int, errs []*importpkgv1.ImportError) {
		h.recordImportJob(ctx, importpkg.FinishedJob{ModelID: modelID, RevisionID: revisionID, Source: source, UserID: act.UserID,
			Details: details, ErrorRows: errorRows, Failed: true, Errors: errs})
	}
	fail := func(status int, err error) {
		if !dryRun {
			h.recordIntegrationRun(ctx, intID, act.UserID, 0, 0, "error", err.Error())
			refused(0, []*importpkgv1.ImportError{{Message: err.Error()}})
		}
		var le *plan.LimitError
		if errors.As(err, &le) {
			h.jsonLimitErr(w, err)
			return
		}
		jsonErr(w, err, status)
	}
	systemManaged, err := writeguard.SystemManaged(ctx, h.db.For(ctx), revisionID)
	if err != nil {
		fail(http.StatusInternalServerError, fmt.Errorf("check system-managed: %w", err))
		return
	}
	if systemManaged {
		fail(http.StatusForbidden, fmt.Errorf("this revision is system-managed and read-only"))
		return
	}
	var gridID string
	_ = h.db.QueryRow(ctx, `SELECT COALESCE(target_id::text,'') FROM model.integration_def WHERE id=$1::uuid AND target_type='grid'`, intID).Scan(&gridID)
	if gridID != "" {
		if err := importpkg.CheckGridColumns(ctx, h.db.For(ctx), gridID, header); err != nil {
			fail(http.StatusBadRequest, err)
			return
		}
	}
	staged, importErrs, err := importpkg.ResolveRows(ctx, h.db.For(ctx), modelID, revisionID, header, rows)
	if err != nil {
		fail(http.StatusBadRequest, fmt.Errorf("%w — map the column to a metric or dimension name, \"metric\"/\"value\", or \"ignore\" in the integration's column map", err))
		return
	}
	okRows, failedRows := fileRows(staged, importErrs)
	if dryRun {
		jsonOK(w, map[string]any{
			"dry_run": true, "rows": okRows, "values": len(staged),
			"error_rows": failedRows, "errors": importErrorRows(importErrs),
		})
		return
	}
	// A file imports whole or not at all, as the Import Wizard's and the
	// table's do. A run used to import the rows that passed and name only
	// the first that failed.
	if len(importErrs) > 0 {
		h.recordIntegrationRun(ctx, intID, act.UserID, 0, failedRows, "error",
			fmt.Sprintf("%d row(s) failed validation, nothing was imported — %s", failedRows, firstRowProblem(importErrs)))
		refused(failedRows, importErrs)
		jsonGridRowErrs(w, staged, importErrs)
		return
	}
	if len(staged) == 0 {
		h.recordIntegrationRun(ctx, intID, act.UserID, 0, 0, "error", "no valid rows")
		jsonOK(w, map[string]any{"rows_imported": 0, "values_imported": 0, "error_rows": 0, "errors": []any{}})
		return
	}
	if _, err := h.commitGridRows(ctx, act, modelID, revisionID, staged, mode, source, details); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, importpkg.ErrWriteDenied) {
			status = http.StatusForbidden
		}
		fail(status, err)
		return
	}
	h.recordIntegrationRun(ctx, intID, act.UserID, okRows, 0, "success", "")
	jsonOK(w, map[string]any{
		"rows_imported": okRows, "values_imported": len(staged),
		"error_rows": 0, "errors": []any{},
	})
}
