package aiassistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// ReadTools returns the read-only tool definitions (no confirmation required).
func ReadTools() []providers.ToolDef {
	noParams := json.RawMessage(`{"type":"object","properties":{},"required":[]}`)
	return []providers.ToolDef{
		{
			Name:        "get_model_summary",
			Description: "Returns a high-level summary of the current application: name, active revision, and counts of metrics, dimensions, grids, dashboards, and workflows.",
			Parameters:  noParams,
		},
		{
			Name:        "list_metrics",
			Description: "Returns every metric definition in the current revision: name, formula (if calculated), format, and dependencies.",
			Parameters:  noParams,
		},
		{
			Name:        "list_dimensions",
			Description: "Returns every dimension and its members: code, label, parent hierarchy, and each member's properties as {key=value} — use these to group or re-parent members by a property (e.g. category). A time dimension is marked [time · granularity] and lists its leaf periods in chronological order with their dates and its aggregate periods (H1, FY26) as such; only such a dimension supports time-series formulas (PREVIOUS, LAG, MOVINGSUM, CUMULATE, ...).",
			Parameters:  noParams,
		},
		{
			Name:        "list_grids",
			Description: "Returns all grid definitions with their configured metrics and dimensions.",
			Parameters:  noParams,
		},
		{
			Name:        "list_dashboards",
			Description: "Returns all dashboards with their widget list (type, position, referenced resource).",
			Parameters:  noParams,
		},
		{
			Name:        "list_revisions",
			Description: "Returns all revisions for the current model, indicating which is active.",
			Parameters:  noParams,
		},
		{
			Name:        "list_workflows",
			Description: "Returns every workflow definition for this application in the working revision: name, id, trigger event, status (draft/published/archived), and step count. Call get_workflow for a definition's steps.",
			Parameters:  noParams,
		},
		{
			Name:        "get_workflow",
			Description: "Returns one workflow definition in full — steps, context_schema, subject, single_active_instance, the automation rules that start it, and the Validate verdict. Call it before update_workflow_def on an existing workflow: steps are replaced as a whole list, so you must resupply the current ones plus your change.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"workflow_def_id":{"type":"string","description":"The workflow's id from list_workflows, or its exact name"}},"required":["workflow_def_id"]}`),
		},
		{
			Name:        "validate_workflow",
			Description: "The developer's Validate button: checks a workflow the way Publish will (ids, routes, roles, reachability, loops). Pass workflow_def_id for a stored one, or name + steps (+ context_schema) for steps you are about to propose. Use it before proposing update_workflow_def so the developer is not handed a workflow they cannot publish.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"workflow_def_id":{"type":"string"},"name":{"type":"string"},"steps":{"type":"array","items":{"type":"object"}},"context_schema":{"type":"array","items":{"type":"object"}}},"required":[]}`),
		},
		{
			Name:        "list_workflow_roles",
			Description: "Returns what a step's assignee_roles and a notification's recipient_role may contain: the platform role codes, and this application's business roles by name with their member counts. Call it before assigning any step; propose create_business_role when the role a developer names does not exist.",
			Parameters:  noParams,
		},
		{
			Name:        "list_automation_rules",
			Description: "Returns every automation rule in the working revision: id, name, trigger type, the workflow it starts, its source form/grid/integration, schedule and enabled state. A workflow only ever fires through one of these.",
			Parameters:  noParams,
		},
		{
			Name:        "list_forms",
			Description: "Returns every form definition in the working revision: name, id, label, and field count. Call get_form for a form's fields.",
			Parameters:  noParams,
		},
		{
			Name:        "get_form",
			Description: "Returns one form in full — its fields as JSON, the integrations posting from it, and its record count. Call it before update_form_def on an existing form: fields are replaced as a whole list, so you must resupply the current ones plus your change.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"form_id":{"type":"string","description":"The form's id from list_forms, or its exact name"}},"required":["form_id"]}`),
		},
		{
			Name:        "list_form_integrations",
			Description: "Returns every form integration in the working revision: which form field posts into which metric, with what aggregation, for which record statuses, and how form fields map to dimensions. Without one, a form's submitted numbers never reach the model.",
			Parameters:  noParams,
		},
		{
			Name:        "list_users",
			Description: "Returns the users of this application's workspaces: email, display name, roles, and their current access rules (dimension/member=level). Use before proposing set_user_access_rules.",
			Parameters:  noParams,
		},
		{
			Name:        "validate_formulas",
			Description: "Checks every calculated metric's formula in the working revision and reports any that reference a metric name which doesn't exist. Use this when the developer asks you to audit, validate, or check the model for broken formulas.",
			Parameters:  noParams,
		},
		{
			Name:        "check_grid_completeness",
			Description: "Checks every grid in the working revision and reports any that have no metrics and/or no dimensions configured. Use this when the developer asks you to audit, validate, or check the model for empty or misconfigured grids.",
			Parameters:  noParams,
		},
	}
}

