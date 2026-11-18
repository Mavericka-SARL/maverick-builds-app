package gateway

// Records linked through kept dimensions (migration 144): a table may keep a
// dimension's members — each record one member — and other tables' dimension
// fields name those members, so an account's contacts and deals name it.
// These routes serve what such links need:
//
//	GET /api/records/{id}/related      the records of other tables naming it
//	GET /api/records/{id}/delete-plan  what deleting it would do
//	GET /api/records/{id}/history      its history fields' changes
//
// and DELETE /api/records/{id} of such a record (deleteRecord,
// record_delete.go) follows each naming field's delete rule.

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/recordpost"
	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// keptMemberCode is the SQLSTATE the kept-member guard (migration 144)
// refuses a member write with.
const keptMemberCode = "MV409"

// recordSavedHook is what runs in the transaction saving a record of form
// that held before and now holds after: the member it is, relabelled, and
// its history fields' changes. Nil when the table has neither.
func recordSavedHook(form *crudapp.FormDef, recordID string, before, after map[string]any, userID string, changed *bool) func(ctx context.Context, tx dbx.DB) error {
	if !form.KeepsDimension() && len(form.HistoryFields()) == 0 {
		return nil
	}
	return func(ctx context.Context, tx dbx.DB) error {
		c, err := recordpost.KeepMember(ctx, tx, form, after)
		if err != nil {
			return err
		}
		if c && changed != nil {
			*changed = true
		}
		return crudapp.LogFieldChanges(ctx, tx, form, recordID, before, after, userID)
	}
}

// restrictDeletePlan recounts plan's linked records as act sees them
// (recordpost.DeletePlan.Restrict): a table it does not reach counts none,
// one it reaches counts the records its visibility keeps. A plan with
// records act does not see is refused, and names no count of them.
func (h *handler) restrictDeletePlan(ctx context.Context, act *actor, plan *recordpost.DeletePlan) error {
	store := crudapp.NewStore(h.db.For(ctx))
	return plan.Restrict(func(tableID string, ids []string) (int, error) {
		if len(ids) == 0 {
			return 0, nil
		}
		rs, err := h.resolveFormRecordScope(ctx, act, tableID)
		if err != nil || !rs.reach {
			return 0, err
		}
		vis, err := h.recordVisibility(ctx, rs)
		if err != nil {
			return 0, err
		}
		vals := make([]any, len(ids))
		for i, id := range ids {
			vals[i] = id
		}
		res, err := store.QueryRecords(ctx, rs.form, vis, act.UserID, crudapp.Query{
			Filters: []crudapp.Filter{{Field: "id", Op: "in", Values: vals}},
			Limit:   1,
		})
		if err != nil {
			return 0, err
		}
		return res.Total, nil
	})
}

// recordDeletePlan serves GET /api/records/{id}/delete-plan: whether the
// caller may delete the record and what it would take with it — the
// confirmation shows it before anything is deleted.
func (h *handler) recordDeletePlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	rec, scope, access, err := h.reachRecord(ctx, act, crudapp.NewStore(h.db.For(ctx)), r.PathValue("id"))
	if err != nil {
		jsonRecordErr(w, err, "record")
		return
	}
	out := map[string]any{"can_delete": access.CanDelete(rec.Status), "blocked": []any{}, "cascade": []any{}, "clear": []any{}}
	if !access.CanDelete(rec.Status) {
		out["refusal"] = "you may not delete this record"
		jsonOK(w, out)
		return
	}
	if !scope.form.KeepsDimension() {
		jsonOK(w, out)
		return
	}
	plan, err := h.records().PlanDelete(ctx, scope.form, rec)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if err := h.restrictDeletePlan(ctx, act, plan); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	out["can_delete"] = plan.CanDelete()
	out["blocked"], out["cascade"], out["clear"] = plan.Blocked, plan.Cascade, plan.Clear
	if !plan.CanDelete() {
		out["refusal"] = plan.Refusal(scope.form.MemberLabel(rec.Data))
	}
	jsonOK(w, out)
}

// relatedTable is one table's records naming a record, as the caller sees
// them.
type relatedTable struct {
	Table      recordsQueryTable     `json:"table"`
	Field      string                `json:"field"`
	FieldLabel string                `json:"field_label"`
	Columns    []crudapp.Column      `json:"columns"`
	Records    []*crudapp.FormRecord `json:"records"`
	Total      int                   `json:"total"`
	// CanCreate: the caller may add a record to the table (one naming this
	// record, the console offers).
	CanCreate bool `json:"can_create"`
}

