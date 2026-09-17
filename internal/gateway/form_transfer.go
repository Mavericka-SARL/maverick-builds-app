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
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

var validRecordStatuses = map[string]bool{"draft": true, "submitted": true, "approved": true, "rejected": true}

func (h *handler) formExport(w http.ResponseWriter, r *http.Request, formID string) {
	ctx := r.Context()
	store := crudapp.NewStore(h.db.For(ctx))

	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	form, err := store.GetForm(ctx, formID)
	if err != nil {
		jsonErr(w, err, http.StatusNotFound)
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "xlsx" {
		jsonErr(w, fmt.Errorf("format must be csv or xlsx"), http.StatusBadRequest)
		return
	}

	// No page size on ListRecords other than an explicit limit; a large cap
	// keeps this a genuine full export without opening an unbounded query.
	records, err := store.ListRecords(ctx, formID, 100000)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	records, err = h.filterFormRecords(ctx, act, form, records)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	header := make([]string, 0, len(form.Fields)+3)
	header = append(header, "id", "status", "created_at")
	for _, f := range form.Fields {
		header = append(header, f.Name)
	}

	rows := make([][]string, 0, len(records))
	for _, rec := range records {
		row := make([]string, 0, len(header))
		row = append(row, rec.ID, rec.Status, rec.CreatedAt.UTC().Format(time.RFC3339))
		for _, f := range form.Fields {
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
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20) // 16 MB limit
	ctx := r.Context()
	store := crudapp.NewStore(h.db.For(ctx))

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

	// Match each column header to a form field by name, then by label
	// (case-insensitive) — mirrors how an exported file names its columns.
	// Columns matching neither (e.g. this form's own "id"/"created_at"
	// export columns) are silently ignored rather than rejected, so a
	// straight export-then-reimport of the same file just works.
	fieldByHeader := map[string]crudapp.FormField{}
	for _, col := range header {
		name := strings.TrimSpace(col)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		for _, f := range form.Fields {
			if strings.ToLower(f.Name) == lower || strings.ToLower(f.Label) == lower {
				fieldByHeader[col] = f
				break
			}
		}
	}

	type rowErr struct {
		Row     int    `json:"row"`
		Column  string `json:"column"`
		Message string `json:"message"`
	}
	type stagedRecord struct {
		data   map[string]any
		status string
	}
	var errs []rowErr
	var staged []stagedRecord

	for _, row := range rawRows {
		data := map[string]any{}
		rowValid := true
		for col, field := range fieldByHeader {
			raw := strings.TrimSpace(row.Cells[col])
			if raw == "" {
				if field.Required {
					errs = append(errs, rowErr{Row: row.RowNumber, Column: col, Message: fmt.Sprintf("%q is required", field.Label)})
					rowValid = false
				}
				continue
			}
			switch field.Type {
			case "number":
				v, pErr := strconv.ParseFloat(raw, 64)
				if pErr != nil {
					errs = append(errs, rowErr{Row: row.RowNumber, Column: col, Message: fmt.Sprintf("cannot parse %q as a number", raw)})
					rowValid = false
					continue
				}
				data[field.Name] = v
			case "boolean":
				v, pErr := strconv.ParseBool(raw)
				if pErr != nil {
					errs = append(errs, rowErr{Row: row.RowNumber, Column: col, Message: fmt.Sprintf("cannot parse %q as true/false", raw)})
					rowValid = false
					continue
				}
				data[field.Name] = v
			default:
				data[field.Name] = raw
			}
		}
		if len(data) == 0 {
			continue // blank row
		}
		status := "draft"
		if s := strings.ToLower(strings.TrimSpace(row.Cells["status"])); s != "" {
			if !validRecordStatuses[s] {
				errs = append(errs, rowErr{Row: row.RowNumber, Column: "status", Message: fmt.Sprintf("%q is not a valid status (draft, submitted, approved, rejected)", s)})
				rowValid = false
			} else {
				status = s
			}
		}
		if !rowValid {
			continue
		}
		staged = append(staged, stagedRecord{data: data, status: status})
	}

	if len(errs) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":      fmt.Sprintf("import rejected: %d row(s) failed validation, nothing was imported", len(errs)),
			"error_rows": len(errs),
			"errors":     errs,
		})
		return
	}
	if len(staged) == 0 {
		jsonErr(w, fmt.Errorf("file contained no usable data rows"), http.StatusBadRequest)
		return
	}

	userID := h.resolveUserID(r)
	for _, sr := range staged {
		rec, cErr := store.CreateRecord(ctx, formID, userID, sr.data)
		if cErr != nil {
			jsonErr(w, fmt.Errorf("create record: %w", cErr), http.StatusInternalServerError)
			return
		}
		if sr.status != "draft" {
			if uErr := store.UpdateRecord(ctx, rec.ID, sr.status, sr.data); uErr != nil {
				jsonErr(w, fmt.Errorf("set status: %w", uErr), http.StatusInternalServerError)
				return
			}
		}
		bgCtx := context.WithoutCancel(ctx)
		recID, status, data := rec.ID, sr.status, sr.data
		go func() { _ = h.applyFormMappings(bgCtx, recID, formID, status, data, userID) }()
	}

	if a, e := h.resolveActor(ctx, r); e == nil {
		appID, _ := h.appIDFromFormID(ctx, formID)
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryDataChange, EventType: auditlog.EventFormImported,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: appID, ResourceType: "form", ResourceID: formID,
			Metadata: map[string]string{"records_created": strconv.Itoa(len(staged))},
		})
	}
	jsonOK(w, map[string]any{"status": "ok", "records_created": len(staged)})
}

