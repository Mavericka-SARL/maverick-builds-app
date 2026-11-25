package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// Records that belong to a record follow its approval (owner decision
// 2026-11-13). A journal's lines are records of their own table naming the
// journal; once the CFO approved journal M04, its preparer changed a line
// 7 → 70 and the journal stayed "approved" while it posted 63 out of
// balance. Now a record naming, through a belonging field, a record that is
// approved or rejected is its parent's: its creator may not change, add or
// delete it (as they may not change the parent), and an administrator who
// does sends the parent back to draft, so it posts nothing until it is
// approved again.
//
// A belonging field is a dimension field whose dimension a table with a
// status field keeps, unless its delete rule is "clear" (empty this field):
// that one is a reference, not a belonging, as record access decides.

// parentLink is one belonging field of a table and the table it names.
type parentLink struct {
	field  string
	parent *crudapp.FormDef
}

// parentRecord is a record a record belongs to.
type parentRecord struct {
	id, formID, code, label, status string
	tableLabel                      string
}

// decided reports whether the parent's decision binds the records under it.
func (p parentRecord) decided() bool {
	return p.status == crudapp.StatusApproved || p.status == crudapp.StatusRejected
}

// name is the parent as a message names it: its table's record and code.
func (p parentRecord) name() string {
	what := p.tableLabel
	if what == "" {
		what = "record"
	}
	name := p.code
	if p.label != "" && p.label != p.code {
		name += " (" + p.label + ")"
	}
	return fmt.Sprintf("%s %s", strings.TrimSuffix(what, "s"), name)
}

// parentCache holds a table's belonging fields and the parents read so far.
type parentCache struct {
	links  []parentLink
	byCode map[string]parentRecord // parent table id + "\x1f" + code
}