// relatedLimit is how many of a table's related records are served.
const relatedLimit = 50

// recordRelated serves GET /api/records/{id}/related: for a record of a
// table keeping a dimension, each other table's records naming its member,
// as the caller may see them (an account's contacts and deals).
func (h *handler) recordRelated(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	rec, scope, _, err := h.reachRecord(ctx, act, crudapp.NewStore(h.db.For(ctx)), r.PathValue("id"))
	if err != nil {
		jsonRecordErr(w, err, "record")
		return
	}
	out := []relatedTable{}
	code := scope.form.MemberCode(rec.Data)
	if code == "" {
		jsonOK(w, map[string]any{"related": out})
		return
	}
	refs, err := h.records().Referrers(ctx, scope.form)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	store := crudapp.NewStore(h.db.For(ctx))
	for _, ref := range refs {
		rs, err := h.resolveFormRecordScope(ctx, act, ref.Form.ID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if !rs.reach {
			continue
		}
		vis, err := h.recordVisibility(ctx, rs)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		res, err := store.QueryRecords(ctx, rs.form, vis, act.UserID, crudapp.Query{
			Filters: []crudapp.Filter{{Field: ref.Field.Name, Op: "eq", Value: code}},
			Limit:   relatedLimit,
		})
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if err := h.prefetchParents(ctx, &rs, res.Records); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		for _, rr := range res.Records {
			rs.permit(rr)
		}
		out = append(out, relatedTable{
			Table:      recordsQueryTable{ID: rs.form.ID, Name: rs.form.Name, Label: rs.form.Label},
			Field:      ref.Field.Name,
			FieldLabel: firstNonEmpty(ref.Field.Label, ref.Field.Name),
			Columns:    rs.form.Columns(),
			Records:    res.Records,
			Total:      res.Total,
			CanCreate:  rs.createAccess().CanCreate(rs.newRecordStatus()),
		})
	}
	jsonOK(w, map[string]any{"related": out})
}

// recordHistory serves GET /api/records/{id}/history: each value the
// record's history fields took, oldest first. A member hidden from the
// caller is shown as hidden, not by its code.
func (h *handler) recordHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	store := crudapp.NewStore(h.db.For(ctx))
	rec, scope, _, err := h.reachRecord(ctx, act, store, r.PathValue("id"))
	if err != nil {
		jsonRecordErr(w, err, "record")
		return
	}
	entries, err := store.FieldHistory(ctx, scope.form, rec.ID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	view := scope.view
	if view == nil {
		if view, err = h.accessViewFor(ctx, act.UserID, scope.form); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}
	for i := range entries {
		f := scope.form.Field(entries[i].Field)
		if f == nil || f.Type != "dimension" || f.DimensionID == "" {
			continue
		}
		for _, v := range []*any{&entries[i].Old, &entries[i].New} {
			if s, ok := (*v).(string); ok && view.HiddenCode(f.DimensionID, s) {
				*v = map[string]any{"hidden": true}
			}
		}
	}
	jsonOK(w, map[string]any{"history": entries})
}

// tableSettings are the settings of a table beyond its fields a create or
// update may send (recordpost.TableSettings); each omitted keeps what the
// table has.
type tableSettings = recordpost.TableSettings

// applyTableSettings saves s on form (whose fields are already saved) —
// recordpost.ApplyTableSettings — and recalculates what reads the kept
// dimension's members when it added any. It answers a status code and
// error for a setting refused (0 when saved).
func (h *handler) applyTableSettings(ctx context.Context, form *crudapp.FormDef, s tableSettings) (int, error) {
	added, err := h.records().ApplyTableSettings(ctx, form, s)
	var se *recordpost.SettingError
	if errors.As(err, &se) {
		return se.Status, err
	} else if err != nil {
		return http.StatusInternalServerError, err
	}
	if added > 0 {
		go h.recalcDimensionDependents(context.WithoutCancel(ctx), form.KeepsDimensionID) //nolint:contextcheck
	}
	return 0, nil
}

// dropWithheldKeptMembers removes from dims, for each dimension a table
// keeps, the members whose records act does not see.
func (h *handler) dropWithheldKeptMembers(ctx context.Context, act *actor, dims []devDimension) error {
	keeperOf := map[string]string{}
	for _, d := range dims {
		if d.keptBy != "" {
			keeperOf[d.ID] = d.keptBy
		}
	}
	if len(keeperOf) == 0 {
		return nil
	}
	store := crudapp.NewStore(h.db.For(ctx))
	for i, d := range dims {
		formID, ok := keeperOf[d.ID]
		if !ok {
			continue
		}
		scope, err := h.resolveFormRecordScope(ctx, act, formID)
		if err != nil {
			return err
		}
		if scope.admin {
			continue
		}
		vis, err := h.recordVisibility(ctx, scope)
		if err != nil {
			return err
		}
		withheld, err := store.WithheldCodes(ctx, scope.form, vis)
		if err != nil || len(withheld) == 0 {
			if err != nil {
				return err
			}
			continue
		}
		gone := map[string]bool{}
		for _, c := range withheld {
			gone[c] = true
		}
		kept := d.Members[:0]
		for _, m := range d.Members {
			if !gone[m.Code] {
				kept = append(kept, m)
			}
		}
		dims[i].Members = kept
	}
	return nil
}

// boardColumn is one column of a board: a member of the column field's
// dimension, or a choice of its select field.
type boardColumn struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Color is the member's colour property, when the board names one.
	Color string `json:"color,omitempty"`
	// ReadOnly: the member is read-only for the viewer — its cards are
	// theirs to see, and no card moves into it.
	ReadOnly bool `json:"read_only,omitempty"`
}

