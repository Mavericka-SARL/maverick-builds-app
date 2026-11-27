package crudapp

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// WatchFieldsRefusal says why fields cannot be an automation rule's
// watch_fields ("" when they can): only a record_updated rule watches fields,
// and on a rule for one table each must be a field of it (or "status") — a
// misspelt field would never fire (migration 199). The developer API and the
// AI Developer check alike.
func (s *Store) WatchFieldsRefusal(ctx context.Context, triggerType, sourceFormID string, fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	if triggerType != "record_updated" {
		return "watch_fields is for a record_updated rule (trigger_type record_updated)"
	}
	if sourceFormID == "" {
		return ""
	}
	form, err := s.GetForm(ctx, sourceFormID)
	if err != nil {
		return ""
	}
	known := map[string]bool{"status": true}
	names := []string{"status"}
	for _, f := range form.Fields {
		if !known[strings.ToLower(f.Name)] {
			names = append(names, f.Name)
		}
		known[strings.ToLower(f.Name)] = true
	}
	sort.Strings(names)
	for _, f := range fields {
		if !known[strings.ToLower(strings.TrimSpace(f))] {
			return fmt.Sprintf("watch_fields: %q is no field of %s (its fields: %s)", f, form.Label, strings.Join(names, ", "))
		}
	}
	return ""
}
