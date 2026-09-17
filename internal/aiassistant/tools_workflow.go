package aiassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// Read tools for workflows and forms (programme item 5). list_workflows and
// list_forms report counts only, which was enough to discover an id and
// nothing else: update_workflow_def and update_form_def replace steps and
// fields wholesale, so a model that cannot read the current list cannot add
// one item to it. These give it what the developer console shows.

// ── get_workflow ──────────────────────────────────────────────────────────────

func (e *ToolExecutor) getWorkflow(ctx context.Context, raw json.RawMessage) (string, error) {
	var p struct {
		WorkflowDefID string `json:"workflow_def_id"`
	}
	_ = json.Unmarshal(raw, &p)
	id, err := resolveWorkflowDefRef(ctx, e.pool, e.modelID, e.revID, p.WorkflowDefID)
	if err != nil {
		return "", err
	}
	def, err := workflow.NewStore(e.pool).GetWorkflowDefFull(ctx, id)
	if err != nil {
		return "", fmt.Errorf("load workflow: %w", err)
	}
	out := map[string]any{
		"id": def.ID, "name": def.Name, "description": def.Description, "status": def.Status,
		"trigger_event": def.TriggerEvent, "subject_type": def.SubjectType,
		"single_active_instance": def.SingleActiveInstance,
		"subject_config":         rawOrNull(def.SubjectConfig),
		"context_schema":         rawOrNull(def.ContextSchema),
		"steps":                  rawOrNull(def.Steps),
	}
	rules, _ := e.rulesForWorkflow(ctx, def.ID, def.Name)
	out["automation_rules"] = rules
	b, _ := json.MarshalIndent(out, "", "  ")
	var sb strings.Builder
	fmt.Fprintf(&sb, "Workflow '%s' (id:%s, status:%s):\n%s\n", def.Name, def.ID, def.Status, b)
	if errs := workflow.ValidateDef(def); len(errs) > 0 {
		fmt.Fprintf(&sb, "Validation: %d issue(s) — %s\n", len(errs), strings.Join(errs, "; "))
	} else {
		sb.WriteString("Validation: OK\n")
	}
	if len(rules) == 0 {
		sb.WriteString("No automation rule starts this workflow yet — it cannot fire until one exists (create_automation_rule).\n")
	}
	return sb.String(), nil
}

func rawOrNull(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(r, &v); err != nil {
		return string(r)
	}
	return v
}

// rulesForWorkflow lists the rules bound to a definition by id or, for rules
// that predate id binding or were created before publish, by name.
func (e *ToolExecutor) rulesForWorkflow(ctx context.Context, defID, name string) ([]map[string]any, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT r.id::text, r.name, r.trigger_type::text, r.enabled
		FROM workflow.automation_rule r
		JOIN core.model m ON m.application_id = r.application_id
		WHERE m.id = $1::uuid
		  AND (r.revision_id IS NULL OR r.revision_id::text = $2)
		  AND (r.workflow_def_id::text = $3 OR (r.workflow_def_id IS NULL AND lower(r.workflow_name) = lower($4)))
		ORDER BY r.name
	`, e.modelID, e.revID, defID, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, rname, trigger string
		var enabled bool
		if rows.Scan(&id, &rname, &trigger, &enabled) != nil {
			continue
		}
		out = append(out, map[string]any{"id": id, "name": rname, "trigger_type": trigger, "enabled": enabled})
	}
	return out, rows.Err()
}

// ── get_form ──────────────────────────────────────────────────────────────────

func (e *ToolExecutor) getForm(ctx context.Context, raw json.RawMessage) (string, error) {
	var p struct {
		FormID string `json:"form_id"`
	}
	_ = json.Unmarshal(raw, &p)
	id, err := resolveFormDefRef(ctx, e.pool, e.modelID, e.revID, p.FormID)
	if err != nil {
		return "", err
	}
	var name, label string
	var fields json.RawMessage
	if err := e.pool.QueryRow(ctx, `SELECT name, label, fields FROM model.form_def WHERE id=$1::uuid`, id).Scan(&name, &label, &fields); err != nil {
		return "", fmt.Errorf("load form: %w", err)
	}
	out := map[string]any{"id": id, "name": name, "label": label, "fields": rawOrNull(fields)}
	b, _ := json.MarshalIndent(out, "", "  ")
	var sb strings.Builder
	fmt.Fprintf(&sb, "Form '%s' (id:%s):\n%s\n", name, id, b)
	integrations, _ := e.integrationsForForm(ctx, id)
	if len(integrations) == 0 {
		sb.WriteString("No form integration posts this form's values into a metric yet (create_form_integration).\n")
	} else {
		sb.WriteString("Integrations posting from this form:\n")
		for _, line := range integrations {
			sb.WriteString("  " + line + "\n")
		}
	}
	var records int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, id).Scan(&records)
	fmt.Fprintf(&sb, "%d saved record(s).\n", records)
	return sb.String(), nil
}

func (e *ToolExecutor) integrationsForForm(ctx context.Context, formID string) ([]string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT fm.id::text, fm.name, fm.source_field, md.name, fm.aggregation, fm.posting_statuses, fm.live_posting
		FROM model.form_metric_mapping fm
		JOIN model.metric_def md ON md.id = fm.target_metric_id
		WHERE fm.form_id = $1::uuid ORDER BY fm.created_at
	`, formID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, name, field, metric, agg string
		var statuses []string
		var live bool
		if rows.Scan(&id, &name, &field, &metric, &agg, &statuses, &live) != nil {
			continue
		}
		out = append(out, fmt.Sprintf("%s (id:%s): field '%s' → metric '%s' (%s, statuses %s, live_posting:%v)", name, id, field, metric, agg, strings.Join(statuses, "/"), live))
	}
	return out, rows.Err()
}