// recordBoardColumns serves GET /api/forms/{id}/board-columns?field=&color_property=:
// a board's columns for the caller — the column field's choices, or its
// dimension's leaf members in their order, without those hidden from the
// caller (by an access rule, or a kept member whose record they do not see)
// or outside the field's allowed members.
func (h *handler) recordBoardColumns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	scope, err := h.resolveFormRecordScope(ctx, act, r.PathValue("id"))
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !scope.reach {
		jsonRecordErr(w, errRecordNotReached, "table")
		return
	}
	f := scope.form.Field(r.URL.Query().Get("field"))
	if f == nil || (f.Type != "select" && (f.Type != "dimension" || f.DimensionID == "")) {
		jsonErr(w, fmt.Errorf("field names no dimension or select field of %s", scope.form.Label), http.StatusBadRequest)
		return
	}
	cols := []boardColumn{}
	if f.Type == "select" {
		for _, o := range f.Options {
			cols = append(cols, boardColumn{Key: o, Label: o})
		}
		jsonOK(w, map[string]any{"columns": cols})
		return
	}
	rows, err := h.db.Query(ctx, `
		SELECT m.code, m.label, COALESCE(m.properties->>$2, '')
		FROM model.dimension_member m
		WHERE m.dimension_id = $1::uuid
		  AND NOT EXISTS (SELECT 1 FROM model.dimension_member c WHERE c.parent_member_id = m.id)
		  AND COALESCE(btrim(m.formula), '') = ''
		ORDER BY m.time_index NULLS LAST, m.sort_order, m.code`, f.DimensionID, r.URL.Query().Get("color_property"))
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var c boardColumn
		if err := rows.Scan(&c.Key, &c.Label, &c.Color); err != nil {
			rows.Close()
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		cols = append(cols, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	view := scope.view
	if view == nil {
		if view, err = h.accessViewFor(ctx, act.UserID, scope.form); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}
	// What the caller does not see as a member of a record: the members
	// hidden from them, and those a table keeps whose records they do not see.
	vis, err := h.recordVisibility(ctx, scope)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	hidden := map[string]bool{}
	for _, c := range vis.HiddenCodes[f.Name] {
		hidden[c] = true
	}
	allowed := map[string]bool{}
	for _, c := range f.AllowedMembers {
		allowed[c] = true
	}
	kept := cols[:0]
	for _, c := range cols {
		if hidden[c.Key] || view.HiddenCode(f.DimensionID, c.Key) || (len(allowed) > 0 && !allowed[c.Key]) {
			continue
		}
		c.ReadOnly = !scope.admin && view.ReadOnlyCode(f.DimensionID, c.Key)
		kept = append(kept, c)
	}
	jsonOK(w, map[string]any{"columns": kept})
}