// filterFormRecords excludes any record touching a dimension member or
// metric the actor is "hidden" from, per identity.user_access_rule — the
// read-side counterpart to applyFormMappings' write-side check (fixed
// earlier this session). A form field of type "dimension"/"metric" embeds
// which dimension/metric it's bound to (crudapp.FormField.DimensionID/
// MetricID, the same embedding duplicateRevision already reads to remap
// field references on revision copy); a record's raw field value is
// resolved against that binding to decide visibility. Coarser than
// per-field redaction — excludes the WHOLE record — matching the existing
// "exclude the hidden row entirely" precedent grid()/gridExport already
// use rather than partially redacting one field within an otherwise
// visible record.
func (h *handler) filterFormRecords(ctx context.Context, act *actor, form *crudapp.FormDef, records []*crudapp.FormRecord) ([]*crudapp.FormRecord, error) {
	dimRules := map[string]string{}
	metricRules := map[string]string{}
	arRows, err := h.db.Query(ctx,
		`SELECT rule_type, ref_id, access FROM identity.user_access_rule WHERE user_id=$1::uuid`, act.UserID)
	if err != nil {
		return nil, err
	}
	for arRows.Next() {
		var ruleType, refID, access string
		if arRows.Scan(&ruleType, &refID, &access) == nil {
			switch ruleType {
			case "dimension_member":
				dimRules[refID] = access
			case "metric":
				metricRules[refID] = access
			}
		}
	}
	arRows.Close()
	if len(dimRules) == 0 && len(metricRules) == 0 {
		return records, nil
	}

	type dimField struct{ name, dimensionID string }
	var dimFields []dimField
	metricFieldMetricID := map[string]string{} // field name -> metric ID
	for _, f := range form.Fields {
		if f.Type == "dimension" && f.DimensionID != "" {
			dimFields = append(dimFields, dimField{f.Name, f.DimensionID})
		}
		if f.Type == "metric" && f.MetricID != "" {
			metricFieldMetricID[f.Name] = f.MetricID
		}
	}
	if len(dimFields) == 0 && len(metricFieldMetricID) == 0 {
		return records, nil // nothing in this form's schema is dimension/metric bound
	}

	// codeHidden[dimensionID][code] — cascades a hidden dimension_member
	// rule down the hierarchy exactly like grid()/hiddenMemberFilter do,
	// over this model's full member universe.
	codeHidden := map[string]map[string]bool{}
	if len(dimRules) > 0 {
		memRows, mErr := h.db.Query(ctx, `
			SELECT m.id::text, m.dimension_id::text, m.code, COALESCE(m.parent_member_id::text,'')
			FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
			WHERE d.model_id = $1::uuid`, form.ModelID)
		if mErr != nil {
			return nil, mErr
		}
		type memberRow struct{ id, dimID, code, parentID string }
		var members []memberRow
		edges := make([]writeguard.MemberEdge, 0, 256)
		for memRows.Next() {
			var mr memberRow
			if memRows.Scan(&mr.id, &mr.dimID, &mr.code, &mr.parentID) == nil {
				members = append(members, mr)
				edges = append(edges, writeguard.MemberEdge{ID: mr.id, ParentID: mr.parentID, DimID: mr.dimID})
			}
		}
		memRows.Close()
		if err := memRows.Err(); err != nil {
			return nil, err
		}
		for id := range writeguard.ExpandHidden(edges, dimRules) {
			dimRules[id] = "hidden"
		}
		for _, mr := range members {
			if dimRules[mr.id] != "hidden" {
				continue
			}
			if codeHidden[mr.dimID] == nil {
				codeHidden[mr.dimID] = map[string]bool{}
			}
			codeHidden[mr.dimID][mr.code] = true
		}
	}

	kept := make([]*crudapp.FormRecord, 0, len(records))
recordLoop:
	for _, rec := range records {
		for _, df := range dimFields {
			code, _ := rec.Data[df.name].(string)
			if code != "" && codeHidden[df.dimensionID][code] {
				continue recordLoop // touches a hidden dimension member
			}
		}
		for fieldName, metricID := range metricFieldMetricID {
			if _, has := rec.Data[fieldName]; has && metricRules[metricID] == "hidden" {
				continue recordLoop // touches a hidden metric
			}
		}
		kept = append(kept, rec)
	}
	return kept, nil
}