// proposeActionsTool is the single write-side tool the LLM can call.
// It does not execute anything — it presents a plan for developer confirmation.
var proposeActionsTool = providers.ToolDef{
	Name: "propose_actions",
	Description: "Propose an ordered list of write operations for the developer to review and confirm. " +
		"Call this whenever you want to create, update, or delete any resource. " +
		"Do NOT attempt to execute write operations directly — always use this tool so the developer can confirm first.",
	Parameters: json.RawMessage(`{
		"type": "object",
		"properties": {
			"steps": {
				"type": "array",
				"description": "Ordered list of write actions to perform after developer confirms",
				"items": {
					"type": "object",
					"properties": {
						"tool":        {"type": "string",  "description": "Write tool name: create_metric | update_metric | delete_metric | create_dimension | add_dimension_member | update_dimension_member | create_grid | add_grid_metric | add_grid_dimension | create_dashboard | add_dashboard_widget | create_revision | create_workflow_def | update_workflow_def | delete_workflow_def | create_form_def | update_form_def | delete_form_def | create_automation_rule | update_automation_rule | delete_automation_rule | create_business_role | create_form_integration | update_form_integration | delete_form_integration | generate_migration | apply_migration | set_user_access_rules"},
						"description": {"type": "string",  "description": "One-line human-readable description of this step shown to the developer"},
						"params":      {"type": "object",  "description": "Parameters for the tool (must match the tool's required fields)"}
					},
					"required": ["tool", "description", "params"]
				}
			}
		},
		"required": ["steps"]
	}`),
}

// AllTools returns both read tools and the propose_actions write gateway.
// This is what the LLM receives on every Phase-2 chat call.
func AllTools() []providers.ToolDef {
	return append(ReadTools(), proposeActionsTool)
}

// IsWriteTool returns true for the propose_actions meta-tool (write gateway).
func IsWriteTool(name string) bool {
	return name == "propose_actions"
}

// ToolExecutor executes read-only tools and returns a plain-text result.
type ToolExecutor struct {
	pool    *pgxpool.Pool
	modelID string
	revID   string
}

func NewToolExecutor(pool *pgxpool.Pool, modelID, revID string) *ToolExecutor {
	return &ToolExecutor{pool: pool, modelID: modelID, revID: revID}
}

