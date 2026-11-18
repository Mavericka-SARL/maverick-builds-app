package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/recordpost"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// recordDeleteRefusal is a delete refused before anything was deleted:
// status is what DELETE /api/records/{id} answers with, and plan, for a
// record a kept dimension's "block" rule holds, what holds it (409).
type recordDeleteRefusal struct {
	status int
	msg    string
	plan   *recordpost.DeletePlan
}

func (e *recordDeleteRefusal) Error() string { return e.msg }

// recordDeleted is what deleting one record did, or would do.
type recordDeleted struct {
	// deleted counts the records that went, the record's own included (a
	// kept record's cascade adds the records deleted with it); cleared the
	// records whose field naming it was emptied.
	deleted, cleared int
	// kept: the record was a kept dimension's member.
	kept bool
}

// deleteRecord deletes rec — reached by act through reachRecord, with its
// scope and act's access to it — as DELETE /api/records/{id} does, and as
// the AI Developer's delete_records step does for the developer: refused
// under a decided parent, without delete access, while a "block" rule's
// records name a kept record, or when its creator may no longer write where
// it posted; what it posted comes back out of the metrics, a kept record's
// cascade goes with it, and each deletion is audited. checkOnly runs every
// check and deletes nothing. The route used to hold all of this, so the AI
// had no way to delete a record (2026-11-17).
func (h *handler) deleteRecord(ctx context.Context, act *actor, store *crudapp.Store, rec *crudapp.FormRecord, scope formRecordScope, access crudapp.RecordAccess, checkOnly bool) (recordDeleted, error) {
	var out recordDeleted
	if access.ParentDecided && !access.Admin {
		msg, _, err := h.parentRefusal(ctx, &scope, rec.Data)
		if err != nil {
			return out, err
		}
		return out, &recordDeleteRefusal{status: http.StatusForbidden, msg: msg}
	}
	if !access.CanDelete(rec.Status) {
		return out, &recordDeleteRefusal{status: http.StatusForbidden, msg: "forbidden: you may not delete this record"}
	}
	// A record that is a kept dimension's member goes by the delete rules
	// of the fields naming it.
	var plan *recordpost.DeletePlan
	if scope.form.KeepsDimension() {
		out.kept = true
		var err error
		if plan, err = h.records().PlanDelete(ctx, scope.form, rec); err != nil {
			return out, err
		}
		if err := h.restrictDeletePlan(ctx, act, plan); err != nil {
			return out, err
		}
		if !plan.CanDelete() {
			return out, &recordDeleteRefusal{status: http.StatusConflict, msg: plan.Refusal(scope.form.MemberLabel(rec.Data)), plan: plan}
		}
		out.deleted, out.cleared = len(plan.DeletedRecords()), len(plan.ClearedRecords())
	} else {
		out.deleted = 1
	}
	// A creator deleting their record withdraws what it posted: only from
	// cells they may write. An administrator removing someone's record
	// needs no write access there (a record posts within its creator's
	// access).
	if rec.CreatedBy != nil && *rec.CreatedBy == act.UserID {
		if code, msg, err := h.postingRefusal(ctx, act.UserID, true, scope.form, rec.ID, nil); err != nil {
			return out, err
		} else if code != 0 {
			return out, &recordDeleteRefusal{status: code, msg: msg}
		}
	}
	if checkOnly {
		return out, nil
	}
	if plan != nil {
		return out, h.deleteKeptRecord(ctx, act, scope, rec, plan)
	}
	// What the record posted into metrics, taken back out once it is gone
	// (retractPostings).
	postings, err := h.recordPostings(ctx, rec.ID)
	if err != nil {
		return out, err
	}
	if err := store.DeleteRecordFrom(ctx, rec.ID, rec.Status); errors.Is(err, crudapp.ErrRecordChanged) {
		return out, &recordDeleteRefusal{status: http.StatusConflict, msg: "the record changed since it was read; reload it and try again"}
	} else if err != nil {
		return out, err
	}
	if len(postings) > 0 {
		bgCtx, userID := context.WithoutCancel(ctx), act.UserID
		go h.retractPostings(bgCtx, postings, userID)
	}
	if access.ParentDecided {
		h.reopenParents(ctx, &scope, act, rec.Data)
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventRecordDeleted,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: scope.appID, ResourceType: "form_record", ResourceID: rec.ID,
		Metadata: map[string]string{"form_id": rec.FormID, "status": rec.Status},
	})
	return out, nil
}

