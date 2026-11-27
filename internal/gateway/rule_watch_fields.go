package gateway

import (
	"context"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
)

// watchFieldsRefusal is crudapp.WatchFieldsRefusal on the routed database.
func (h *handler) watchFieldsRefusal(ctx context.Context, triggerType, sourceFormID string, fields []string) string {
	return crudapp.NewStore(h.db.For(ctx)).WatchFieldsRefusal(ctx, triggerType, sourceFormID, fields)
}

// ruleTriggerAndForm is a rule's trigger type and table as stored.
func (h *handler) ruleTriggerAndForm(ctx context.Context, ruleID string) (string, string) {
	var trigger, form string
	_ = h.db.QueryRow(ctx, `SELECT trigger_type::text, COALESCE(source_form_id::text,'') FROM workflow.automation_rule WHERE id = $1::uuid`, ruleID).Scan(&trigger, &form)
	return trigger, form
}
