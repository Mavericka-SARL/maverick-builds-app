package gateway

// The gateway's side of internal/recordpost, where what access rules mean
// for a table's records and how records post into metrics live — shared with
// the integration worker, which posts a scheduled connector's rows the same
// way. These keep the names the gateway's routes call them by.

import (
	"context"
	"fmt"

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

// records is the record poster on the caller's tenant database.
func (h *handler) records() *recordpost.Poster {
	return &recordpost.Poster{Pool: h.db.For, Log: h.log, MembersChanged: func(ctx context.Context, form *crudapp.FormDef) {
		h.recalcDimensionDependents(ctx, form.KeepsDimensionID)
	}}
}

func (h *handler) accessViewFor(ctx context.Context, userID string, form *crudapp.FormDef) (*accessView, error) {
	return h.records().AccessViewFor(ctx, userID, form)
}

func (h *handler) recordValueProblems(ctx context.Context, view *accessView, form *crudapp.FormDef, current, data map[string]any, required bool, leaves map[string]map[string]bool) ([]fieldProblem, error) {
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

func (h *handler) backfillFormMapping(ctx context.Context, mappingID, userID string) (int, error) {
	return h.records().Backfill(ctx, mappingID, userID)
}

func (h *handler) repostTable(ctx context.Context, formID, userID string) (mappings, processed int) {
	return h.records().RepostTable(ctx, formID, userID)
}

func (h *handler) dispatchRecordCreated(ctx context.Context, appID, revisionID, formID, recordID, status, userID string) {
	h.records().DispatchRecordCreated(ctx, appID, revisionID, formID, recordID, status, userID)
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
// /api/forms/{id}/import, and a saved csv_import or google_sheets
// integration whose target is a table.
func (h *handler) importFormRows(ctx context.Context, act *actor, scope formRecordScope, form *crudapp.FormDef, header []string, rawRows []importpkg.RawRow) (int, []formRowError, error) {
	// Each record of a table keeping a dimension is a new member: within the
	// edition's members per dimension, as a record saved by hand is.
	if form.KeepsDimension() && h.plans != nil {
		if cid, _ := h.customerOfDimension(ctx, form.KeepsDimensionID); cid != "" {
			if err := h.plans.CheckMembers(ctx, h.db.For(ctx), cid, form.KeepsDimensionID, len(rawRows)); err != nil {
				return 0, nil, err
			}
		}
	}
	return h.records().ImportRows(ctx, recordpost.ImportRequest{
		Form: form, CreatorID: act.UserID, Header: header, Rows: rawRows,
		NewStatus: scope.newRecordStatus(),
		StatusRefusal: func(status string) string {
			if !scope.createAccess().CanCreate(status) {
				return fmt.Sprintf("only an administrator of this application imports a record as %s", status)
			}
			return ""
		},
		AppID: scope.appID, RevisionID: scope.revisionID,
	})
}