// deleteKeptRecord deletes rec, a record of a table keeping a dimension, by
// plan (checked by deleteRecord): it, the records deleted with it and the
// member it was go in one transaction, and the fields cleared are emptied.
func (h *handler) deleteKeptRecord(ctx context.Context, act *actor, scope formRecordScope, rec *crudapp.FormRecord, plan *recordpost.DeletePlan) error {
	ps := h.records()
	var postings []recordPosting
	for _, id := range plan.DeletedRecords() {
		ps, err := h.recordPostings(ctx, id)
		if err != nil {
			return err
		}
		postings = append(postings, ps...)
	}
	// Input values filed against a member that goes with its record leave
	// with it; the revision is recalculated only when there were any.
	membersHeldValues := false
	for _, m := range plan.DroppedMembers() {
		var held bool
		if err := h.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runtime.fact_input WHERE dim_members @> jsonb_build_object($1::text, $2::text))`,
			m.DimID, m.Code).Scan(&held); err != nil {
			return err
		}
		membersHeldValues = membersHeldValues || held
	}
	if err := ps.ExecuteDelete(ctx, plan, rec.Status, act.UserID); errors.Is(err, crudapp.ErrRecordChanged) {
		return &recordDeleteRefusal{status: http.StatusConflict, msg: "the record changed since it was read; reload it and try again"}
	} else if err != nil {
		return err
	}
	bgCtx, userID := context.WithoutCancel(ctx), act.UserID
	if len(postings) > 0 {
		go h.retractPostings(bgCtx, postings, userID)
	}
	// A cleared record posts what it holds now.
	store := crudapp.NewStore(h.db.For(ctx))
	for _, c := range plan.ClearedRecords() {
		if cur, err := store.GetRecord(ctx, c.RecordID); err == nil {
			formID, status, data := cur.FormID, cur.Status, cur.Data
			go func() { _ = h.applyFormMappings(bgCtx, c.RecordID, formID, status, data, userID) }()
		}
	}
	dims := map[string]bool{}
	for _, m := range plan.DroppedMembers() {
		dims[m.DimID] = true
	}
	h.recalcInBackground(ctx, scope.revisionID, "recalculation after a record and its member were deleted", func(bg context.Context) {
		if membersHeldValues {
			if err := h.recalcRevisionFromInputs(bg, scope.modelID, scope.revisionID); err != nil {
				h.log.Warn().Err(err).Str("revision", scope.revisionID).Msg("recalc after a kept member was removed")
			}
		}
		for d := range dims {
			h.recalcDimensionDependents(bg, d)
		}
	})
	deleted := plan.DeletedRecords()
	for i, id := range deleted {
		meta := map[string]string{"form_id": rec.FormID, "status": rec.Status}
		if i == 0 {
			meta["deleted_with_it"] = strconv.Itoa(len(deleted) - 1)
			meta["cleared"] = strconv.Itoa(len(plan.ClearedRecords()))
		} else {
			meta = map[string]string{"deleted_with": rec.ID}
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryDataChange, EventType: auditlog.EventRecordDeleted,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ApplicationID: scope.appID, ResourceType: "form_record", ResourceID: id,
			Metadata: meta,
		})
	}
	return nil
}

// jsonRecordDeleteErr answers DELETE /api/records/{id} that deleteRecord
// refused or failed: a "block" rule's refusal with its plan (409), another
// refusal with its status, anything else a server error.
func jsonRecordDeleteErr(w http.ResponseWriter, err error) {
	var ref *recordDeleteRefusal
	switch {
	case errors.As(err, &ref) && ref.plan != nil:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": ref.msg, "plan": ref.plan})
	case errors.As(err, &ref):
		jsonErr(w, errors.New(ref.msg), ref.status)
	default:
		jsonErr(w, err, http.StatusInternalServerError)
	}
}
