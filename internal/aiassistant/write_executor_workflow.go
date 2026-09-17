package aiassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// The write tools that bring the AI Developer level with the developer role
// for workflows and forms (programme item 5, 2026-09-16). Before these, the
// assistant could author a workflow definition but never wire it to fire
// (no automation rule), never assign a step to a role that did not exist
// yet, never delete what it had built, and never make a form's numbers
// reach the model (no form integration). Each tool mirrors the developer
// console's HTTP handler for the same action, minus what stays human-only:
// publishing a workflow, and firing one.

// applicationID resolves the executor's model to its application, which is
// what workflow definitions, rules and business roles are scoped by.
func (e *WriteExecutor) applicationID(ctx context.Context) (string, error) {
	var appID string
	if err := e.pool.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, e.modelID).Scan(&appID); err != nil {
		return "", fmt.Errorf("resolve application: %w", err)
	}
	return appID, nil
}

// workspaceID is the workspace a business role for this application belongs
// in, resolved the way the console's baWorkspaceModel does: the app's own
// workspace when it has one; otherwise (apps created since migration 030
// belong to a customer directly) a workspace of that customer — one the
// acting developer holds a role in, else the customer's oldest.
func (e *WriteExecutor) workspaceID(ctx context.Context) (string, error) {
	var appWS *string
	var customerID string
	if err := e.pool.QueryRow(ctx, `
		SELECT app.workspace_id::text, COALESCE(app.customer_id::text, ws.customer_id::text)
		FROM core.model m
		JOIN core.application app ON app.id = m.application_id
		LEFT JOIN core.workspace ws ON ws.id = app.workspace_id
		WHERE m.id = $1::uuid
	`, e.modelID).Scan(&appWS, &customerID); err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	if appWS != nil && *appWS != "" {
		return *appWS, nil
	}
	var wsID string
	if e.userID != "" {
		if err := e.pool.QueryRow(ctx, `
			SELECT ws.id::text FROM core.workspace ws
			JOIN identity.role_assignment ra ON ra.workspace_id = ws.id
			WHERE ws.customer_id = $1::uuid AND ra.user_id = $2::uuid
			ORDER BY ws.created_at LIMIT 1
		`, customerID, e.userID).Scan(&wsID); err == nil {
			return wsID, nil
		}
	}
	if err := e.pool.QueryRow(ctx, `
		SELECT id::text FROM core.workspace WHERE customer_id = $1::uuid ORDER BY created_at LIMIT 1
	`, customerID).Scan(&wsID); err != nil {
		return "", fmt.Errorf("no workspace found for this application's customer")
	}
	return wsID, nil
}

// validationNote renders workflow.ValidateDef's verdict for a tool result.
// The write is not refused on problems — the developer console saves an
// incomplete draft too and only Publish refuses — but the model is told, the
// same way the editor's Validate button tells a person.
func validationNote(def *workflow.WorkflowDefFull) string {
	errs := workflow.ValidateDef(def)
	if len(errs) == 0 {
		return "validation: OK — a developer can publish it"
	}
	return fmt.Sprintf("validation: %d issue(s) a developer would have to fix before publishing — %s", len(errs), strings.Join(errs, "; "))
}

// ── delete_workflow_def ───────────────────────────────────────────────────────