// ── list_workflow_roles ───────────────────────────────────────────────────────

// The two vocabularies a step's assignee_roles and a notification's
// recipient_role accept: the platform roles every deployment has, and the
// business roles of this application's customer (the console's own picker
// lists the same set through /api/developer/workflow-roles).
func (e *ToolExecutor) listWorkflowRoles(ctx context.Context) (string, error) {
	var sb strings.Builder
	sb.WriteString("Platform roles (use the code): business_user, business_admin, developer, platform_admin\n")
	rows, err := e.pool.Query(ctx, `
		SELECT br.id::text, br.name, count(brm.user_id)
		FROM identity.business_role br
		JOIN core.workspace w ON w.id = br.workspace_id
		LEFT JOIN identity.business_role_member brm ON brm.role_id = br.id
		WHERE w.customer_id = (
		    SELECT COALESCE(a.customer_id, ws.customer_id)
		    FROM core.model mo
		    JOIN core.application a ON a.id = mo.application_id
		    LEFT JOIN core.workspace ws ON ws.id = a.workspace_id
		    WHERE mo.id = $1::uuid
		)
		GROUP BY br.id, br.name ORDER BY br.name
	`, e.modelID)
	if err != nil {
		return "", fmt.Errorf("list business roles: %w", err)
	}
	defer rows.Close()
	n := 0
	sb.WriteString("Business roles (use the NAME exactly as written):\n")
	for rows.Next() {
		var id, name string
		var members int
		if rows.Scan(&id, &name, &members) != nil {
			continue
		}
		n++
		fmt.Fprintf(&sb, "  %s (id:%s, %d member(s))\n", name, id, members)
	}
	if n == 0 {
		sb.WriteString("  none yet — create_business_role adds one; a business admin then adds its members\n")
	}
	return sb.String(), rows.Err()
}

// ── list_automation_rules ─────────────────────────────────────────────────────

