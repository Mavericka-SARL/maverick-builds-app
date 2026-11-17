package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// aiSaveRecords saves (or, dryRun, checks) a save_records step's records as
// userID, the developer in the chat: the table import a person's file goes
// through (importFormRows) — each record's fields, the statuses they may
// create, who may add records, where each posts, the edition's members, the
// rules a new record fires; nothing saved when any record fails; a record
// whose typed code a record holds updates it. The assistant could not add a
// record and asked the developer to type it (2026-11-16).
func (h *handler) aiSaveRecords(ctx context.Context, userID string, req aiassistant.RecordsSave, dryRun bool) (string, error) {
	act, err := h.actorByUserID(ctx, userID)
	if err != nil {
		return "", err
	}
	scope, err := h.resolveFormRecordScope(ctx, act, req.TableID)
	if err != nil {
		return "", err
	}
	if scope.form == nil {
		return "", fmt.Errorf("table %s not found", req.TableID)
	}
	table := scope.form.Name
	if !scope.reach {
		return "", fmt.Errorf("you cannot add records to table '%s'", table)
	}
	res, err := h.importFormRows(ctx, act, scope, scope.form, req.Header, req.Rows, dryRun, "", nil)
	if err != nil {
		return "", err
	}
	if len(res.Errors) > 0 {
		lines := make([]map[string]any, 0, len(res.Errors))
		for _, e := range res.Errors {
			lines = append(lines, map[string]any{"row": e.Row, "column": e.Column, "message": e.Message})
		}
		return "", fmt.Errorf("%d record(s) refused, so none was saved — the step is all or nothing (\"row\" is the record's "+
			"position in records; name each field by its name or label):\n%s", len(res.Errors), importErrorLines(lines, 15))
	}
	if res.Written() == 0 && res.Unchanged == 0 {
		return "", fmt.Errorf("the records hold no field of table '%s': name each field by its name or label", table)
	}
	var parts []string
	verb := map[bool][3]string{false: {"added", "updated by their code", "unchanged"}, true: {"would be added", "would be updated by their code", "would stay unchanged"}}[dryRun]
	if res.Created > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", res.Created, verb[0]))
	}
	if res.Updated > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", res.Updated, verb[1]))
	}
	if res.Unchanged > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", res.Unchanged, verb[2]))
	}
	return fmt.Sprintf("Table '%s': %s", table, strings.Join(parts, ", ")), nil
}