func (e *WriteExecutor) deleteWorkflowDef(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		WorkflowDefID string `json:"workflow_def_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.WorkflowDefID == "" {
		return "", "", fmt.Errorf("workflow_def_id is required")
	}
	id, err := resolveWorkflowDefRef(ctx, e.pool, e.modelID, e.revID, p.WorkflowDefID)
	if err != nil {
		return "", "", err
	}
	var name, status string
	_ = e.pool.QueryRow(ctx, `SELECT name, status FROM workflow.workflow_def WHERE id=$1::uuid`, id).Scan(&name, &status)
	// Same rule as the developer's DELETE: only a draft with no instances
	// goes; anything that has run is archived, never deleted.
	if err := workflow.NewStore(e.pool).DeleteWorkflowDef(ctx, id); err != nil {
		if status != "" && status != "draft" {
			return "", "", fmt.Errorf("workflow '%s' is %s — only a draft can be deleted; a developer archives a published workflow from the Workflows tab", name, status)
		}
		return "", "", fmt.Errorf("delete workflow: %w", err)
	}
	return fmt.Sprintf("Workflow '%s' deleted", name), "", nil
}

// ── delete_form_def ───────────────────────────────────────────────────────────

func (e *WriteExecutor) deleteFormDef(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		FormID string `json:"form_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.FormID == "" {
		return "", "", fmt.Errorf("form_id is required")
	}
	id, err := resolveFormDefRef(ctx, e.pool, e.modelID, e.revID, p.FormID)
	if err != nil {
		return "", "", err
	}
	var name string
	var records, integrations int
	_ = e.pool.QueryRow(ctx, `SELECT name FROM model.form_def WHERE id=$1::uuid`, id).Scan(&name)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, id).Scan(&records)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM model.form_metric_mapping WHERE form_id=$1::uuid`, id).Scan(&integrations)
	if err := crudapp.NewStore(e.pool).DeleteForm(ctx, id); err != nil {
		return "", "", fmt.Errorf("delete form: %w", err)
	}
	note := ""
	if records > 0 || integrations > 0 {
		note = fmt.Sprintf(" (with %d saved record(s) and %d integration(s))", records, integrations)
	}
	return fmt.Sprintf("Form '%s' deleted%s", name, note), "", nil
}

// ── automation rules ──────────────────────────────────────────────────────────

type automationRuleParams struct {
	AutomationRuleID    string `json:"automation_rule_id"`
	Name                string `json:"name"`
	Description         string `json:"description"`
	TriggerType         string `json:"trigger_type"`
	WorkflowDefID       string `json:"workflow_def_id"`
	WorkflowName        string `json:"workflow_name"`
	SourceFormID        string `json:"source_form_id"`
	SourceGridID        string `json:"source_grid_id"`
	SourceIntegrationID string `json:"source_integration_id"`
	Enabled             *bool  `json:"enabled"`
	CronExpr            string `json:"cron_expr"`
	Timezone            string `json:"timezone"`
	MisfirePolicy       string `json:"misfire_policy"`
	MaxRetries          int    `json:"max_retries"`
	RetryBackoffSeconds int    `json:"retry_backoff_seconds"`
}

// ruleBinding resolves the workflow a rule fires. The rule is stored bound
// by NAME unless the definition is already published: the store refuses an
// id binding to an unpublished definition, and the assistant cannot publish
// (that stays a developer's click). Binding by name is what the engine's own
// fireRule falls back to, scoped to the rule's revision, so the rule becomes
// live the moment the developer publishes — no second proposal needed.
func (e *WriteExecutor) ruleBinding(ctx context.Context, p automationRuleParams) (workflowName, workflowDefID, note string, err error) {
	ref := p.WorkflowDefID
	if ref == "" {
		ref = p.WorkflowName
	}
	if ref == "" {
		return "", "", "", fmt.Errorf("workflow_def_id or workflow_name is required — the rule must name the workflow it starts")
	}
	id, err := resolveWorkflowDefRef(ctx, e.pool, e.modelID, e.revID, ref)
	if err != nil {
		return "", "", "", err
	}
	var name, status string
	if err := e.pool.QueryRow(ctx, `SELECT name, status FROM workflow.workflow_def WHERE id=$1::uuid`, id).Scan(&name, &status); err != nil {
		return "", "", "", fmt.Errorf("workflow %s not found", ref)
	}
	if status == "published" {
		return name, id, "", nil
	}
	return name, "", fmt.Sprintf("bound to workflow '%s' by name; it fires once a developer publishes that workflow", name), nil
}

// ruleSources resolves and checks the source the trigger needs. An event
// rule with no source matches every form/grid/integration of the
// application — allowed, as it is in the console — but a source that is
// named must exist in this model.
func (e *WriteExecutor) ruleSources(ctx context.Context, p *automationRuleParams) error {
	if p.SourceFormID != "" {
		id, err := resolveFormDefRef(ctx, e.pool, e.modelID, e.revID, p.SourceFormID)
		if err != nil {
			return err
		}
		p.SourceFormID = id
	}
	if p.SourceGridID != "" {
		id, err := e.requireInModel(ctx, "grid", p.SourceGridID)
		if err != nil {
			return err
		}
		p.SourceGridID = id
	}
	if p.SourceIntegrationID != "" {
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM model.integration WHERE id=$1::uuid AND model_id=$2::uuid`, p.SourceIntegrationID, e.modelID).Scan(&n); err != nil || n == 0 {
			return fmt.Errorf("integration %s not found in this model", p.SourceIntegrationID)
		}
	}
	switch p.TriggerType {
	case "schedule":
		if p.CronExpr == "" {
			return fmt.Errorf("a schedule rule needs cron_expr (5-field cron, e.g. \"0 9 * * 1\" for Mondays at 09:00)")
		}
	}
	return nil
}