// Execute dispatches a tool call by name and returns its text output.
func (e *ToolExecutor) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "get_workflow":
		return e.getWorkflow(ctx, args)
	case "validate_workflow":
		return e.validateWorkflow(ctx, args)
	case "list_workflow_roles":
		return e.listWorkflowRoles(ctx)
	case "list_automation_rules":
		return e.listAutomationRules(ctx)
	case "get_form":
		return e.getForm(ctx, args)
	case "list_form_integrations":
		return e.listFormIntegrations(ctx)
	case "get_model_summary":
		return e.modelSummary(ctx)
	case "list_metrics":
		return e.listMetrics(ctx)
	case "list_dimensions":
		return e.listDimensions(ctx)
	case "list_grids":
		return e.listGrids(ctx)
	case "list_dashboards":
		return e.listDashboards(ctx)
	case "list_revisions":
		return e.listRevisions(ctx)
	case "list_workflows":
		return e.listWorkflows(ctx)
	case "list_forms":
		return e.listForms(ctx)
	case "list_users":
		return e.listUsers(ctx)
	case "validate_formulas":
		return e.validateFormulas(ctx)
	case "check_grid_completeness":
		return e.checkGridCompleteness(ctx)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func (e *ToolExecutor) modelSummary(ctx context.Context) (string, error) {
	var appName, modelName string
	_ = e.pool.QueryRow(ctx, `
		SELECT a.name, m.name FROM core.application a
		JOIN core.model m ON m.application_id = a.id
		WHERE m.id = $1::uuid`, e.modelID,
	).Scan(&appName, &modelName)

	var activeRev string
	_ = e.pool.QueryRow(ctx, `
		SELECT COALESCE(r.name, '') FROM core.model m
		LEFT JOIN model.revision r ON r.id = m.active_revision_id
		WHERE m.id = $1::uuid`, e.modelID,
	).Scan(&activeRev)

	var metricCount, dimCount, gridCount, dashCount, wfCount, formCount int
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`, e.modelID, e.revID).Scan(&metricCount)
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dimension_def WHERE model_id=$1::uuid`, e.modelID).Scan(&dimCount)
	// Grid/dashboard/form counts are revision-scoped like the metric count
	// above (SYNC-03): a model with three drafts otherwise reported every
	// draft's copies as if they were one revision's inventory.
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.grid_def WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL)`, e.modelID, e.revID).Scan(&gridCount)
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dashboard_def WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL)`, e.modelID, e.revID).Scan(&dashCount)
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.form_def WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL)`, e.modelID, e.revID).Scan(&formCount)
	// Real bug fixed here: this previously queried the nonexistent table
	// workflow.definition (the real table is workflow.workflow_def) — the
	// error was silently swallowed (like every other count in this
	// function), so "Workflows: %d" always reported 0 regardless of how
	// many actually existed.
	_ = e.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workflow.workflow_def wd
		JOIN core.model m ON m.application_id = wd.application_id
		WHERE m.id = $1::uuid AND (wd.revision_id IS NULL OR wd.revision_id = $2::uuid)
	`, e.modelID, e.revID).Scan(&wfCount)

	return fmt.Sprintf(
		"Application: %s | Model: %s | Active revision: %s | Working revision: %s\nMetrics: %d | Dimensions: %d | Grids: %d | Dashboards: %d | Forms: %d | Workflows: %d",
		appName, modelName, activeRev, e.revID, metricCount, dimCount, gridCount, dashCount, formCount, wfCount,
	), nil
}

