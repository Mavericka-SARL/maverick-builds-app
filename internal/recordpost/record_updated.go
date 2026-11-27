package recordpost

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
)

// A rule that fires when a record changes (trigger record_updated, migration
// 199): a rescheduled activity starts the check of the contracts ending
// before it, instead of the Monday watch finding it (development-programme
// build, 2026-11-27). Every save path that changes a record fires it — the
// record form, a board move, the API and connector, a file's row — with the
// fields that changed and what they held before and after; a rule watching
// fields fires only for a change of one of them.

// UpdatedRecord is a record as it was and as a save left it.
type UpdatedRecord struct {
	Old, New *crudapp.FormRecord
}

// ChangedFields are the fields whose value differs between old and new, in
// name order ("status" too when the status changed). A field absent, null
// and "" are the same: nothing.
func ChangedFields(oldData, newData map[string]any, oldStatus, newStatus string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range []map[string]any{oldData, newData} {
		for k := range m {
			if seen[k] {
				continue
			}
			seen[k] = true
			if !sameFieldValue(oldData[k], newData[k]) {
				out = append(out, k)
			}
		}
	}
	if oldStatus != newStatus {
		out = append(out, "status")
	}
	sort.Strings(out)
	return out
}

func sameFieldValue(a, b any) bool {
	if empty(a) && empty(b) {
		return true
	}
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

func empty(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

// fieldText is a field's value as a rule's payload carries it.
func fieldText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// UpdatedPayload is a record_updated rule's payload: the record, its status,
// the changed fields (comma-separated) and, per changed field, old_<field>
// and new_<field>.
func UpdatedPayload(formID, recordID, oldStatus, newStatus string, oldData, newData map[string]any, changed []string) map[string]string {
	p := map[string]string{"form_id": formID, "record_id": recordID, "status": newStatus, "changed_fields": strings.Join(changed, ",")}
	for _, f := range changed {
		if f == "status" {
			p["old_status"], p["new_status"] = oldStatus, newStatus
			continue
		}
		p["old_"+f], p["new_"+f] = fieldText(oldData[f]), fieldText(newData[f])
	}
	return p
}

// DispatchRecordUpdated fires, in the background, the record_updated rules
// of formID for a record a save changed; nothing when nothing changed.
// imported: a file's row changed it (rules switched off for imports skip it).
func (ps *Poster) DispatchRecordUpdated(ctx context.Context, appID, revisionID, formID, recordID, oldStatus, newStatus string, oldData, newData map[string]any, userID string, imported bool) {
	changed := ChangedFields(oldData, newData, oldStatus, newStatus)
	if len(changed) == 0 {
		return
	}
	payload := UpdatedPayload(formID, recordID, oldStatus, newStatus, oldData, newData, changed)
	if imported {
		payload["record_source"] = "import"
	}
	bgCtx := context.WithoutCancel(ctx)
	go ps.workflowStore(ctx).DispatchEventRules(bgCtx, appID, revisionID, "record_updated", formID, userID, payload)
}