func schedule(p automationRuleParams) *workflow.ScheduleConfig {
	if p.TriggerType != "schedule" && p.CronExpr == "" {
		return nil
	}
	return &workflow.ScheduleConfig{
		CronExpr: p.CronExpr, Timezone: p.Timezone, MisfirePolicy: p.MisfirePolicy,
		MaxRetries: p.MaxRetries, RetryBackoffSeconds: p.RetryBackoffSeconds,
	}
}

func (e *WriteExecutor) createAutomationRule(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p automationRuleParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	if p.TriggerType == "" {
		p.TriggerType = "manual"
	}
	if !validTriggerTypes[p.TriggerType] {
		return "", "", fmt.Errorf("unknown trigger_type %q — use one of %s", p.TriggerType, triggerTypeList)
	}
	appID, err := e.applicationID(ctx)
	if err != nil {
		return "", "", err
	}
	workflowName, workflowDefID, note, err := e.ruleBinding(ctx, p)
	if err != nil {
		return "", "", err
	}
	if err := e.ruleSources(ctx, &p); err != nil {
		return "", "", err
	}
	rule, err := workflow.NewStore(e.pool).CreateAutomationRuleScoped(ctx, appID, e.revID, p.Name, p.Description, p.TriggerType,
		workflowName, workflowDefID, p.SourceFormID, p.SourceGridID, p.SourceIntegrationID, schedule(p))
	if err != nil {
		return "", "", fmt.Errorf("create automation rule: %w", err)
	}
	if p.Enabled != nil && !*p.Enabled {
		if _, err := workflow.NewStore(e.pool).UpdateAutomationRuleScoped(ctx, rule.ID, "", "", "", "", "", "", "", "", p.Enabled, nil); err != nil {
			return "", "", fmt.Errorf("disable automation rule: %w", err)
		}
	}
	msg := fmt.Sprintf("Automation rule '%s' created (id: %s, trigger: %s, starts workflow '%s')", p.Name, rule.ID, p.TriggerType, workflowName)
	if note != "" {
		msg += " — " + note
	}
	return msg, rule.ID, nil
}

func (e *WriteExecutor) updateAutomationRule(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p automationRuleParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.AutomationRuleID == "" {
		return "", "", fmt.Errorf("automation_rule_id is required — call list_automation_rules for the id")
	}
	appID, err := e.applicationID(ctx)
	if err != nil {
		return "", "", err
	}
	var ruleAppID, ruleRevID, curTrigger string
	if err := e.pool.QueryRow(ctx, `
		SELECT application_id::text, COALESCE(revision_id::text,''), trigger_type::text
		FROM workflow.automation_rule WHERE id=$1::uuid
	`, p.AutomationRuleID).Scan(&ruleAppID, &ruleRevID, &curTrigger); err != nil {
		return "", "", fmt.Errorf("automation rule %s not found", p.AutomationRuleID)
	}
	if ruleAppID != appID {
		return "", "", fmt.Errorf("automation rule %s belongs to another application", p.AutomationRuleID)
	}
	if e.revID != "" && ruleRevID != "" && ruleRevID != e.revID {
		return "", "", fmt.Errorf("automation rule %s is not in the current working revision — call list_automation_rules for the copy in scope", p.AutomationRuleID)
	}
	if p.TriggerType != "" && !validTriggerTypes[p.TriggerType] {
		return "", "", fmt.Errorf("unknown trigger_type %q — use one of %s", p.TriggerType, triggerTypeList)
	}
	if p.TriggerType == "" {
		p.TriggerType = curTrigger
	}
	var workflowName, workflowDefID, note string
	if p.WorkflowDefID != "" || p.WorkflowName != "" {
		if workflowName, workflowDefID, note, err = e.ruleBinding(ctx, p); err != nil {
			return "", "", err
		}
	}
	if err := e.ruleSources(ctx, &p); err != nil {
		return "", "", err
	}
	// The store treats an empty trigger_type as "unchanged", so only pass it
	// when the caller actually asked for a change.
	trigger := p.TriggerType
	if trigger == curTrigger {
		trigger = ""
	}
	rule, err := workflow.NewStore(e.pool).UpdateAutomationRuleScoped(ctx, p.AutomationRuleID, p.Name, p.Description, trigger,
		workflowName, workflowDefID, p.SourceFormID, p.SourceGridID, p.SourceIntegrationID, p.Enabled, schedule(p))
	if err != nil {
		return "", "", fmt.Errorf("update automation rule: %w", err)
	}
	state := "enabled"
	if !rule.Enabled {
		state = "disabled"
	}
	msg := fmt.Sprintf("Automation rule '%s' updated (trigger: %s, starts workflow '%s', %s)", rule.Name, rule.TriggerType, rule.WorkflowName, state)
	if note != "" {
		msg += " — " + note
	}
	return msg, "", nil
}

