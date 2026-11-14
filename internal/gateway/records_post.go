package gateway

// The gateway's side of internal/recordpost, where what access rules mean
// for a table's records and how records post into metrics live — shared with
// the integration worker, which posts a scheduled connector's rows the same
// way. These keep the names the gateway's routes call them by.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/recordpost"
)

type (
	accessView   = recordpost.AccessView
	fieldProblem = recordpost.FieldProblem
	recordState  = recordpost.RecordState
	formRowError = recordpost.RowError
)

// newMemberCache caches the members a record's values name, for one save.
func newMemberCache() *recordpost.MemberCache { return recordpost.NewMemberCache() }

// records is the record poster on the caller's tenant database.
func (h *handler) records() *recordpost.Poster {
	return &recordpost.Poster{Pool: h.db.For, Log: h.log, MembersChanged: func(ctx context.Context, form *crudapp.FormDef) {
		h.recalcDimensionDependents(ctx, form.KeepsDimensionID)
	}, RecalcDone: func(ctx context.Context, _ string) {
		h.resumeHeldConditions(ctx)
	}}
}

func (h *handler) accessViewFor(ctx context.Context, userID string, form *crudapp.FormDef) (*accessView, error) {
	return h.records().AccessViewFor(ctx, userID, form)
}

func (h *handler) recordValueProblems(ctx context.Context, view *accessView, form *crudapp.FormDef, current, data map[string]any, required bool, leaves *recordpost.MemberCache) ([]fieldProblem, error) {
	return h.records().RecordValueProblems(ctx, view, form, current, data, required, leaves)
}

func (h *handler) postingRefusal(ctx context.Context, creatorID string, forYou bool, form *crudapp.FormDef, recordID string, to *recordState) (int, string, error) {
	return h.records().PostingRefusal(ctx, creatorID, forYou, form, recordID, to)
}

func (h *handler) applyFormMappings(ctx context.Context, recordID, formID, status string, data map[string]any, userID string) error {
	return h.records().Apply(ctx, recordID, formID, status, data, userID)
}

func (h *handler) recomputeFactInput(ctx context.Context, mappingID, revisionID, userID string) {
	h.records().RecomputeFactInput(ctx, mappingID, revisionID, userID)
}

// backfillFormMapping posts the records already in mappingID's table
// (recordpost.Backfill): how many it went through and the postings a guard
// kept out, by why.
func (h *handler) backfillFormMapping(ctx context.Context, mappingID, userID string) (int, int, []recordpost.SkippedReason, error) {
	n, skipped, err := h.records().Backfill(ctx, mappingID, userID)
	total, reasons := recordpost.SkippedList(skipped)
	return n, total, reasons, err
}

func (h *handler) repostTable(ctx context.Context, formID, userID string) recordpost.RepostResult {
	return h.records().RepostTable(ctx, formID, userID)
}

// postingReport is what a Sync or a backfill answers: "ok", or "partial"
// with the postings a lock, a closed period or the creators' access kept
// out and why — an "ok" used to hide a released period's lock (2026-11-13).
func postingReport(body map[string]any, skipped int, reasons []recordpost.SkippedReason) map[string]any {
	body["status"] = "ok"
	body["postings_skipped"] = skipped
	body["skipped_reasons"] = reasons
	if skipped > 0 {
		body["status"] = "partial"
	}
	return body
}

func (h *handler) dispatchRecordCreated(ctx context.Context, appID, revisionID, formID, recordID, status, userID string) {
	h.records().DispatchRecordCreated(ctx, appID, revisionID, formID, recordID, status, userID, false)
}

func (h *handler) workflowSetsRecordStatus(ctx context.Context, recordID, status, byUserID string) {
	h.records().WorkflowRecordStatus(ctx, recordID, status, byUserID)
}

// retryTransient runs op up to attempts times, pausing between tries
// (recordpost.RetryTransient).
func retryTransient(ctx context.Context, attempts int, op func() error) error {
	return recordpost.RetryTransient(ctx, attempts, op)
}

