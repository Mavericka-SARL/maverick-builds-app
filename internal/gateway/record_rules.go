package gateway

// The gateway's routes over internal/recordpost, which decides what a
// person's access rules mean for a table's records (see its package
// documentation): which records a caller sees (recordVisibility), the
// field problems a refused save answers with, and whether an approval may
// approve the record its run decides (approvalRefusal).

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
)

// recordVisibility is which of scope's table's records its caller sees.
func (h *handler) recordVisibility(ctx context.Context, scope formRecordScope) (crudapp.Visibility, error) {
	if !scope.reach || scope.form == nil {
		return crudapp.Visibility{Nothing: true}, nil
	}
	view := scope.view
	if view == nil {
		var err error
		if view, err = h.accessViewFor(ctx, scope.userID, scope.form); err != nil {
			return crudapp.Visibility{}, err
		}
	}
	return h.records().VisibilityFor(ctx, view, scope.form, scope.reach, scope.admin, scope.userID)
}

// jsonFieldProblems answers a save refused for its values: 400, the first
// problem as the error and every problem listed.
func jsonFieldProblems(w http.ResponseWriter, problems []fieldProblem) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": problems[0].Message, "problems": problems})
}

// approvalRefusal says why an approval in run instanceID may not be decided
// by act: a record the run decides the status of would be approved into a
// cell its creator may not write (postingRefusal). 0 and "" when it may.
func (h *handler) approvalRefusal(ctx context.Context, act *actor, instanceID string) (int, string, error) {
	store := crudapp.NewStore(h.db.For(ctx))
	for _, id := range h.workflowStore(ctx).DrivenRecords(ctx, instanceID) {
		rec, err := store.GetRecord(ctx, id)
		if err != nil {
			return 0, "", err
		}
		form, err := store.GetForm(ctx, rec.FormID)
		if err != nil {
			return 0, "", err
		}
		creator := act.UserID
		if rec.CreatedBy != nil {
			creator = *rec.CreatedBy
		}
		code, msg, err := h.postingRefusal(ctx, creator, creator == act.UserID, form, rec.ID, &recordState{Status: crudapp.StatusApproved, Data: rec.Data})
		if err != nil || code != 0 {
			if code != 0 {
				msg = "the record this run decides cannot be approved: " + msg
			}
			return code, msg, err
		}
	}
	return 0, "", nil
}