func (e *WriteExecutor) deleteAutomationRule(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		AutomationRuleID string `json:"automation_rule_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.AutomationRuleID == "" {
		return "", "", fmt.Errorf("automation_rule_id is required")
	}
	appID, err := e.applicationID(ctx)
	if err != nil {
		return "", "", err
	}
	var ruleAppID, name string
	if err := e.pool.QueryRow(ctx, `SELECT application_id::text, name FROM workflow.automation_rule WHERE id=$1::uuid`, p.AutomationRuleID).Scan(&ruleAppID, &name); err != nil {
		return "", "", fmt.Errorf("automation rule %s not found", p.AutomationRuleID)
	}
	if ruleAppID != appID {
		return "", "", fmt.Errorf("automation rule %s belongs to another application", p.AutomationRuleID)
	}
	if err := workflow.NewStore(e.pool).DeleteAutomationRule(ctx, p.AutomationRuleID); err != nil {
		return "", "", fmt.Errorf("delete automation rule: %w", err)
	}
	return fmt.Sprintf("Automation rule '%s' deleted", name), "", nil
}

// ── create_business_role ──────────────────────────────────────────────────────

// A business role is what a workflow step is assigned to ("Finance Review")
// and what a notification step can address. The developer creates them from
// the Roles screen (the baOrDev guard); the assistant mirrors that. Members
// are a business admin's decision and stay out of reach here.
func (e *WriteExecutor) createBusinessRole(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || strings.TrimSpace(p.Name) == "" {
		return "", "", fmt.Errorf("name is required")
	}
	name := strings.TrimSpace(p.Name)
	wsID, err := e.workspaceID(ctx)
	if err != nil {
		return "", "", err
	}
	var existing string
	if err := e.pool.QueryRow(ctx, `
		SELECT id::text FROM identity.business_role WHERE workspace_id=$1::uuid AND lower(name)=lower($2)
	`, wsID, name).Scan(&existing); err == nil {
		return fmt.Sprintf("Business role '%s' already exists (id: %s) — nothing to create", name, existing), existing, nil
	}
	var id string
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, $2) RETURNING id::text`,
		wsID, name).Scan(&id); err != nil {
		return "", "", fmt.Errorf("create business role: %w", err)
	}
	return fmt.Sprintf("Business role '%s' created (id: %s, no members yet — a business admin adds people to it). Use the NAME '%s' in assignee_roles or as a notification recipient_role.", name, id, name), id, nil
}

// ── form integrations ─────────────────────────────────────────────────────────

type formIntegrationParams struct {
	FormIntegrationID string            `json:"form_integration_id"`
	FormID            string            `json:"form_id"`
	GridID            string            `json:"grid_id"`
	Name              string            `json:"name"`
	SourceField       string            `json:"source_field"`
	TargetMetricID    string            `json:"target_metric_id"`
	Aggregation       string            `json:"aggregation"`
	PostingStatuses   []string          `json:"posting_statuses"`
	DimensionMappings map[string]string `json:"dimension_mappings"`
	LivePosting       *bool             `json:"live_posting"`
}

