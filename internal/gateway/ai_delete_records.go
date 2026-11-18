package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/recordpost"
)

// aiDeleteRecords deletes (or, dryRun, checks) a delete_records step's
// records as userID, the developer in the chat, each as DELETE
// /api/records/{id} deletes it (deleteRecord): a record they do not see is
// one the table does not have, a value two records hold names neither, and
// every record is checked before any is deleted — one refused refuses the
// step. The assistant could not delete a record and asked the developer to
// do it by hand (2026-11-17).
func (h *handler) aiDeleteRecords(ctx context.Context, userID string, req aiassistant.RecordsDelete, dryRun bool) (string, error) {
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
	form := scope.form
	if !scope.reach {
		return "", fmt.Errorf("you cannot reach the records of table '%s'", form.Name)
	}
	// The field each value names a record by: match_by, else the table's
	// code; a record id names its record in any table.
	field := form.CodeField()
	if req.MatchBy != "" {
		if field, err = recordpost.MatchField(form, req.MatchBy); err != nil {
			return "", err
		}
	}
	fieldLabel := "id"
	ids := map[string][]string{}
	if field != nil {
		fieldLabel = firstNonEmpty(field.Label, field.Name)
		if ids, err = recordpost.RecordIDsMatching(ctx, h.db.For(ctx), form, field, req.Values); err != nil {
			return "", err
		}
	}
	store := crudapp.NewStore(h.db.For(ctx))
	type target struct {
		value  string
		rec    *crudapp.FormRecord
		scope  formRecordScope
		access crudapp.RecordAccess
	}
	var targets []target
	var refusals []string
	refuse := func(value, msg string) {
		refusals = append(refusals, fmt.Sprintf("- %q: %s", value, msg))
	}
	taken := map[string]bool{} // a record named twice (by code and by id) is deleted once
	for _, v := range req.Values {
		found := ids[v]
		if len(found) == 0 {
			if _, perr := uuid.Parse(v); perr == nil {
				found = []string{v}
			}
		}
		var reached []target
		for _, id := range found {
			rec, rs, access, rerr := h.reachRecord(ctx, act, store, id)
			if errors.Is(rerr, errRecordNotReached) {
				continue // a record they do not see is not there for them
			} else if rerr != nil {
				return "", rerr
			}
			if rec.FormID != form.ID {
				continue
			}
			reached = append(reached, target{value: v, rec: rec, scope: rs, access: access})
		}
		switch {
		case len(reached) == 0:
			refuse(v, fmt.Sprintf("no record of %s you can see has %s %q", form.Label, fieldLabel, v))
			continue
		case len(reached) > 1:
			refuse(v, fmt.Sprintf("%d records of %s hold %s %q: name each by its code instead", len(reached), form.Label, fieldLabel, v))
			continue
		}
		t := reached[0]
		if taken[t.rec.ID] {
			continue
		}
		taken[t.rec.ID] = true
		if _, cerr := h.deleteRecord(ctx, act, store, t.rec, t.scope, t.access, true); cerr != nil {
			var ref *recordDeleteRefusal
			if !errors.As(cerr, &ref) {
				return "", cerr
			}
			refuse(v, strings.TrimPrefix(ref.msg, "forbidden: "))
			continue
		}
		targets = append(targets, t)
	}
	if len(refusals) > 0 {
		shown := refusals
		if len(shown) > 15 {
			shown = append(shown[:15:15], fmt.Sprintf("- … and %d more", len(refusals)-15))
		}
		return "", fmt.Errorf("%d of the %d record(s) cannot be deleted, so none was — the step is all or nothing:\n%s",
			len(refusals), len(req.Values), strings.Join(shown, "\n"))
	}
	deleted, withThem, cleared := 0, 0, 0
	for _, t := range targets {
		res, derr := h.deleteRecord(ctx, act, store, t.rec, t.scope, t.access, dryRun)
		if derr != nil {
			msg := derr.Error()
			if dryRun || deleted == 0 {
				return "", fmt.Errorf("%q: %s", t.value, msg)
			}
			// Each record is deleted on its own, as by hand: what went
			// before this one stays deleted, and the result says so.
			return "", fmt.Errorf("%d record(s) of %s deleted, then %q failed and the rest were not deleted: %s",
				deleted, form.Label, t.value, msg)
		}
		deleted++
		withThem += res.deleted - 1
		cleared += res.cleared
	}
	verb := map[bool]string{false: "deleted", true: "would be deleted"}[dryRun]
	out := fmt.Sprintf("Table '%s': %d record(s) %s", form.Name, deleted, verb)
	if withThem > 0 {
		out += fmt.Sprintf(", with %d linked record(s) their fields' delete rules take with them", withThem)
	}
	if cleared > 0 {
		out += fmt.Sprintf("; %d linked record(s) have the field naming them emptied", cleared)
	}
	return out, nil
}