func (e *ToolExecutor) listMetrics(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT name, is_input, COALESCE(formula,''), format, agg_rule
		FROM model.metric_def
		WHERE model_id=$1::uuid AND revision_id=$2::uuid
		ORDER BY is_input DESC, name`, e.modelID, e.revID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString("Metrics:\n")
	for rows.Next() {
		var name, formula, format, agg string
		var isInput bool
		_ = rows.Scan(&name, &isInput, &formula, &format, &agg)
		if isInput {
			fmt.Fprintf(&sb, "  [INPUT]  %s  (format:%s, agg:%s)\n", name, format, agg)
		} else {
			fmt.Fprintf(&sb, "  [CALC]   %s = %s  (format:%s, agg:%s)\n", name, formula, format, agg)
		}
	}
	return sb.String(), rows.Err()
}

func (e *ToolExecutor) listDimensions(ctx context.Context) (string, error) {
	parentDimByName := map[string]string{}
	pdRows, pdErr := e.pool.Query(ctx, `
		SELECT d.name, pd.name
		FROM model.dimension_def d
		JOIN model.dimension_def pd ON pd.id = d.parent_dimension_id
		WHERE d.model_id = $1::uuid`, e.modelID)
	if pdErr == nil {
		for pdRows.Next() {
			var name, parentName string
			if pdRows.Scan(&name, &parentName) == nil {
				parentDimByName[name] = parentName
			}
		}
		pdRows.Close()
	}

	rows, err := e.pool.Query(ctx, `
		SELECT d.name, m.code, m.label, COALESCE(pm.code,'') AS parent_code,
		       COALESCE(m.properties,'{}'::jsonb)::text,
		       d.dimension_type, COALESCE(d.time_granularity,''), COALESCE(d.fiscal_year_start_month,0),
		       COALESCE(m.period_start::text,''), COALESCE(m.period_end::text,'')
		FROM model.dimension_def d
		JOIN model.dimension_member m ON m.dimension_id = d.id
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE d.model_id = $1::uuid
		ORDER BY d.name, m.time_index NULLS LAST, m.sort_order`, e.modelID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type member struct{ code, label, parent, props, period string }
	dims := map[string][]member{}
	timeBadge := map[string]string{}
	var order []string
	for rows.Next() {
		var dname, code, label, parent, propsRaw, dimType, granularity, pStart, pEnd string
		var fiscalStart int
		_ = rows.Scan(&dname, &code, &label, &parent, &propsRaw, &dimType, &granularity, &fiscalStart, &pStart, &pEnd)
		period := ""
		if dimType == "time" {
			timeBadge[dname] = fmt.Sprintf(" [time · %s, fiscal year starts month %d]", granularity, fiscalStart)
			if pStart != "" {
				period = fmt.Sprintf(" %s..%s", pStart, pEnd)
			} else {
				period = " (aggregate period)"
			}
		}
		// Render properties as sorted key=value pairs — the AI was asked (live)
		// to re-parent members "by their category property" and couldn't,
		// because this listing never showed properties at all.
		props := ""
		var pm map[string]string
		if json.Unmarshal([]byte(propsRaw), &pm) == nil && len(pm) > 0 {
			keys := make([]string, 0, len(pm))
			for k := range pm {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			pairs := make([]string, 0, len(keys))
			for _, k := range keys {
				pairs = append(pairs, k+"="+pm[k])
			}
			props = " {" + strings.Join(pairs, ", ") + "}"
		}
		if _, ok := dims[dname]; !ok {
			order = append(order, dname)
		}
		dims[dname] = append(dims[dname], member{code, label, parent, props, period})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, d := range order {
		if parentName, ok := parentDimByName[d]; ok {
			fmt.Fprintf(&sb, "Dimension: %s (child of: %s — members below roll up to a %s member via parent)\n", d, parentName, parentName)
		} else {
			fmt.Fprintf(&sb, "Dimension: %s%s\n", d, timeBadge[d])
		}
		for _, m := range dims[d] {
			if m.parent != "" {
				fmt.Fprintf(&sb, "  %s (%s) → parent: %s%s\n", m.code, m.label, m.parent, m.props)
			} else {
				fmt.Fprintf(&sb, "  %s (%s)%s%s\n", m.code, m.label, m.period, m.props)
			}
		}
	}
	return sb.String(), nil
}

func (e *ToolExecutor) listGrids(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT g.id::text, g.name,
		       COALESCE((SELECT string_agg(md.name,', ') FROM model.grid_metric gm JOIN model.metric_def md ON md.id=gm.metric_id WHERE gm.grid_id=g.id),''),
		       COALESCE((SELECT string_agg(dd.name,', ') FROM model.grid_dimension gdim JOIN model.dimension_def dd ON dd.id=gdim.dimension_id WHERE gdim.grid_id=g.id),'')
		FROM model.grid_def g
		WHERE g.model_id=$1::uuid
		  AND (g.revision_id = $2::uuid OR g.revision_id IS NULL)
		ORDER BY g.name`, e.modelID, e.revID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString("Grids:\n")
	for rows.Next() {
		var id, name, metrics, dims string
		_ = rows.Scan(&id, &name, &metrics, &dims)
		fmt.Fprintf(&sb, "  %s (id:%s)\n    metrics: %s\n    dims: %s\n", name, id, metrics, dims)
	}
	return sb.String(), rows.Err()
}

func (e *ToolExecutor) listDashboards(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT d.id::text, d.name,
		       COUNT(w.id) AS widget_count
		FROM model.dashboard_def d
		LEFT JOIN model.dashboard_widget w ON w.dashboard_id = d.id
		WHERE d.model_id=$1::uuid
		  AND (d.revision_id = $2::uuid OR d.revision_id IS NULL)
		GROUP BY d.id, d.name ORDER BY d.name`, e.modelID, e.revID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString("Dashboards:\n")
	for rows.Next() {
		var id, name string
		var wc int
		_ = rows.Scan(&id, &name, &wc)
		fmt.Fprintf(&sb, "  %s (id:%s) — %d widget(s)\n", name, id, wc)
	}
	return sb.String(), rows.Err()
}

func (e *ToolExecutor) listRevisions(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT r.id::text, r.name, r.created_at::date,
		       (m.active_revision_id = r.id) AS is_active
		FROM model.revision r
		JOIN core.model m ON m.id = r.model_id
		WHERE r.model_id=$1::uuid
		ORDER BY r.created_at DESC`, e.modelID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString("Revisions:\n")
	for rows.Next() {
		var id, name, date string
		var active bool
		_ = rows.Scan(&id, &name, &date, &active)
		tag := ""
		if active {
			tag = " [ACTIVE]"
		}
		fmt.Fprintf(&sb, "  %s — %s (id:%s)%s\n", name, date, id, tag)
	}
	return sb.String(), rows.Err()
}