// checkIntegrationFields confirms the fields the mapping reads actually exist
// on the form, and resolves every dimension in dimension_mappings into the
// working revision. The console's pickers make these mistakes impossible;
// a model typing names can make all of them.
func (e *WriteExecutor) checkIntegrationFields(ctx context.Context, formID string, p *formIntegrationParams) error {
	form, err := crudapp.NewStore(e.pool).GetForm(ctx, formID)
	if err != nil {
		return fmt.Errorf("load form: %w", err)
	}
	fields := map[string]bool{}
	for _, f := range form.Fields {
		fields[f.Name] = true
	}
	if p.SourceField != "" && !fields[p.SourceField] {
		return fmt.Errorf("form '%s' has no field %q — source_field must be one of its field names", form.Name, p.SourceField)
	}
	resolved := map[string]string{}
	for dimRef, fieldName := range p.DimensionMappings {
		dimID, err := e.requireInModel(ctx, "dimension", dimRef)
		if err != nil {
			return fmt.Errorf("dimension_mappings key %q: %w", dimRef, err)
		}
		if !fields[fieldName] {
			return fmt.Errorf("dimension_mappings maps dimension %q to field %q, which form '%s' does not have", dimRef, fieldName, form.Name)
		}
		resolved[dimID] = fieldName
	}
	p.DimensionMappings = resolved
	return nil
}

func (e *WriteExecutor) createFormIntegration(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p formIntegrationParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.FormID == "" || p.Name == "" || p.SourceField == "" || p.TargetMetricID == "" {
		return "", "", fmt.Errorf("form_id, name, source_field and target_metric_id are required")
	}
	formID, err := resolveFormDefRef(ctx, e.pool, e.modelID, e.revID, p.FormID)
	if err != nil {
		return "", "", err
	}
	metricID, err := e.requireInModel(ctx, "metric", p.TargetMetricID)
	if err != nil {
		return "", "", err
	}
	if p.GridID != "" {
		if p.GridID, err = e.requireInModel(ctx, "grid", p.GridID); err != nil {
			return "", "", err
		}
	}
	if p.DimensionMappings == nil {
		p.DimensionMappings = map[string]string{}
	}
	if err := e.checkIntegrationFields(ctx, formID, &p); err != nil {
		return "", "", err
	}
	if p.Aggregation == "" {
		p.Aggregation = "sum"
	}
	if len(p.PostingStatuses) == 0 {
		p.PostingStatuses = []string{"approved"}
	}
	live := true
	if p.LivePosting != nil {
		live = *p.LivePosting
	}
	dimJSON, _ := json.Marshal(p.DimensionMappings)
	var id string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO model.form_metric_mapping
		  (model_id, form_id, grid_id, name, source_field, target_metric_id, aggregation,
		   posting_statuses, dimension_mappings, live_posting, revision_id)
		VALUES ($1::uuid, $2::uuid, NULLIF($3,'')::uuid, $4, $5, $6::uuid, $7, $8, $9, $10, NULLIF($11,'')::uuid)
		RETURNING id::text`,
		e.modelID, formID, p.GridID, p.Name, p.SourceField, metricID, p.Aggregation,
		p.PostingStatuses, dimJSON, live, e.revID,
	).Scan(&id); err != nil {
		return "", "", fmt.Errorf("create form integration: %w", err)
	}
	var metricName string
	_ = e.pool.QueryRow(ctx, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, metricID).Scan(&metricName)
	return fmt.Sprintf("Form integration '%s' created (id: %s): field '%s' posts to metric '%s' (%s) for records with status %s, %d dimension mapping(s)",
		p.Name, id, p.SourceField, metricName, p.Aggregation, strings.Join(p.PostingStatuses, "/"), len(p.DimensionMappings)), id, nil
}

func (e *WriteExecutor) loadIntegration(ctx context.Context, id string) (formID, name string, err error) {
	var modelID, revID string
	if err := e.pool.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text,''), form_id::text, name
		FROM model.form_metric_mapping WHERE id=$1::uuid
	`, id).Scan(&modelID, &revID, &formID, &name); err != nil {
		return "", "", fmt.Errorf("form integration %s not found — call list_form_integrations for the id", id)
	}
	if modelID != e.modelID {
		return "", "", fmt.Errorf("form integration %s belongs to another model", id)
	}
	if e.revID != "" && revID != "" && revID != e.revID {
		return "", "", fmt.Errorf("form integration %s is not in the current working revision — call list_form_integrations for the copy in scope", id)
	}
	return formID, name, nil
}