// importFormRows creates a record of form for each row as act, in the
// statuses scope lets them create (recordpost.ImportRows): POST
// /api/forms/{id}/import, a saved csv_import or google_sheets integration
// whose target is a table, and the AI Developer's import of an attached
// file. dryRun checks every row and creates nothing.
// replaceBy names the field a load replaces by (recordpost.ImportRequest):
// the records it deletes are ones act may delete, as by hand. beforeCommit
// (recordpost.ImportRequest.BeforeCommit) runs in the records' transaction:
// the records import writes its Recent Imports line, with its
// Idempotency-Key, there.
func (h *handler) importFormRows(ctx context.Context, act *actor, scope formRecordScope, form *crudapp.FormDef, header []string, rawRows []importpkg.RawRow, dryRun bool, replaceBy string, beforeCommit func(context.Context, pgx.Tx, recordpost.ImportResult) error) (recordpost.ImportResult, error) {
	// Each record of a table keeping a dimension is a new member: within the
	// edition's members per dimension, as a record saved by hand is.
	if form.KeepsDimension() && h.plans != nil {
		if cid, _ := h.customerOfDimension(ctx, form.KeepsDimensionID); cid != "" {
			if err := h.plans.CheckMembers(ctx, h.db.For(ctx), cid, form.KeepsDimensionID, len(rawRows)); err != nil {
				return recordpost.ImportResult{}, err
			}
		}
	}
	// Rows under an approved or rejected record: refused for anyone but an
	// administrator, whose load sends those records back to draft.
	var underDecided []map[string]any
	var parentErr error
	res, err := h.records().Import(ctx, recordpost.ImportRequest{
		RecordRefusal: func(data map[string]any) string {
			msg, p, err := h.parentRefusal(ctx, &scope, data)
			if err != nil {
				parentErr = err
				return "the record it belongs to could not be read"
			}
			if p != nil && msg == "" {
				underDecided = append(underDecided, data)
			}
			return strings.TrimPrefix(msg, "forbidden: ")
		},
		Form: form, CreatorID: act.UserID, Header: header, Rows: rawRows,
		NewStatus: scope.newRecordStatus(),
		StatusRefusal: func(status string) string {
			if scope.noAdd {
				return scope.noAddReason()
			}
			if !scope.createAccess().CanCreate(status) {
				return fmt.Sprintf("only an administrator of this application imports a record as %s", status)
			}
			return ""
		},
		AppID: scope.appID, RevisionID: scope.revisionID,
		DryRun: dryRun, ReplaceBy: replaceBy, ByPerson: true, BeforeCommit: beforeCommit,
		DeleteRefusal: func(rec *crudapp.FormRecord) string {
			if !scope.access(rec).CanDelete(rec.Status) {
				return "you may not delete it"
			}
			return ""
		},
	})
	if err == nil && parentErr != nil {
		err = parentErr
	}
	if err == nil && !dryRun && len(underDecided) > 0 {
		h.reopenParents(ctx, &scope, act, underDecided...)
	}
	return res, err
}

// jsonImportErr answers an import that failed as a whole: a load whose
// replace is refused (409) or whose replace_by is not a field it may replace
// by (400), a plan limit, or a server error.
func (h *handler) jsonImportErr(w http.ResponseWriter, err error) {
	switch {
	case recordpost.IsReplaceRefused(err):
		jsonErr(w, err, http.StatusConflict)
	case strings.HasPrefix(err.Error(), "replace_by:"):
		jsonErr(w, err, http.StatusBadRequest)
	default:
		h.jsonLimitErr(w, err)
	}
}

// checkReplaceBy says why table tableID's loader may not replace by name
// (nil when it may).
func (h *handler) checkReplaceBy(ctx context.Context, tableID, name string) error {
	form, err := crudapp.NewStore(h.db.For(ctx)).GetForm(ctx, tableID)
	if err != nil {
		return fmt.Errorf("replace_by: the loader's table: %w", err)
	}
	_, err = recordpost.ReplaceFields(form, name)
	return err
}

// replaceBySpec is a loader's replace_by as configured: one field name, or
// several — "entity, period" or ["entity", "period"] — held as names
// separated by commas (recordpost.ReplaceFields).
type replaceBySpec string

func (r *replaceBySpec) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*r = replaceBySpec(one)
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("replace_by is a field name or a list of them")
	}
	*r = replaceBySpec(strings.Join(many, ","))
	return nil
}

// replacedNote is what a run that replaced records says of them.
func replacedNote(n int) string {
	if n == 1 {
		return "replaced 1 earlier record"
	}
	return fmt.Sprintf("replaced %d earlier records", n)
}