// listWorkflows scopes to workflows visible in the working revision the
// same way modelSummary's (fixed) workflow count does: application-joined
// through the model, and either revision-less (legacy rows from before
// migration 056) or matching this exact revision — never another
// revision's workflow of the same name.
func (e *ToolExecutor) listWorkflows(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT wd.id::text, wd.name, wd.trigger_event, wd.status, jsonb_array_length(wd.steps)
		FROM workflow.workflow_def wd
		JOIN core.model m ON m.application_id = wd.application_id
		WHERE m.id=$1::uuid AND (wd.revision_id IS NULL OR wd.revision_id=$2::uuid)
		ORDER BY wd.name`, e.modelID, e.revID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString("Workflows:\n")
	var count int
	for rows.Next() {
		var id, name, triggerEvent, status string
		var stepCount int
		if rows.Scan(&id, &name, &triggerEvent, &status, &stepCount) != nil {
			continue
		}
		count++
		fmt.Fprintf(&sb, "  %s (id:%s)  (trigger:%s, status:%s, steps:%d)\n", name, id, triggerEvent, status, stepCount)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count == 0 {
		return "No workflows defined yet.", nil
	}
	return sb.String(), nil
}

func (e *ToolExecutor) listForms(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT id::text, name, label, jsonb_array_length(CASE WHEN jsonb_typeof(fields) = 'array' THEN fields ELSE '[]'::jsonb END)
		FROM model.form_def
		WHERE model_id=$1::uuid AND (revision_id IS NULL OR revision_id=$2::uuid)
		ORDER BY name`, e.modelID, e.revID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString("Forms:\n")
	var count int
	for rows.Next() {
		var id, name, label string
		var fieldCount int
		if rows.Scan(&id, &name, &label, &fieldCount) != nil {
			continue
		}
		count++
		fmt.Fprintf(&sb, "  %s (id:%s, %s) — %d field(s)\n", name, id, label, fieldCount)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count == 0 {
		return "No forms defined yet.", nil
	}
	return sb.String(), nil
}

// validateFormulas checks every calculated metric's formula references an
// existing metric name, using the same existence check createMetric already
// runs pre-flight (model-scoped, not revision-scoped).
func (e *ToolExecutor) validateFormulas(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT name, formula FROM model.metric_def
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND is_input=false AND formula IS NOT NULL
		ORDER BY name`, e.modelID, e.revID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type brokenMetric struct {
		name   string
		reason string
	}
	// Collect first, then validate: metricformula queries the same pool, and
	// running those queries while this cursor is open would deadlock on a
	// single-connection pool.
	type metricRow struct{ name, formula string }
	var found []metricRow
	for rows.Next() {
		var m metricRow
		if rows.Scan(&m.name, &m.formula) != nil {
			continue
		}
		found = append(found, m)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	rows.Close()

	var checked int
	var problems []brokenMetric
	for _, m := range found {
		checked++
		// Report exactly what the developer role's own save would reject —
		// parse errors, unknown functions, self-reference, cycles and
		// unresolved names. Splitting the formula on operators and treating
		// each piece as a metric name, which this used to do, called every
		// function name and every legacy {reference} a missing metric, so a
		// healthy model reported as broken.
		if _, vErr := metricformula.Validate(ctx, e.pool, metricformula.Request{
			ModelID: e.modelID, RevisionID: e.revID, Name: m.name, Formula: m.formula,
		}); vErr != nil {
			var invalid *metricformula.ValidationError
			if !errors.As(vErr, &invalid) {
				return "", vErr
			}
			problems = append(problems, brokenMetric{name: m.name, reason: invalid.Message})
		}
	}

	if len(problems) == 0 {
		return fmt.Sprintf("All %d calculated metric(s) have valid formulas.", checked), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d of %d calculated metric(s) with invalid formulas:\n", len(problems), checked)
	for _, p := range problems {
		fmt.Fprintf(&sb, "  %s: %s\n", p.name, p.reason)
	}
	return sb.String(), nil
}

// checkGridCompleteness flags grids with no metrics and/or no dimensions
// configured. Matches list_grids' scoping (model-wide, not revision-filtered)
// so the two tools always agree on which grids exist.
func (e *ToolExecutor) checkGridCompleteness(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT g.id::text, g.name,
		       (SELECT COUNT(*) FROM model.grid_metric gm WHERE gm.grid_id=g.id),
		       (SELECT COUNT(*) FROM model.grid_dimension gd WHERE gd.grid_id=g.id)
		FROM model.grid_def g
		WHERE g.model_id=$1::uuid
		ORDER BY g.name`, e.modelID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type incompleteGrid struct {
		id, name                    string
		missingMetrics, missingDims bool
	}
	var checked int
	var problems []incompleteGrid
	for rows.Next() {
		var id, name string
		var metricCount, dimCount int
		if rows.Scan(&id, &name, &metricCount, &dimCount) != nil {
			continue
		}
		checked++
		if metricCount == 0 || dimCount == 0 {
			problems = append(problems, incompleteGrid{
				id: id, name: name,
				missingMetrics: metricCount == 0,
				missingDims:    dimCount == 0,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	if len(problems) == 0 {
		return fmt.Sprintf("All %d grid(s) have at least one metric and one dimension configured.", checked), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d of %d grid(s) with missing configuration:\n", len(problems), checked)
	for _, p := range problems {
		var missing []string
		if p.missingMetrics {
			missing = append(missing, "metrics")
		}
		if p.missingDims {
			missing = append(missing, "dimensions")
		}
		fmt.Fprintf(&sb, "  %s (id:%s): missing %s\n", p.name, p.id, strings.Join(missing, " and "))
	}
	return sb.String(), nil
}

// listUsers returns the users of this application's customer (all its
// workspaces): email, display name, roles, and each user's current access
// rules with the dimension/member they point at spelled out by name — the
// shape set_user_access_rules consumes, so the model can read the current
// state and propose a replacement without ever handling a UUID.
func (e *ToolExecutor) listUsers(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT u.email, u.display_name,
		       COALESCE(string_agg(DISTINCT ra.role, ','), ''),
		       COALESCE((
		           SELECT string_agg(d.name || '/' || m.code || '=' || r.access, ', ' ORDER BY d.name || '/' || m.code)
		           FROM identity.user_access_rule r
		           JOIN model.dimension_member m ON m.id::text = r.ref_id
		           JOIN model.dimension_def d ON d.id = m.dimension_id
		           WHERE r.user_id = u.id AND r.rule_type = 'dimension_member'
		       ), '')
		FROM identity.user u
		JOIN identity.role_assignment ra ON ra.user_id = u.id
		JOIN core.workspace w ON w.id = ra.workspace_id
		WHERE w.customer_id = (
		    SELECT COALESCE(a.customer_id, ws.customer_id)
		    FROM core.model mo
		    JOIN core.application a ON a.id = mo.application_id
		    LEFT JOIN core.workspace ws ON ws.id = a.workspace_id
		    WHERE mo.id = $1::uuid
		)
		GROUP BY u.id, u.email, u.display_name
		ORDER BY u.email
	`, e.modelID)
	if err != nil {
		return "", fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var b strings.Builder
	b.WriteString("Users in this application's workspaces:\n")
	n := 0
	for rows.Next() {
		var email, name, roles, rules string
		if err := rows.Scan(&email, &name, &roles, &rules); err != nil {
			return "", err
		}
		n++
		if rules == "" {
			rules = "none (full access)"
		}
		fmt.Fprintf(&b, "- %s (%s) — roles: %s — access rules: %s\n", email, name, roles, rules)
	}
	if n == 0 {
		return "No users found for this application's customer.", nil
	}
	return b.String(), nil
}