func (e *WriteExecutor) updateFormIntegration(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p formIntegrationParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.FormIntegrationID == "" {
		return "", "", fmt.Errorf("form_integration_id is required")
	}
	formID, curName, err := e.loadIntegration(ctx, p.FormIntegrationID)
	if err != nil {
		return "", "", err
	}
	// Partial update: the console's PATCH overwrites every column, so read
	// the row first and only replace what the caller supplied.
	var cur formIntegrationParams
	var dimRaw []byte
	var live bool
	if err := e.pool.QueryRow(ctx, `
		SELECT COALESCE(grid_id::text,''), name, source_field, target_metric_id::text, aggregation, posting_statuses, dimension_mappings, live_posting
		FROM model.form_metric_mapping WHERE id=$1::uuid
	`, p.FormIntegrationID).Scan(&cur.GridID, &cur.Name, &cur.SourceField, &cur.TargetMetricID, &cur.Aggregation, &cur.PostingStatuses, &dimRaw, &live); err != nil {
		return "", "", fmt.Errorf("load form integration: %w", err)
	}
	_ = json.Unmarshal(dimRaw, &cur.DimensionMappings)
	if p.Name == "" {
		p.Name = cur.Name
	}
	if p.SourceField == "" {
		p.SourceField = cur.SourceField
	}
	if p.TargetMetricID == "" {
		p.TargetMetricID = cur.TargetMetricID
	} else if p.TargetMetricID, err = e.requireInModel(ctx, "metric", p.TargetMetricID); err != nil {
		return "", "", err
	}
	if p.GridID == "" {
		p.GridID = cur.GridID
	} else if p.GridID, err = e.requireInModel(ctx, "grid", p.GridID); err != nil {
		return "", "", err
	}
	if p.Aggregation == "" {
		p.Aggregation = cur.Aggregation
	}
	if len(p.PostingStatuses) == 0 {
		p.PostingStatuses = cur.PostingStatuses
	}
	if p.DimensionMappings == nil {
		p.DimensionMappings = cur.DimensionMappings
	}
	if p.LivePosting == nil {
		p.LivePosting = &live
	}
	if err := e.checkIntegrationFields(ctx, formID, &p); err != nil {
		return "", "", err
	}
	dimJSON, _ := json.Marshal(p.DimensionMappings)
	if _, err := e.pool.Exec(ctx, `
		UPDATE model.form_metric_mapping SET
		  grid_id=NULLIF($2,'')::uuid, name=$3, source_field=$4, target_metric_id=$5::uuid, aggregation=$6,
		  posting_statuses=$7, dimension_mappings=$8, live_posting=$9
		WHERE id=$1::uuid`,
		p.FormIntegrationID, p.GridID, p.Name, p.SourceField, p.TargetMetricID, p.Aggregation,
		p.PostingStatuses, dimJSON, *p.LivePosting,
	); err != nil {
		return "", "", fmt.Errorf("update form integration: %w", err)
	}
	if p.Name != curName {
		return fmt.Sprintf("Form integration '%s' updated (renamed from '%s')", p.Name, curName), "", nil
	}
	return fmt.Sprintf("Form integration '%s' updated", p.Name), "", nil
}

func (e *WriteExecutor) deleteFormIntegration(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		FormIntegrationID string `json:"form_integration_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.FormIntegrationID == "" {
		return "", "", fmt.Errorf("form_integration_id is required")
	}
	_, name, err := e.loadIntegration(ctx, p.FormIntegrationID)
	if err != nil {
		return "", "", err
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM model.form_metric_mapping WHERE id=$1::uuid`, p.FormIntegrationID); err != nil {
		return "", "", fmt.Errorf("delete form integration: %w", err)
	}
	return fmt.Sprintf("Form integration '%s' deleted", name), "", nil
}
