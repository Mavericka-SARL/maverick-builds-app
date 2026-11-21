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

// tableLoad is how importFormRows loads rows into a table.
type tableLoad struct {
	// dryRun checks every row and creates nothing.
	dryRun bool
	// replaceBy names the field a load replaces by (recordpost.ImportRequest):
	// the records it deletes are ones act may delete, as by hand.
	replaceBy string
	// matchBy names the field a row is matched to the record it updates by
	// (a source system's id, or the auto-code); "" matches by the table's
	// typed code.
	matchBy string
	// missingKeeps: a row with no cell for a column keeps the value the
	// record it updates holds (recordpost.ImportRequest.MissingKeeps).
	missingKeeps bool
	// beforeCommit (recordpost.ImportRequest.BeforeCommit) runs in the
	// records' transaction: the records import writes its Recent Imports
	// line, with its Idempotency-Key, there.
	beforeCommit func(context.Context, pgx.Tx, recordpost.ImportResult) error
}

// importFormRows creates a record of form for each row as act, in the
// statuses scope lets them create (recordpost.ImportRows): POST
// /api/forms/{id}/import, a saved csv_import or google_sheets integration
// whose target is a table, and the AI Developer's import of an attached
// file. dryRun checks every row and creates nothing.
// load says how (tableLoad).
func (h *handler) importFormRows(ctx context.Context, act *actor, scope formRecordScope, form *crudapp.FormDef, header []string, rawRows []importpkg.RawRow, load tableLoad) (recordpost.ImportResult, error) {
	dryRun, replaceBy, beforeCommit := load.dryRun, load.replaceBy, load.beforeCommit
	// Each record a file adds to a table keeping a dimension is a new
	// member: within the edition's members per dimension, as a record saved
	// by hand is. A row updating the record holding its code adds none.
	var newMembers func(n int) error
	if form.KeepsDimension() && h.plans != nil {
		if cid, _ := h.customerOfDimension(ctx, form.KeepsDimensionID); cid != "" {
			newMembers = func(n int) error {
				return h.plans.CheckMembers(ctx, h.db.For(ctx), cid, form.KeepsDimensionID, n)
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
		Form: form, CreatorID: act.UserID, Admin: scope.admin, Header: header, Rows: rawRows,
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
		DryRun: dryRun, ReplaceBy: replaceBy, MatchBy: load.matchBy, MissingKeeps: load.missingKeeps, ByPerson: true, BeforeCommit: beforeCommit,
		DeleteRefusal: func(rec *crudapp.FormRecord) string {
			if !scope.access(rec).CanDelete(rec.Status) {
				return "you may not delete it"
			}
			return ""
		},
		// A row whose code a record holds updates it, as act may by hand.
		UpdateRefusal: func(rec *crudapp.FormRecord, status string) string {
			access := scope.access(rec)
			switch {
			case access.ParentDecided && !access.Admin:
				msg, _, err := h.parentRefusal(ctx, &scope, rec.Data)
				if err != nil {
					parentErr = err
					return "the record it belongs to could not be read"
				}
				return strings.TrimPrefix(msg, "forbidden: ")
			case access.CanUpdate(rec.Status, status):
			case status != rec.Status:
				return fmt.Sprintf("you may not move it from %s to %s", rec.Status, status)
			default:
				return "you may not change it"
			}
			return ""
		},
		NewMembers: newMembers,
		// A row updates only a record act may see: one they may not is not
		// there for them, as on the records widget.
		VisibleIDs: func(ctx context.Context, ids []string) (map[string]bool, error) {
			return h.visibleRecordIDs(ctx, act, scope, form, ids)
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
// replace is refused (409) or whose replace_by or match_by is not a field it
// may replace or match by (400), a plan limit, or a server error.
func (h *handler) jsonImportErr(w http.ResponseWriter, err error) {
	switch {
	case recordpost.IsReplaceRefused(err):
		jsonErr(w, err, http.StatusConflict)
	case strings.HasPrefix(err.Error(), "replace_by:"), strings.HasPrefix(err.Error(), "match_by:"):
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

// visibleRecordIDs keeps the ids, of form's records, of those act may see
// (recordVisibility, as the records widget decides).
func (h *handler) visibleRecordIDs(ctx context.Context, act *actor, scope formRecordScope, form *crudapp.FormDef, ids []string) (map[string]bool, error) {
	vis, err := h.recordVisibility(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(ids))
	if vis.Nothing {
		return out, nil
	}
	if vis.OwnerID == "" && len(vis.HiddenCodes) == 0 && len(vis.HiddenNames) == 0 && len(vis.WithheldFields) == 0 {
		for _, id := range ids {
			out[id] = true
		}
		return out, nil
	}
	store := crudapp.NewStore(h.db.For(ctx))
	for start := 0; start < len(ids); start += crudapp.MaxRecordLimit {
		chunk := ids[start:min(start+crudapp.MaxRecordLimit, len(ids))]
		vals := make([]any, len(chunk))
		for i, id := range chunk {
			vals[i] = id
		}
		res, err := store.QueryRecords(ctx, form, vis, act.UserID, crudapp.Query{
			Filters: []crudapp.Filter{{Field: "id", Op: "in", Values: vals}},
			Limit:   len(chunk),
		})
		if err != nil {
			return nil, err
		}
		for _, r := range res.Records {
			out[r.ID] = true
		}
	}
	return out, nil
}

// checkMatchBy says why table tableID's loader may not match records by name
// (nil when it may).
func (h *handler) checkMatchBy(ctx context.Context, tableID, name string) error {
	form, err := crudapp.NewStore(h.db.For(ctx)).GetForm(ctx, tableID)
	if err != nil {
		return fmt.Errorf("match_by: the loader's table: %w", err)
	}
	_, err = recordpost.MatchField(form, name)
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
// loadNote says what a table load did besides adding records: the records
// it updated by their code, those whose row changed nothing, the earlier
// records it replaced ("" when none).
func loadNote(res recordpost.ImportResult) string {
	var parts []string
	if res.Updated == 1 {
		parts = append(parts, "updated 1 record by its code")
	} else if res.Updated > 1 {
		parts = append(parts, fmt.Sprintf("updated %d records by their code", res.Updated))
	}
	if res.Unchanged > 0 {
		parts = append(parts, fmt.Sprintf("%d unchanged", res.Unchanged))
	}
	if res.Replaced > 0 {
		parts = append(parts, replacedNote(res.Replaced))
	}
	return strings.Join(parts, ", ")
}

func replacedNote(n int) string {
	if n == 1 {
		return "replaced 1 earlier record"
	}
	return fmt.Sprintf("replaced %d earlier records", n)
}
