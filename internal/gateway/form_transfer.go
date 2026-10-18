package gateway

// GET /api/forms/{id}/export and POST /api/forms/{id}/import — CSV/XLSX
// transfer of form_record data, the form-side counterpart to grid_export.go
// and /api/import/upload. Columns are form field NAMES (falling back to
// field LABELS on the way in), so an exported file round-trips through
// import unmodified, matching the name-based column convention
// importpkg.ResolveRows already established for grid data.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
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
func (h *handler) formImport(w http.ResponseWriter, r *http.Request, formID string) {
	extendImportDeadlines(w)
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
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.CSV == "" && body.XLSXBase64 == "") {
		jsonErr(w, fmt.Errorf("csv or xlsx_base64 field required"), http.StatusBadRequest)
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
	}
	if err != nil {
		jsonErr(w, fmt.Errorf("parse file: %w", err), http.StatusBadRequest)
		return
	}
	if len(rawRows) == 0 {
		jsonErr(w, fmt.Errorf("file contained no data rows"), http.StatusBadRequest)
		return
	}

	created, errs, err := h.importFormRows(ctx, act, scope, form, header, rawRows)
	if err != nil {
		h.jsonLimitErr(w, err)
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
		ApplicationID: scope.appID, ResourceType: "form", ResourceID: formID,
		Metadata: map[string]string{"records_created": strconv.Itoa(created)},
	})
	jsonOK(w, map[string]any{"status": "ok", "records_created": created})
}

// jsonFormRowErrs answers a refused form import: 422, nothing created.
func jsonFormRowErrs(w http.ResponseWriter, errs []formRowError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":      fmt.Sprintf("import rejected: %d row(s) failed validation, nothing was imported", len(errs)),
		"error_rows": len(errs),
		"errors":     errs,
	})
}
