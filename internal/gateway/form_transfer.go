package gateway

// GET /api/forms/{id}/export and POST /api/forms/{id}/import — CSV/XLSX
// transfer of form_record data, the form-side counterpart to grid_export.go
// and /api/import/upload. Columns are form field NAMES (falling back to
// field LABELS on the way in), so an exported file round-trips through
// import unmodified, matching the name-based column convention
// importpkg.ResolveRows already established for grid data.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/recordpost"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

func (h *handler) formExport(w http.ResponseWriter, r *http.Request, formID string) {
	ctx := r.Context()
	store := crudapp.NewStore(h.db.For(ctx))

	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	// The records of a table the caller reaches only (resolveFormRecordScope),
	// those they may see (recordVisibility), and — given the widget's
	// query — those it shows, in its order.
	scope, err := h.resolveFormRecordScope(ctx, act, formID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !scope.reach {
		jsonRecordErr(w, errRecordNotReached, "table")
		return
	}
	form := scope.form
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "xlsx" {
		jsonErr(w, fmt.Errorf("format must be csv or xlsx"), http.StatusBadRequest)
		return
	}
	q, err := recordQueryParam(r)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	vis, err := h.recordVisibility(ctx, scope)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	records, err := store.ExportRecords(ctx, form, vis, act.UserID, q)
	if crudapp.IsQueryError(err) {
		jsonErr(w, err, http.StatusBadRequest)
		return
	} else if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	// A status field's column holds the record's status.
	header := make([]string, 0, len(form.Fields)+2)
	header = append(header, "id", "created_at")
	for _, f := range form.Fields {
		header = append(header, f.Name)
	}

	rows := make([][]string, 0, len(records))
	for _, rec := range records {
		row := make([]string, 0, len(header))
		row = append(row, rec.ID, rec.CreatedAt.UTC().Format(time.RFC3339))
		for _, f := range form.Fields {
			if f.Type == crudapp.FieldStatus {
				row = append(row, rec.Status)
				continue
			}
			row = append(row, formCellString(rec.Data[f.Name]))
		}
		rows = append(rows, row)
	}

	if err := writeTabularResponse(w, format, slugify(form.Name), header, rows); err != nil {
		h.log.Warn().Err(err).Msg("form export: write response failed")
	}
}

func formCellString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// formImport serves POST /api/forms/{id}/import — bulk-creates form_record
// rows from an uploaded CSV or native XLSX, using the exact same
// {csv | xlsx_base64} JSON envelope as /api/import/upload. Like that
// endpoint, validation is whole-file atomic: any row failing validation
// rejects the entire file before anything is created.
//
// Sent again under the same Idempotency-Key, a file is answered from the
// first and creates nothing: the key is kept on the import's Recent Imports
// line, written in the transaction that creates the records. Without one, a
// client sending a file again after a 502 created every record twice
// (2026-10-15).
func (h *handler) formImport(w http.ResponseWriter, r *http.Request, formID string) {
	extendImportDeadlines(w)
	started := time.Now()
	key, ok := importIdempotencyKey(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20) // 16 MB limit
	ctx := r.Context()
	store := crudapp.NewStore(h.db.For(ctx))

	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	// Records go into a form the caller reaches only
	// (resolveFormRecordScope), in the statuses they may create
	// (crudapp.RecordAccess.CanCreate), checked below per row.
	scope, err := h.resolveFormRecordScope(ctx, act, formID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !scope.reach {
		jsonRecordErr(w, errRecordNotReached, "table")
		return
	}

	form, err := store.GetForm(ctx, formID)
	if err != nil {
		jsonErr(w, err, http.StatusNotFound)
		return
	}

	var body struct {
		CSV        string `json:"csv"`
		XLSXBase64 string `json:"xlsx_base64"`
		// DryRun checks every row as the import would and creates nothing:
		// the Import Wizard's Validate step.
		DryRun bool `json:"dry_run"`
		// RowLines is the line in the person's file of each data row of
		// csv, when csv was written out of it (the Import Wizard's):
		// every answer about a row names that line (importpkg.RenumberRows).
		RowLines []int `json:"row_lines"`
		// FileName names the person's file in Recent Imports.
		FileName string `json:"file_name"`
		// ReplaceBy names a field: the table's records holding a value of
		// it the file holds are replaced (recordpost.ImportRequest).
		ReplaceBy string `json:"replace_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.CSV == "" && body.XLSXBase64 == "") {
		jsonErr(w, fmt.Errorf("the file is empty (send its rows as csv, or the workbook as xlsx_base64)"), http.StatusBadRequest)
		return
	}
	jobs := importpkg.NewStore(h.db.For(ctx))
	if key != "" && !body.DryRun && !h.replayKeyedImport(w, ctx, jobs, form.ModelID, act.UserID, key, "table", tableImportReplay) {
		return
	}

	var header []string
	var rawRows []importpkg.RawRow
	if body.XLSXBase64 != "" {
		raw, decErr := base64.StdEncoding.DecodeString(body.XLSXBase64)
		if decErr != nil {
			jsonErr(w, fmt.Errorf("xlsx_base64: %w", decErr), http.StatusBadRequest)
			return
		}
		header, rawRows, err = importpkg.ParseXLSXRows(raw)
	} else {
		header, rawRows, err = importpkg.ParseCSVRows([]byte(body.CSV))
		importpkg.RenumberRows(rawRows, body.RowLines)
	}
	if err != nil {
		jsonErr(w, fmt.Errorf("parse file: %w", err), http.StatusBadRequest)
		return
	}
	if len(rawRows) == 0 {
		jsonErr(w, fmt.Errorf("file contained no data rows"), http.StatusBadRequest)
		return
	}

	tableJob := func(created int, errs []formRowError, fatal error) importpkg.FinishedJob {
		job := h.tableImportJob(ctx, formID, "upload", act.UserID, body.FileName, started, created, errs, fatal)
		if body.ReplaceBy != "" {
			job.Details.Mode = string(importpkg.ModeReplace)
		}
		return job
	}
	// The import's Recent Imports line, with its key, commits with the
	// records it created — or, the key taken by another request sent at the
	// same time, nothing of the file does (importpkg.ErrImportKeyInUse).
	var jobID, revisionID string
	var recordJob func(context.Context, pgx.Tx, recordpost.ImportResult) error
	if !body.DryRun {
		recordJob = func(ctx context.Context, tx pgx.Tx, res recordpost.ImportResult) error {
			job := tableJob(res.Created, nil, nil)
			if job.ModelID == "" || res.Created == 0 {
				return nil
			}
			id, err := importpkg.RecordJobTx(ctx, tx, job, key)
			jobID, revisionID = id, job.RevisionID
			return err
		}
	}
	res, err := h.importFormRows(ctx, act, scope, form, header, rawRows, body.DryRun, body.ReplaceBy, recordJob)
	created, errs := res.Created, res.Errors
	if body.DryRun && err == nil {
		jsonOK(w, map[string]any{"dry_run": true, "rows": created, "rows_replaced": res.Replaced, "error_rows": len(errs), "errors": errs})
		return
	}
	if errors.Is(err, importpkg.ErrImportKeyInUse) {
		// Sent twice at once: the other send created the records.
		if h.replayKeyedImport(w, ctx, jobs, form.ModelID, act.UserID, key, "table", tableImportReplay) {
			jsonErr(w, err, http.StatusConflict)
		}
		return
	}
	if !body.DryRun && (err != nil || len(errs) > 0) {
		// A refused file's line holds no key: its correction goes under it.
		h.recordImportJob(ctx, tableJob(created, errs, err))
	}
	if err != nil {
		h.jsonImportErr(w, err)
		return
	}
	if len(errs) > 0 {
		jsonFormRowErrs(w, errs)
		return
	}
	if created == 0 {
		jsonErr(w, fmt.Errorf("file contained no usable data rows"), http.StatusBadRequest)
		return
	}

	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventFormImported,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: scope.appID, ResourceType: "form", ResourceID: formID, RevisionID: revisionID,
		Metadata: map[string]string{"records_created": strconv.Itoa(created), "records_replaced": strconv.Itoa(res.Replaced)},
	})
	out := map[string]any{"status": "ok", "records_created": created, "records_replaced": res.Replaced}
	if jobID != "" {
		out["job_id"] = jobID
	}
	jsonOK(w, out)
}

// jsonFormRowErrs answers a refused form import: 422, nothing created. When
// every row is refused for one reason — a period locked while its approval
// waits — the answer says it once (2026-11-07: a reload repeated the lock
// on each of 15 rows under "15 row(s) failed validation").
func jsonFormRowErrs(w http.ResponseWriter, errs []formRowError) {
	msg := fmt.Sprintf("import rejected: %d row(s) failed validation, nothing was imported", len(errs))
	if len(errs) > 1 {
		same := true
		for _, e := range errs[1:] {
			same = same && e.Message == errs[0].Message && e.Column == errs[0].Column
		}
		if same {
			msg = fmt.Sprintf("nothing was imported: %s (all %d rows)", errs[0].Message, len(errs))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":      msg,
		"error_rows": len(errs),
		"errors":     errs,
	})
}