// parentLinks are form's belonging fields.
func (h *handler) parentLinks(ctx context.Context, form *crudapp.FormDef) ([]parentLink, error) {
	if form == nil {
		return nil, nil
	}
	var out []parentLink
	store := crudapp.NewStore(h.db.For(ctx))
	for _, f := range form.Fields {
		if f.Type != "dimension" || f.DimensionID == "" || f.OnDelete == crudapp.OnDeleteClear || f.Multiple { // a list names no parent
			continue
		}
		var parentID string
		err := h.db.QueryRow(ctx, `SELECT id::text FROM model.form_def WHERE keeps_dimension_id = $1::uuid AND id <> $2::uuid LIMIT 1`,
			f.DimensionID, form.ID).Scan(&parentID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		parent, err := store.GetForm(ctx, parentID)
		if err != nil {
			return nil, err
		}
		if parent.HasStatus() && parent.CodeField() != nil {
			out = append(out, parentLink{field: f.Name, parent: parent})
		}
	}
	return out, nil
}

// withParents gives scope its table's belonging fields, read once.
func (h *handler) withParents(ctx context.Context, scope *formRecordScope) error {
	if scope.parents != nil || scope.form == nil {
		return nil
	}
	links, err := h.parentLinks(ctx, scope.form)
	if err != nil {
		return err
	}
	scope.parents = &parentCache{links: links, byCode: map[string]parentRecord{}}
	return nil
}

// recordParents are the parents data names, read into the scope's cache.
func (h *handler) recordParents(ctx context.Context, scope *formRecordScope, data map[string]any) ([]parentRecord, error) {
	if err := h.withParents(ctx, scope); err != nil || scope.parents == nil || len(scope.parents.links) == 0 {
		return nil, err
	}
	var out []parentRecord
	for _, l := range scope.parents.links {
		code, _ := data[l.field].(string)
		code = strings.TrimSpace(code)
		if code == "" {
			continue
		}
		key := l.parent.ID + "\x1f" + code
		p, ok := scope.parents.byCode[key]
		if !ok {
			cf := l.parent.CodeField().Name
			labelField := l.parent.MemberLabelField
			if labelField == "" {
				labelField = cf
			}
			err := h.db.QueryRow(ctx, `
				SELECT id::text, COALESCE(status::text, ''), COALESCE(data->>$3, '')
				FROM runtime.form_record WHERE form_id = $1::uuid AND data->>$2 = $4 LIMIT 1`,
				l.parent.ID, cf, labelField, code).Scan(&p.id, &p.status, &p.label)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			p.formID, p.code, p.tableLabel = l.parent.ID, code, l.parent.Label
			scope.parents.byCode[key] = p
		}
		if p.id != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// decidedParent is the first approved or rejected parent of any of datas.
func (h *handler) decidedParent(ctx context.Context, scope *formRecordScope, datas ...map[string]any) (*parentRecord, error) {
	for _, d := range datas {
		if d == nil {
			continue
		}
		ps, err := h.recordParents(ctx, scope, d)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			if p.decided() {
				return &p, nil
			}
		}
	}
	return nil, nil
}

// prefetchParents reads the parents of records into scope, so permit can
// serve each record's permissions without a read per record.
func (h *handler) prefetchParents(ctx context.Context, scope *formRecordScope, records []*crudapp.FormRecord) error {
	for _, rec := range records {
		if rec == nil {
			continue
		}
		if _, err := h.recordParents(ctx, scope, rec.Data); err != nil {
			return err
		}
	}
	return nil
}

// parentDecidedCached reports, from what the cache holds, whether data
// names a decided parent.
func (s formRecordScope) parentDecidedCached(data map[string]any) bool {
	if s.parents == nil {
		return false
	}
	for _, l := range s.parents.links {
		code, _ := data[l.field].(string)
		if p, ok := s.parents.byCode[l.parent.ID+"\x1f"+strings.TrimSpace(code)]; ok && p.decided() {
			return true
		}
	}
	return false
}

// parentRefusal is why the scope's caller may not save or delete a record
// whose data (before and after) names a decided parent; "" when they may.
// An administrator may: the parent then goes back to draft (reopenParents).
func (h *handler) parentRefusal(ctx context.Context, scope *formRecordScope, datas ...map[string]any) (string, *parentRecord, error) {
	p, err := h.decidedParent(ctx, scope, datas...)
	if err != nil || p == nil {
		return "", p, err
	}
	if scope.admin {
		return "", p, nil
	}
	return fmt.Sprintf("forbidden: this record belongs to %s, which is %s: it changes only once that record is back to draft",
		p.name(), p.status), p, nil
}

// reopenParents sends the decided parents of datas back to draft after an
// administrator changed a record under them: they post as drafts and need
// approval again. byUserID is the administrator.
func (h *handler) reopenParents(ctx context.Context, scope *formRecordScope, act *actor, datas ...map[string]any) {
	seen := map[string]bool{}
	for _, d := range datas {
		if d == nil {
			continue
		}
		ps, err := h.recordParents(ctx, scope, d)
		if err != nil {
			h.log.Warn().Err(err).Msg("reading a changed record's parent")
			continue
		}
		for _, p := range ps {
			if !p.decided() || seen[p.id] {
				continue
			}
			seen[p.id] = true
			var data []byte
			err := h.db.QueryRow(ctx, `
				UPDATE runtime.form_record SET status = 'draft'::runtime.record_status, updated_at = now()
				WHERE id = $1::uuid AND status IN ('approved'::runtime.record_status, 'rejected'::runtime.record_status)
				RETURNING data`, p.id).Scan(&data)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				h.log.Warn().Err(err).Str("record_id", p.id).Msg("sending a parent record back to draft")
				continue
			}
			var parentData map[string]any
			_ = json.Unmarshal(data, &parentData)
			bgCtx := context.WithoutCancel(ctx)
			go func(id, formID string, pd map[string]any) {
				_ = h.applyFormMappings(bgCtx, id, formID, crudapp.StatusDraft, pd, act.UserID)
			}(p.id, p.formID, parentData)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryDataChange, EventType: auditlog.EventRecordUpdated,
				ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
				ApplicationID: scope.appID, ResourceType: "form_record", ResourceID: p.id,
				Metadata: map[string]string{"form_id": p.formID, "status": crudapp.StatusDraft, "previous_status": p.status,
					"set_by": "a record belonging to it changed"},
			})
			p.status = crudapp.StatusDraft
			scope.parents.byCode[p.formID+"\x1f"+p.code] = p
		}
	}
}
