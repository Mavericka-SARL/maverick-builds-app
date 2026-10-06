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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
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
	// The records of a form the caller reaches only (resolveFormRecordScope).
	if scope, err := h.resolveFormRecordScope(ctx, act, formID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	} else if !scope.reach {
		jsonRecordErr(w, errRecordNotReached, "form")
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
		jsonRecordErr(w, errRecordNotReached, "form")
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
		jsonErr(w, err, http.StatusInternalServerError)
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

// formRowError is one row a form import refuses.
type formRowError struct {
	Row     int    `json:"row"`
	Column  string `json:"column"`
	Message string `json:"message"`
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

// importFormRows creates a record of form for each row and posts it through
// the form's mappings — the one path for POST /api/forms/{id}/import and a
// saved csv_import or google_sheets integration whose target is a form.
// Validation is whole-file atomic: when errs is not empty nothing was
// created. A row's status column, when there is one, must be a status the
// caller may create (scope); otherwise a record starts as a draft.
func (h *handler) importFormRows(ctx context.Context, act *actor, scope formRecordScope, form *crudapp.FormDef, header []string, rawRows []importpkg.RawRow) (created int, errs []formRowError, err error) {
	store := crudapp.NewStore(h.db.For(ctx))

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

	type stagedRecord struct {
		data   map[string]any
		status string
	}
	var staged []stagedRecord
	memberLeaves := map[string]map[string]bool{} // dimension id → code → leaf

	for _, row := range rawRows {
		data := map[string]any{}
		rowValid := true
		for col, field := range fieldByHeader {
			raw := strings.TrimSpace(row.Cells[col])
			if raw == "" {
				if field.Required {
					errs = append(errs, formRowError{Row: row.RowNumber, Column: col, Message: fmt.Sprintf("%q is required", field.Label)})
					rowValid = false
				}
				continue
			}
			if field.Type == "dimension" && field.DimensionID != "" {
				if msg, mErr := h.formMemberProblem(ctx, field, raw, memberLeaves); mErr != nil {
					return 0, nil, mErr
				} else if msg != "" {
					errs = append(errs, formRowError{Row: row.RowNumber, Column: col, Message: msg})
					rowValid = false
					continue
				}
			}
			switch {
			// A metric field bound to a metric holds its amount, entered as a
			// number on screen; stored as text it was never posted.
			case field.Type == "number", field.Type == "metric" && field.MetricID != "":
				v, pErr := strconv.ParseFloat(raw, 64)
				if pErr != nil {
					errs = append(errs, formRowError{Row: row.RowNumber, Column: col, Message: fmt.Sprintf("cannot parse %q as a number", raw)})
					rowValid = false
					continue
				}
				data[field.Name] = v
			case field.Type == "boolean":
				v, pErr := strconv.ParseBool(raw)
				if pErr != nil {
					errs = append(errs, formRowError{Row: row.RowNumber, Column: col, Message: fmt.Sprintf("cannot parse %q as true/false", raw)})
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
			if !crudapp.ValidRecordStatus(s) {
				errs = append(errs, formRowError{Row: row.RowNumber, Column: "status", Message: fmt.Sprintf("%q is not a valid status (draft, submitted, approved, rejected)", s)})
				rowValid = false
			} else if !scope.createAccess().CanCreate(s) {
				errs = append(errs, formRowError{Row: row.RowNumber, Column: "status", Message: fmt.Sprintf("only an administrator of this application imports a record as %s", s)})
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
		return 0, errs, nil
	}

	userID := act.UserID
	for _, sr := range staged {
		rec, cErr := store.CreateRecordWithStatus(ctx, form.ID, userID, sr.status, sr.data)
		if cErr != nil {
			return created, nil, fmt.Errorf("create record: %w", cErr)
		}
		created++
		bgCtx := context.WithoutCancel(ctx)
		recID, status, data := rec.ID, sr.status, sr.data
		go func() { _ = h.applyFormMappings(bgCtx, recID, form.ID, status, data, userID) }()
		// The rules a record created by hand fires: an automation that
		// starts a workflow per submitted expense did not start for
		// imported ones.
		h.dispatchRecordCreated(ctx, scope.appID, scope.revisionID, form.ID, recID, status, userID)
	}
	return created, nil, nil
}

// formMemberProblem says why raw is no member a dimension field of a record
// can hold — a code its dimension does not have, a parent member, one the
// field's allowed members leave out — or "" when it is one. The form's own
// member picker offers leaves only; a record naming another code was
// created but its posting went where no grid reads. leaves caches each
// dimension's codes (true for a leaf) across the file.
func (h *handler) formMemberProblem(ctx context.Context, field crudapp.FormField, raw string, leaves map[string]map[string]bool) (string, error) {
	codes, ok := leaves[field.DimensionID]
	if !ok {
		codes = map[string]bool{}
		rows, err := h.db.Query(ctx, `
			SELECT m.code, NOT EXISTS (SELECT 1 FROM model.dimension_member c
			                           WHERE c.parent_member_id = m.id AND c.dimension_id = m.dimension_id)
			FROM model.dimension_member m
			WHERE m.dimension_id = $1::uuid AND NULLIF(btrim(m.formula),'') IS NULL`, field.DimensionID)
		if err != nil {
			return "", fmt.Errorf("read members of %s: %w", field.Label, err)
		}
		for rows.Next() {
			var code string
			var leaf bool
			if err := rows.Scan(&code, &leaf); err != nil {
				rows.Close()
				return "", err
			}
			codes[code] = leaf
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", err
		}
		leaves[field.DimensionID] = codes
	}
	leaf, known := codes[raw]
	switch {
	case !known:
		return fmt.Sprintf("%q is not a member of %s (member codes match exactly)", raw, field.Label), nil
	case !leaf:
		return fmt.Sprintf("%q is not a leaf member of %s (has child members)", raw, field.Label), nil
	case len(field.AllowedMembers) > 0 && !slices.Contains(field.AllowedMembers, raw):
		return fmt.Sprintf("%q is not one of the members %s allows", raw, field.Label), nil
	}
	return "", nil
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
	// The form's field bindings point at its own revision's dimensions and
	// metrics, so the rules resolve by lineage against that revision (a
	// form of an older revision hides what the active one does). Fails
	// closed.
	var formRevision string
	if err := h.db.QueryRow(ctx,
		`SELECT COALESCE(revision_id::text, '') FROM model.form_def WHERE id=$1::uuid`, form.ID,
	).Scan(&formRevision); err != nil {
		return nil, fmt.Errorf("resolve form revision: %w", err)
	}
	dimRules, metricRules, err := loadUserAccessRules(ctx, h.db, act.UserID, formRevision)
	if err != nil {
		return nil, err
	}
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
			if sErr := memRows.Scan(&mr.id, &mr.dimID, &mr.code, &mr.parentID); sErr != nil {
				memRows.Close()
				return nil, sErr
			}
			members = append(members, mr)
			edges = append(edges, writeguard.MemberEdge{ID: mr.id, ParentID: mr.parentID, DimID: mr.dimID})
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
