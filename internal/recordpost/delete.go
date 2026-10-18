package recordpost

// Deleting a record of a table that keeps a dimension: the records of other
// tables naming its member go by their dimension field's delete rule —
// "block" (it cannot be deleted while one names it), "cascade" (they are
// deleted with it, and so on down) or "clear" (the field is emptied). The
// plan is worked out first, so the person confirming sees what goes; then
// it is carried out in one transaction, or not at all.

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// DeleteRef is how many records of a table a delete touches through one of
// its fields.
type DeleteRef struct {
	TableID    string `json:"table_id"`
	TableLabel string `json:"table_label"`
	Field      string `json:"field"`
	FieldLabel string `json:"field_label"`
	Count      int    `json:"count"`
}

// DeletePlan is what deleting a record does.
type DeletePlan struct {
	// Blocked lists what stops the delete: records naming the member under
	// a "block" rule, and Reasons the model's own uses of it (a formula).
	Blocked []DeleteRef `json:"blocked"`
	Reasons []string    `json:"reasons,omitempty"`
	// Cascade lists the records deleted with it, Clear the records whose
	// field is emptied.
	Cascade []DeleteRef `json:"cascade"`
	Clear   []DeleteRef `json:"clear"`

	deletes []plannedDelete
	clears  []plannedClear
}

// CanDelete reports whether nothing blocks the plan.
func (p *DeletePlan) CanDelete() bool { return len(p.Blocked) == 0 && len(p.Reasons) == 0 }

// Refusal is the sentence a blocked plan answers with ("" when it is not).
func (p *DeletePlan) Refusal(label string) string {
	if p.CanDelete() {
		return ""
	}
	msg := fmt.Sprintf("%s cannot be deleted", label)
	if len(p.Blocked) > 0 {
		parts := make([]string, len(p.Blocked))
		for i, b := range p.Blocked {
			parts[i] = fmt.Sprintf("%d %s of %s (by its %s)", b.Count, plural(b.Count, "record"), b.TableLabel, b.FieldLabel)
		}
		msg += ": " + joinAnd(parts) + " name it — remove or reassign them first"
	}
	for _, r := range p.Reasons {
		msg += "; " + r
	}
	return msg
}

// DroppedMembers are the dimensions and codes of the members the plan
// deletes with their records.
func (p *DeletePlan) DroppedMembers() []struct{ DimID, Code string } {
	var out []struct{ DimID, Code string }
	for _, d := range p.deletes {
		if d.code != "" {
			out = append(out, struct{ DimID, Code string }{d.form.KeepsDimensionID, d.code})
		}
	}
	return out
}

// DeletedRecords are the ids of every record the plan deletes, the
// record itself first.
func (p *DeletePlan) DeletedRecords() []string {
	out := make([]string, len(p.deletes))
	for i, d := range p.deletes {
		out[i] = d.recordID
	}
	return out
}

// ClearedRecords are the records whose field the plan empties, by table.
func (p *DeletePlan) ClearedRecords() []ClearedRecord {
	var out []ClearedRecord
	for _, c := range p.clears {
		for _, id := range c.recordIDs {
			out = append(out, ClearedRecord{Form: c.form, RecordID: id, Field: c.field})
		}
	}
	return out
}

// ClearedRecord is one record a delete empties a field of.
type ClearedRecord struct {
	Form     *crudapp.FormDef
	RecordID string
	Field    string
}

type plannedDelete struct {
	form     *crudapp.FormDef
	recordID string
	code     string // the member it is, in a table keeping a dimension
}

type plannedClear struct {
	form      *crudapp.FormDef
	field     string
	recordIDs []string
}

// Referrer is a dimension field of a table naming a kept dimension.
type Referrer struct {
	Form  *crudapp.FormDef
	Field crudapp.FormField
}

// PlanDelete works out what deleting rec, a record of form, does.
func (ps *Poster) PlanDelete(ctx context.Context, form *crudapp.FormDef, rec *crudapp.FormRecord) (*DeletePlan, error) {
	plan := &DeletePlan{Blocked: []DeleteRef{}, Cascade: []DeleteRef{}, Clear: []DeleteRef{}}
	type item struct {
		form *crudapp.FormDef
		id   string
		data map[string]any
	}
	queue := []item{{form, rec.ID, rec.Data}}
	deleting := map[string]bool{rec.ID: true}
	type hit struct {
		ref  Referrer
		ids  []string
		rule string
	}
	var hits []hit
	referrers := map[string][]Referrer{} // kept dimension id → its referring fields
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		code := it.form.MemberCode(it.data)
		plan.deletes = append(plan.deletes, plannedDelete{form: it.form, recordID: it.id, code: code})
		if code == "" {
			continue
		}
		dimID := it.form.KeepsDimensionID
		if err := ps.memberInModelUse(ctx, dimID, code, plan); err != nil {
			return nil, err
		}
		refs, ok := referrers[dimID]
		if !ok {
			var err error
			if refs, err = ps.Referrers(ctx, it.form); err != nil {
				return nil, err
			}
			referrers[dimID] = refs
		}
		for _, ref := range refs {
			ids, datas, err := recordsNaming(ctx, ps, ref, code)
			if err != nil {
				return nil, err
			}
			if len(ids) == 0 {
				continue
			}
			rule := ref.Field.DeleteRule()
			hits = append(hits, hit{ref: ref, ids: ids, rule: rule})
			if rule != crudapp.OnDeleteCascade {
				continue
			}
			for i, id := range ids {
				if !deleting[id] {
					deleting[id] = true
					queue = append(queue, item{ref.Form, id, datas[i]})
				}
			}
		}
	}
	// A record being deleted anyway neither blocks nor is cleared.
	add := func(list []DeleteRef, ref Referrer, n int) []DeleteRef {
		for i := range list {
			if list[i].TableID == ref.Form.ID && list[i].Field == ref.Field.Name {
				list[i].Count += n
				return list
			}
		}
		return append(list, DeleteRef{TableID: ref.Form.ID, TableLabel: firstNonEmpty(ref.Form.Label, ref.Form.Name),
			Field: ref.Field.Name, FieldLabel: firstNonEmpty(ref.Field.Label, ref.Field.Name), Count: n})
	}
	for _, h := range hits {
		var left []string
		for _, id := range h.ids {
			if h.rule == crudapp.OnDeleteCascade || !deleting[id] {
				left = append(left, id)
			}
		}
		if len(left) == 0 {
			continue
		}
		switch h.rule {
		case crudapp.OnDeleteCascade:
			plan.Cascade = add(plan.Cascade, h.ref, len(left))
		case crudapp.OnDeleteClear:
			plan.Clear = add(plan.Clear, h.ref, len(left))
			plan.clears = append(plan.clears, plannedClear{form: h.ref.Form, field: h.ref.Field.Name, recordIDs: left})
		default:
			plan.Blocked = add(plan.Blocked, h.ref, len(left))
		}
	}
	return plan, nil
}

