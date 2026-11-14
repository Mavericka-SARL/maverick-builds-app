package gateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// A run a record starts takes its Dimension member context from the
// record's fields of the same name: a dimension field's code as it stands,
// and a date for a time dimension as the shortest period holding it. In the
// consolidation kit the intercompany dispute watch could only read the
// year's total, because the match's date ("2026-09-30") was refused as a
// period (2026-11-13).
func TestRecordStartedRunTakesItsPeriodFromTheRecordsDate(t *testing.T) {
	f := setupWFRuntimeAuditFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var v string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return v
	}
	model := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Group') RETURNING id::text`, f.appID)
	rev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Live') RETURNING id::text`, model)
	q(`UPDATE core.model SET active_revision_id = $2::uuid WHERE id = $1::uuid RETURNING id::text`, model, rev)
	period := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'period') RETURNING id::text`, model, rev)
	fy := q(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index) VALUES ($1::uuid, 'FY2026', 'FY 2026', '2026-01-01', '2026-12-31', 0) RETURNING id::text`, period)
	for _, m := range [][4]string{{"2026-09", "2026-09-01", "2026-09-30", "9"}, {"2026-10", "2026-10-01", "2026-10-31", "10"}} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, parent_member_id) VALUES ($1::uuid, $2, $2, $3::date, $4::date, $5::int, $6::uuid) RETURNING id::text`, period, m[0], m[1], m[2], m[3], fy)
	}
	entity := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'entity') RETURNING id::text`, model, rev)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'PROD', 'Production') RETURNING id::text`, entity)
	fields, _ := json.Marshal([]map[string]any{
		{"name": "entity", "label": "Entity", "type": "dimension", "dimension_id": entity},
		{"name": "period", "label": "Period", "type": "date"},
		{"name": "status", "label": "Status", "type": "status"},
	})
	form := q(`INSERT INTO model.form_def (model_id, revision_id, name, label, fields) VALUES ($1::uuid, $2::uuid, 'ic_match', 'Matches', $3::jsonb) RETURNING id::text`, model, rev, string(fields))
	record := q(`INSERT INTO runtime.form_record (form_id, data, status, created_by) VALUES ($1::uuid, '{"entity":"PROD","period":"2026-09-30"}', 'submitted', $2::uuid) RETURNING id::text`, form, f.devID)

	ws := workflow.NewStore(f.pool)
	def, err := ws.CreateWorkflowDefFull(ctx, f.appID, rev, "Dispute watch", "", "form_submit", f.devID)
	if err != nil {
		t.Fatal(err)
	}
	steps, _ := json.Marshal([]map[string]any{{"id": "r", "name": "Review", "type": "approval", "assignee_roles": []string{"business_admin"}}})
	schema, _ := json.Marshal([]map[string]any{
		{"key": "entity", "label": "Entity", "data_type": "Dimension member", "dimension_id": entity, "required": true},
		{"key": "period", "label": "Period", "data_type": "Dimension member", "dimension_id": period, "required": true},
	})
	if _, err := ws.UpdateWorkflowDefFull(ctx, def.ID, def.Name, def.Description, def.TriggerEvent, "none", f.devID, steps, schema, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.PublishWorkflowDef(ctx, def.ID, f.devID); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.CreateAutomationRule(ctx, f.appID, rev, "Watch", "", "form_submit", "", def.ID, form, "", nil); err != nil {
		t.Fatal(err)
	}

	ws.DispatchEventRules(ctx, f.appID, rev, "form_submit", form, f.devID,
		map[string]string{"form_id": form, "record_id": record, "status": "submitted"})
	var got map[string]any
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		var raw []byte
		if err := f.pool.QueryRow(ctx, `SELECT context FROM workflow.workflow_instance WHERE workflow_def_id = $1::uuid`, def.ID).Scan(&raw); err == nil {
			_ = json.Unmarshal(raw, &got)
			break
		}
	}
	if got == nil {
		t.Fatal("the submitted record started no run; its date was not taken as a period")
	}
	if got["period"] != "2026-09" || got["entity"] != "PROD" {
		t.Fatalf("run context period %v entity %v; want 2026-09 (the month holding 2026-09-30, not FY2026) and PROD", got["period"], got["entity"])
	}

	// A member code stays as it stands; a date no period holds is refused.
	res, err := ws.ResolveStartContext(ctx, def.ID, f.devID, map[string]string{"entity": "PROD", "period": "2026-10"})
	if err != nil || res["period"] != "2026-10" {
		t.Fatalf("a period code: %v %v; want 2026-10", res, err)
	}
	if _, err := ws.ResolveStartContext(ctx, def.ID, f.devID, map[string]string{"entity": "PROD", "period": "2027-03-31"}); err == nil {
		t.Fatal("a date no period holds started a run")
	}
}
