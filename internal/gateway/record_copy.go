package gateway

import (
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

// recordsCopy handles POST /api/forms/{id}/records/copy, body {record_ids,
// field, value}: new records like the ones named, with the date field
// `field` set to `value` — last month's intercompany matches and loans
// brought into this month's close, instead of typed again (2026-11-13). The
// copies are made as the caller's own, as a file import would make them:
// checked row by row, all or none, given new codes, starting as drafts in a
// table with a status, and posted as one batch. A table whose records carry
// a code typed from outside cannot be copied: each copy needs its own code.
func (h *handler) recordsCopy(w http.ResponseWriter, r *http.Request, formID string) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var body struct {
		RecordIDs []string `json:"record_ids"`
		Field     string   `json:"field"`
		Value     string   `json:"value"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	scope, err := h.resolveFormRecordScope(ctx, act, formID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !scope.reach {
		jsonRecordErr(w, errRecordNotReached, "table")
		return
	}
	if scope.noAdd {
		jsonErr(w, fmt.Errorf("forbidden: %s", scope.noAddReason()), http.StatusForbidden)
		return
	}
	form := scope.form
	if form.TypedCodeField() != nil {
		jsonErr(w, fmt.Errorf("the records of %s carry a code given from outside: each copy needs its own code, so they are added, not copied", form.Label), http.StatusBadRequest)
		return
	}
	f := form.Field(strings.TrimSpace(body.Field))
	if f == nil || f.Type != "date" {
		jsonErr(w, fmt.Errorf("field names the date field each copy takes the new value of (%q is no date field of %s)", body.Field, form.Label), http.StatusBadRequest)
		return
	}
	if _, err := time.Parse("2006-01-02", strings.TrimSpace(body.Value)); err != nil {
		jsonErr(w, fmt.Errorf("value is the copies' %s, a date (YYYY-MM-DD), not %q", firstNonEmpty(f.Label, f.Name), body.Value), http.StatusBadRequest)
		return
	}
	if len(body.RecordIDs) == 0 || len(body.RecordIDs) > 1000 {
		jsonErr(w, fmt.Errorf("record_ids names the records to copy, 1 to 1000"), http.StatusBadRequest)
		return
	}
	vis, err := h.recordVisibility(ctx, scope)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	store := crudapp.NewStore(h.db.For(ctx))
	// The columns: every field a person fills (not the server's code, not
	// the status: a copy starts as the caller's new record does).
	var header []string
	for _, fd := range form.Fields {
		if fd.Type == crudapp.FieldAutoCode || fd.Type == crudapp.FieldStatus {
			continue
		}
		header = append(header, fd.Name)
	}
	rows := make([]importpkg.RawRow, 0, len(body.RecordIDs))
	for i, id := range body.RecordIDs {
		rec, err := store.VisibleRecord(ctx, form, vis, id)
		if err != nil || rec == nil || rec.FormID != formID {
			jsonErr(w, fmt.Errorf("record %s is not one of %s you see", id, form.Label), http.StatusNotFound)
			return
		}
		cells := map[string]string{}
		for _, name := range header {
			cells[name] = cellText(rec.Data[name])
		}
		cells[f.Name] = strings.TrimSpace(body.Value)
		rows = append(rows, importpkg.RawRow{RowNumber: i + 1, Cells: cells})
	}
	res, err := h.importFormRows(ctx, act, scope, form, header, rows, false, "", nil)
	if err != nil {
		h.jsonImportErr(w, err)
		return
	}
	if len(res.Errors) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "nothing was copied: a copy was refused", "errors": res.Errors})
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventFormRecordCreated,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: scope.appID, ResourceType: "form", ResourceID: formID,
		Metadata: map[string]string{"copied": strconv.Itoa(res.Created), "field": f.Name, "value": body.Value},
	})
	jsonOK(w, map[string]any{"status": "ok", "copied": res.Created})
}

// cellText is a record value as an import cell holds it.
func cellText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return x.String()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