// memberInModelUse adds to plan's reasons a use of the member that is not
// a record: a formula or pick-list naming it.
func (ps *Poster) memberInModelUse(ctx context.Context, dimID, code string, plan *DeletePlan) error {
	var memberID string
	err := ps.db(ctx).QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, dimID, code).Scan(&memberID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := metricformula.CheckMemberNotInUse(ctx, ps.db(ctx), dimID, memberID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		if metricformula.IsValidationError(err) {
			plan.Reasons = append(plan.Reasons, err.Error())
			return nil
		}
		return err
	}
	return nil
}

// referrersOf lists the dimension fields, of any table of form's model,
// naming the dimension form keeps.
func (ps *Poster) Referrers(ctx context.Context, form *crudapp.FormDef) ([]Referrer, error) {
	forms, err := crudapp.NewStore(ps.db(ctx)).ListForms(ctx, form.ModelID, "")
	if err != nil {
		return nil, err
	}
	var out []Referrer
	for _, f := range forms {
		for _, fd := range f.Fields {
			if fd.Type == "dimension" && fd.DimensionID == form.KeepsDimensionID {
				out = append(out, Referrer{Form: f, Field: fd})
			}
		}
	}
	return out, nil
}

// recordsNaming lists the records of ref's table whose field names code.
func recordsNaming(ctx context.Context, ps *Poster, ref Referrer, code string) ([]string, []map[string]any, error) {
	rows, err := ps.db(ctx).Query(ctx, `
		SELECT id::text, data FROM runtime.form_record
		WHERE form_id = $1::uuid AND data->>$2::text = $3
		ORDER BY created_at, id`, ref.Form.ID, ref.Field.Name, code)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids []string
	var datas []map[string]any
	for rows.Next() {
		var id string
		var d map[string]any
		if err := rows.Scan(&id, &d); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		datas = append(datas, d)
	}
	return ids, datas, rows.Err()
}

// ExecuteDelete carries out plan in one transaction: it empties the fields
// it clears, deletes every record it deletes and the members those were.
// The record itself is deleted only while it is in rootStatus
// (crudapp.ErrRecordChanged otherwise, and nothing is done). A blocked plan
// is refused.
func (ps *Poster) ExecuteDelete(ctx context.Context, plan *DeletePlan, rootStatus string, userID string) error {
	if !plan.CanDelete() {
		return errors.New("the delete is blocked")
	}
	tx, err := ps.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	for _, c := range plan.clears {
		for _, id := range c.recordIDs {
			var before map[string]any
			if err := tx.QueryRow(ctx, `SELECT data FROM runtime.form_record WHERE id = $1::uuid FOR UPDATE`, id).Scan(&before); err != nil {
				return fmt.Errorf("clear %s: %w", c.field, err)
			}
			if _, err := tx.Exec(ctx, `UPDATE runtime.form_record SET data = data - $2::text, updated_at = now() WHERE id = $1::uuid`, id, c.field); err != nil {
				return fmt.Errorf("clear %s: %w", c.field, err)
			}
			if slices.Contains(c.form.HistoryFields(), c.field) {
				after := map[string]any{}
				for k, v := range before {
					if k != c.field {
						after[k] = v
					}
				}
				if err := crudapp.LogFieldChanges(ctx, tx, c.form, id, before, after, userID); err != nil {
					return err
				}
			}
		}
	}
	for i, d := range plan.deletes {
		if i == 0 {
			ct, err := tx.Exec(ctx,
				`DELETE FROM runtime.form_record WHERE id = $1::uuid AND status IS NOT DISTINCT FROM NULLIF($2,'')::runtime.record_status`,
				d.recordID, rootStatus)
			if err != nil {
				return err
			}
			if ct.RowsAffected() == 0 {
				return crudapp.ErrRecordChanged
			}
		} else if _, err := tx.Exec(ctx, `DELETE FROM runtime.form_record WHERE id = $1::uuid`, d.recordID); err != nil {
			return err
		}
	}
	// The members last, once nothing deleted here names them.
	for _, d := range plan.deletes {
		if err := DropKeptMember(ctx, tx, d.form, d.code); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

func joinAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	out := ""
	for i, p := range parts {
		switch {
		case i == 0:
		case i == len(parts)-1:
			out += " and "
		default:
			out += ", "
		}
		out += p
	}
	return out
}