func (e *ToolExecutor) listAutomationRules(ctx context.Context) (string, error) {
	var appID string
	if err := e.pool.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, e.modelID).Scan(&appID); err != nil {
		return "", fmt.Errorf("resolve application: %w", err)
	}
	rules, err := workflow.NewStore(e.pool).ListAutomationRules(ctx, appID, e.revID)
	if err != nil {
		return "", fmt.Errorf("list automation rules: %w", err)
	}
	if len(rules) == 0 {
		return "No automation rules yet — no workflow in this application can fire until one exists.", nil
	}
	var sb strings.Builder
	sb.WriteString("Automation rules:\n")
	for _, r := range rules {
		state := "enabled"
		if !r.Enabled {
			state = "disabled"
		}
		bound := "by name"
		if r.WorkflowDefID != "" {
			bound = "by id"
		}
		fmt.Fprintf(&sb, "  %s (id:%s)  trigger:%s → workflow '%s' (%s), %s", r.Name, r.ID, r.TriggerType, r.WorkflowName, bound, state)
		if r.SourceFormID != "" {
			fmt.Fprintf(&sb, ", source_form_id:%s", r.SourceFormID)
		}
		if r.SourceGridID != "" {
			fmt.Fprintf(&sb, ", source_grid_id:%s", r.SourceGridID)
		}
		if r.SourceIntegrationID != "" {
			fmt.Fprintf(&sb, ", source_integration_id:%s", r.SourceIntegrationID)
		}
		if r.CronExpr != "" {
			fmt.Fprintf(&sb, ", cron:%q %s", r.CronExpr, r.Timezone)
		}
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// ── list_form_integrations ────────────────────────────────────────────────────

func (e *ToolExecutor) listFormIntegrations(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT fm.id::text, fm.name, fd.name, fm.source_field, md.name, fm.aggregation, fm.posting_statuses, fm.dimension_mappings, fm.live_posting
		FROM model.form_metric_mapping fm
		JOIN model.form_def fd ON fd.id = fm.form_id
		JOIN model.metric_def md ON md.id = fm.target_metric_id
		WHERE fm.model_id = $1::uuid AND (fm.revision_id IS NULL OR fm.revision_id::text = $2)
		ORDER BY fd.name, fm.created_at
	`, e.modelID, e.revID)
	if err != nil {
		return "", fmt.Errorf("list form integrations: %w", err)
	}
	defer rows.Close()
	var sb strings.Builder
	sb.WriteString("Form integrations (a form's field posting into a metric):\n")
	n := 0
	for rows.Next() {
		var id, name, form, field, metric, agg string
		var statuses []string
		var dimRaw []byte
		var live bool
		if rows.Scan(&id, &name, &form, &field, &metric, &agg, &statuses, &dimRaw, &live) != nil {
			continue
		}
		n++
		dims := map[string]string{}
		_ = json.Unmarshal(dimRaw, &dims)
		var mapped []string
		for dimID, fieldName := range dims {
			var dimName string
			_ = e.pool.QueryRow(ctx, `SELECT name FROM model.dimension_def WHERE id=$1::uuid`, dimID).Scan(&dimName)
			if dimName == "" {
				dimName = dimID
			}
			mapped = append(mapped, dimName+"←"+fieldName)
		}
		fmt.Fprintf(&sb, "  %s (id:%s)  form '%s' field '%s' → metric '%s' (%s, statuses %s, live_posting:%v, dimensions: %s)\n",
			name, id, form, field, metric, agg, strings.Join(statuses, "/"), live, strings.Join(mapped, ", "))
	}
	if n == 0 {
		return "No form integrations yet — a form's submitted values do not reach any metric until one exists (create_form_integration).", nil
	}
	return sb.String(), rows.Err()
}

// ── validate_workflow ─────────────────────────────────────────────────────────

// The developer's Validate button, for the model: either a stored definition
// (workflow_def_id, with optional overrides) or a definition that exists only
// in the conversation so far (name + steps + context_schema). Same validator
// the Publish gate runs, so "Valid" here means a developer could publish it.
func (e *ToolExecutor) validateWorkflow(ctx context.Context, raw json.RawMessage) (string, error) {
	var p struct {
		WorkflowDefID string          `json:"workflow_def_id"`
		Name          string          `json:"name"`
		Steps         json.RawMessage `json:"steps"`
		ContextSchema json.RawMessage `json:"context_schema"`
	}
	_ = json.Unmarshal(raw, &p)
	def := &workflow.WorkflowDefFull{Name: p.Name}
	if p.WorkflowDefID != "" {
		id, err := resolveWorkflowDefRef(ctx, e.pool, e.modelID, e.revID, p.WorkflowDefID)
		if err != nil {
			return "", err
		}
		stored, err := workflow.NewStore(e.pool).GetWorkflowDefFull(ctx, id)
		if err != nil {
			return "", fmt.Errorf("load workflow: %w", err)
		}
		def = stored
		if p.Name != "" {
			def.Name = p.Name
		}
	} else if def.Name == "" {
		def.Name = "(unnamed)"
	}
	if len(p.Steps) > 0 {
		def.Steps = p.Steps
	}
	if len(p.ContextSchema) > 0 {
		def.ContextSchema = p.ContextSchema
	}
	errs := workflow.ValidateDef(def)
	if len(errs) == 0 {
		return fmt.Sprintf("Workflow '%s' is valid — a developer could publish it as is.", def.Name), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Workflow '%s' has %d issue(s) a developer would have to fix before publishing:\n", def.Name, len(errs))
	for _, m := range errs {
		sb.WriteString("  - " + m + "\n")
	}
	return sb.String(), nil
}
