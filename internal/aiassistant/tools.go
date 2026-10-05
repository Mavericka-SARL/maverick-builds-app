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
	tools := []providers.ToolDef{
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
			Description: "Returns every dimension and its members: code, label, parent hierarchy, the dimension's DECLARED properties with their data types (text | number | date — only a declared property can be read in a formula as dimension.property), and each member's property values as {key=value} — use these to group or re-parent members by a property (e.g. category). A time dimension is marked [time · granularity] and lists its leaf periods in chronological order with their dates and its aggregate periods (H1, FY26) as such; only such a dimension supports time-series formulas (PREVIOUS, LAG, LEAD, OFFSET, MOVINGSUM, CUMULATE, the *TODATE family incl. HALFYEARTODATE, YEARVALUE/HALFYEARVALUE/QUARTERVALUE/MONTHVALUE, TIMESUM, START, END, ...).",
			Parameters:  noParams,
		},
		{
			Name:        "list_grids",
			Description: "Returns all grid definitions with their metrics and dimensions (and a dimension's display level when one is set).",
			Parameters:  noParams,
		},
		{
			Name:        "list_dashboards",
			Description: "Returns the dashboard folders, then every dashboard with its folder, tags and widgets — each widget's id, type, what it shows, position and size.",
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
			Description: "Returns one workflow definition in full — steps, context_schema, subject, single_active_instance, approver_may_start, the automation rules that start it, and the Validate verdict. Call it before update_workflow_def on an existing workflow: steps are replaced as a whole list, so you must resupply the current ones plus your change.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"workflow_def_id":{"type":"string","description":"The workflow's id from list_workflows, or its exact name"}},"required":["workflow_def_id"]}`),
		},
		{
			Name:        "validate_workflow",
			Description: "The developer's Validate button: checks a workflow the way Publish will (ids, routes, roles, reachability, loops). Pass workflow_def_id for a stored one, or name + steps (+ context_schema) for steps you are about to propose. Use it before proposing update_workflow_def so the developer is not handed a workflow they cannot publish.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"workflow_def_id":{"type":"string"},"name":{"type":"string"},"steps":{"type":"array","items":{"type":"object"}},"context_schema":{"type":"array","items":{"type":"object"}}},"required":[]}`),
		},
		{
			Name:        "list_workflow_roles",
			Description: "Returns what a step's assignee_roles and a notification's recipient_role may contain: the platform role codes, and this application's business roles by name with their member counts and the dashboards of this revision each may open. Call it before assigning any step; propose create_business_role when the role a developer names does not exist.",
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
			Description: "Returns the users of this application's workspaces: email, display name, roles, and their current access rules: member rules in the active revision (dimension/member=level) — the set set_user_access_rules replaces — plus metric rules and a count of member rules on members not in the active revision, which it keeps. Use before proposing set_user_access_rules.",
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
	for _, d := range integrationToolDefs() {
		tools = append(tools, providers.ToolDef{Name: d.Name, Description: d.Description, Parameters: json.RawMessage(d.Parameters)})
	}
	return tools
}

// WriteToolNames lists every tool a proposal step may name, grouped the way
// the prompt documents them. The propose_actions schema's enum and the
// prompt's tool list are built from it, and a test checks that
// WriteExecutor.Execute runs every tool on it — the list, the schema and the
// executor once disagreed (two migration tools the schema offered did
// nothing). A tool added to Execute must be added here to be offered.
var WriteToolNames = []string{
	"create_metric", "update_metric", "delete_metric",
	"create_dimension", "update_dimension", "delete_dimension",
	"add_dimension_member", "update_dimension_member", "delete_dimension_member", "generate_time_members",
	"reorder_dimension_members",
	"add_dimension_property", "update_dimension_property", "delete_dimension_property",
	"create_grid", "update_grid", "delete_grid",
	"add_grid_metric", "remove_grid_metric", "reorder_grid_metrics", "add_grid_dimension", "update_grid_dimension", "remove_grid_dimension",
	"create_dashboard_folder", "update_dashboard_folder", "delete_dashboard_folder",
	"create_dashboard", "update_dashboard", "delete_dashboard",
	"add_dashboard_widget", "update_dashboard_widget", "delete_dashboard_widget",
	"set_tags", "create_revision",
	"create_workflow_def", "update_workflow_def", "delete_workflow_def",
	"archive_workflow_def", "restore_workflow_def", "duplicate_workflow_def",
	"create_form_def", "update_form_def", "delete_form_def",
	"create_automation_rule", "update_automation_rule", "delete_automation_rule",
	"create_business_role", "update_business_role", "delete_business_role", "set_role_dashboards",
	"create_form_integration", "update_form_integration", "delete_form_integration", "backfill_form_integration",
	"set_user_access_rules",
	"create_file_integration", "import_file_data", "write_input_values", "create_export_integration", "update_integration", "delete_integration",
}

// proposeActionsTool is the single write-side tool the LLM can call.
// It does not execute anything — it presents a plan for developer confirmation.
var proposeActionsTool = providers.ToolDef{
	Name: "propose_actions",
	Description: "Propose an ordered list of write operations for the developer to review and confirm. " +
		"Call this whenever you want to create, update, or delete any resource. " +
		"Do NOT attempt to execute write operations directly — always use this tool so the developer can confirm first.",
	Parameters: proposeActionsSchema(),
}

func proposeActionsSchema() json.RawMessage {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"steps": map[string]any{
				"type":        "array",
				"description": "Ordered list of write actions to perform after developer confirms",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"tool": map[string]any{"type": "string", "enum": WriteToolNames,
							"description": "Write tool name (the system prompt documents each one's params)"},
						"description": map[string]any{"type": "string",
							"description": "One-line human-readable description of this step shown to the developer"},
						"params": map[string]any{"type": "object",
							"description": "Parameters for the tool (must match the tool's required fields). create_dimension takes name, and optionally agg_rule, parent_dimension_name, dimension_type/time_granularity/fiscal_year_start_month, tags, members, and for a property grouping source_dimension_id (id or name) + source_property (declared on the source) + derive_members"},
					},
					"required": []string{"tool", "description", "params"},
				},
			},
		},
		"required": []string{"steps"},
	}
	b, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	return b
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
	hooks   ReadHooks
}

func NewToolExecutor(pool *pgxpool.Pool, modelID, revID string) *ToolExecutor {
	return &ToolExecutor{pool: pool, modelID: modelID, revID: revID}
}

// Execute dispatches a tool call by name and returns its text output.
func (e *ToolExecutor) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	out, err := e.execute(ctx, name, args)
	if err == nil && strings.TrimSpace(out) == "" {
		// An empty answer reads as no answer: on an empty revision the model
		// called list_dimensions three times running and was stopped as stuck.
		return fmt.Sprintf("%s: nothing yet — the working revision has none.", name), nil
	}
	return out, err
}

func (e *ToolExecutor) execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
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
	case "list_integrations":
		return e.listIntegrations(ctx)
	case "preview_file_import":
		return e.previewFileImport(ctx, args)
	case "prepare_converted_file":
		return e.prepareConvertedFile(ctx, args)
	case "read_attached_sheet":
		return e.readAttachedSheet(ctx, args)
	case "preview_export":
		return e.previewExport(ctx, args)
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

// tagSuffix renders a definition's tags for the listings (", tags: a, b"),
// or nothing when it has none.
func tagSuffix(tagList []string) string {
	if len(tagList) == 0 {
		return ""
	}
	return ", tags: " + strings.Join(tagList, ", ")
}

func (e *ToolExecutor) listMetrics(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT m.name, m.is_input, COALESCE(m.formula,''), m.format, m.agg_rule, m.tags, COALESCE(m.label,''),
		       COALESCE((SELECT d.name FROM model.dimension_def d WHERE d.id = m.picklist_dimension_id),''),
		       CASE WHEN m.highlight_rules = '[]'::jsonb THEN '' ELSE m.highlight_rules::text END
		FROM model.metric_def m
		WHERE m.model_id=$1::uuid AND m.revision_id=$2::uuid
		ORDER BY m.is_input DESC, m.name`, e.modelID, e.revID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var sb strings.Builder
	sb.WriteString("Metrics:\n")
	for rows.Next() {
		var name, formula, format, agg string
		var isInput bool
		var tagList []string
		var label, picklist, highlights string
		_ = rows.Scan(&name, &isInput, &formula, &format, &agg, &tagList, &label, &picklist, &highlights)
		if picklist != "" {
			format += " of " + picklist
		}
		if highlights != "" {
			agg += ", highlight_rules: " + highlights
		}
		if label != "" {
			name = fmt.Sprintf("%s \"%s\"", name, label)
		}
		if isInput {
			fmt.Fprintf(&sb, "  [INPUT]  %s  (format:%s, agg:%s%s)\n", name, format, agg, tagSuffix(tagList))
		} else {
			fmt.Fprintf(&sb, "  [CALC]   %s = %s  (format:%s, agg:%s%s)\n", name, formula, format, agg, tagSuffix(tagList))
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
		WHERE d.model_id = $1::uuid AND ($2 = '' OR d.revision_id::text = $2 OR d.revision_id IS NULL)`, e.modelID, e.revID)
	if pdErr == nil {
		for pdRows.Next() {
			var name, parentName string
			if pdRows.Scan(&name, &parentName) == nil {
				parentDimByName[name] = parentName
			}
		}
		pdRows.Close()
	}

	// Property groupings: "area groups employees by area".
	groupingByName := map[string]string{}
	gRows, gErr := e.pool.Query(ctx, `
		SELECT d.name, sd.name, COALESCE(d.source_property,'')
		FROM model.dimension_def d
		JOIN model.dimension_def sd ON sd.id = d.source_dimension_id
		WHERE d.model_id = $1::uuid AND ($2 = '' OR d.revision_id::text = $2 OR d.revision_id IS NULL)`, e.modelID, e.revID)
	if gErr == nil {
		for gRows.Next() {
			var name, sourceName, prop string
			if gRows.Scan(&name, &sourceName, &prop) == nil {
				groupingByName[name] = fmt.Sprintf(" (groups %s by its property %s — a member stands for every %s member whose %s equals its code)",
					sourceName, prop, sourceName, prop)
			}
		}
		gRows.Close()
	}

	// The declared properties (model.dimension_property), which are what a
	// formula can read as dimension.property, typed by data_type. Member
	// values alone do not say that: a key no declaration names is stored but
	// refused in a formula (UNKNOWN_PROPERTY).
	declaredProps := map[string][]string{}
	dpRows, dpErr := e.pool.Query(ctx, `
		SELECT d.name, p.name, p.data_type
		FROM model.dimension_property p
		JOIN model.dimension_def d ON d.id = p.dimension_id
		WHERE d.model_id = $1::uuid AND ($2 = '' OR d.revision_id::text = $2 OR d.revision_id IS NULL)
		ORDER BY d.name, lower(p.name)`, e.modelID, e.revID)
	if dpErr == nil {
		for dpRows.Next() {
			var dname, pname, ptype string
			if dpRows.Scan(&dname, &pname, &ptype) == nil {
				declaredProps[dname] = append(declaredProps[dname], pname+" ("+ptype+")")
			}
		}
		dpRows.Close()
	}

	// The working revision's dimensions only: every revision holds its own
	// copy, and reading them all listed each member once per revision. A
	// LEFT JOIN, so a dimension with no members yet is still listed.
	rows, err := e.pool.Query(ctx, `
		SELECT d.name, COALESCE(m.code,''), COALESCE(m.label,''), COALESCE(pm.code,'') AS parent_code,
		       COALESCE(m.properties,'{}'::jsonb)::text,
		       d.dimension_type, COALESCE(d.time_granularity,''), COALESCE(d.fiscal_year_start_month,0),
		       COALESCE(m.period_start::text,''), COALESCE(m.period_end::text,''), d.tags, COALESCE(btrim(m.formula),''),
		       d.business_maintained
		FROM model.dimension_def d
		LEFT JOIN model.dimension_member m ON m.dimension_id = d.id
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE d.model_id = $1::uuid AND ($2 = '' OR d.revision_id::text = $2 OR d.revision_id IS NULL)
		ORDER BY d.name, m.time_index NULLS LAST, m.sort_order`, e.modelID, e.revID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type member struct{ code, label, parent, props, period string }
	dims := map[string][]member{}
	timeBadge := map[string]string{}
	dimTags := map[string]string{}
	var order []string
	for rows.Next() {
		var dname, code, label, parent, propsRaw, dimType, granularity, pStart, pEnd, memberFormula string
		var fiscalStart int
		var tagList []string
		var businessMaintained bool
		_ = rows.Scan(&dname, &code, &label, &parent, &propsRaw, &dimType, &granularity, &fiscalStart, &pStart, &pEnd, &tagList, &memberFormula, &businessMaintained)
		if len(tagList) > 0 {
			dimTags[dname] = " [" + strings.TrimPrefix(tagSuffix(tagList), ", ") + "]"
		}
		if businessMaintained && !strings.Contains(dimTags[dname], "business-maintained") {
			dimTags[dname] += " [business-maintained: business users add, rename and remove its members]"
		}
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
			dims[dname] = nil
		}
		if memberFormula != "" {
			period += " = " + memberFormula + " (calculated member)"
		}
		if code != "" {
			dims[dname] = append(dims[dname], member{code, label, parent, props, period})
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, d := range order {
		if parentName, ok := parentDimByName[d]; ok {
			fmt.Fprintf(&sb, "Dimension: %s (child of: %s — members below roll up to a %s member via parent)%s\n", d, parentName, parentName, dimTags[d])
		} else {
			fmt.Fprintf(&sb, "Dimension: %s%s%s%s\n", d, timeBadge[d], groupingByName[d], dimTags[d])
		}
		if props := declaredProps[d]; len(props) > 0 {
			fmt.Fprintf(&sb, "  Declared properties: %s\n", strings.Join(props, ", "))
		} else {
			sb.WriteString("  Declared properties: none\n")
		}
		if len(dims[d]) == 0 {
			sb.WriteString("  (no members yet)\n")
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
		SELECT g.id::text, g.name, g.tags,
		       COALESCE((SELECT string_agg(md.name,', ') FROM model.grid_metric gm JOIN model.metric_def md ON md.id=gm.metric_id WHERE gm.grid_id=g.id),''),
		       COALESCE((SELECT string_agg(dd.name || COALESCE(' (display level ' || gdim.display_level || ')', ''), ', ')
		                 FROM model.grid_dimension gdim JOIN model.dimension_def dd ON dd.id=gdim.dimension_id WHERE gdim.grid_id=g.id),'')
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
		var tagList []string
		_ = rows.Scan(&id, &name, &tagList, &metrics, &dims)
		fmt.Fprintf(&sb, "  %s (id:%s%s)\n    metrics: %s\n    dims: %s\n", name, id, tagSuffix(tagList), metrics, dims)
	}
	return sb.String(), rows.Err()
}

// listDashboards lists the working revision's folders, then each dashboard
// with its folder and every widget — id, type, what it shows (a grid
// widget's chosen metrics too), place and size — so a widget can be named in update_dashboard_widget or
// delete_dashboard_widget.
func (e *ToolExecutor) listDashboards(ctx context.Context) (string, error) {
	var sb strings.Builder
	frows, err := e.pool.Query(ctx, `
		SELECT f.id::text, f.name, COALESCE(p.name, '')
		FROM model.dashboard_folder f LEFT JOIN model.dashboard_folder p ON p.id = f.parent_id
		WHERE f.model_id=$1::uuid AND (f.revision_id = $2::uuid OR f.revision_id IS NULL)
		ORDER BY f.name`, e.modelID, e.revID)
	if err != nil {
		return "", err
	}
	sb.WriteString("Folders:\n")
	nf := 0
	for frows.Next() {
		var id, name, parent string
		_ = frows.Scan(&id, &name, &parent)
		nf++
		if parent != "" {
			fmt.Fprintf(&sb, "  %s (id:%s, in %s)\n", name, id, parent)
		} else {
			fmt.Fprintf(&sb, "  %s (id:%s)\n", name, id)
		}
	}
	frows.Close()
	if nf == 0 {
		sb.WriteString("  none\n")
	}

	rows, err := e.pool.Query(ctx, `
		SELECT d.id::text, d.name, d.tags, COALESCE(f.name, ''),
		       w.id::text, COALESCE(w.widget_type, ''),
		       COALESCE(md.name, gd.name, fd.name, w.ref_id, ''),
		       COALESCE(w.pos_x, 0), COALESCE(w.pos_y, 0), COALESCE(w.size_w, 0), COALESCE(w.size_h, 0), COALESCE(w.title, ''),
		       COALESCE((SELECT string_agg(m.name, ', ' ORDER BY o.ord)
		                 FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(w.widget_props->'metric_ids') = 'array'
		                                                     THEN w.widget_props->'metric_ids' ELSE '[]'::jsonb END) WITH ORDINALITY o(id, ord)
		                 JOIN model.metric_def m ON m.id::text = o.id), '')
		FROM model.dashboard_def d
		LEFT JOIN model.dashboard_folder f ON f.id = d.folder_id
		LEFT JOIN model.dashboard_widget w ON w.dashboard_id = d.id
		LEFT JOIN model.metric_def md ON w.widget_type = 'metric_kpi' AND md.id::text = w.ref_id
		LEFT JOIN model.grid_def gd ON w.widget_type IN ('chart','grid','import') AND gd.id::text = w.ref_id
		LEFT JOIN model.form_def fd ON w.widget_type = 'form' AND fd.id::text = w.ref_id
		WHERE d.model_id=$1::uuid
		  AND (d.revision_id = $2::uuid OR d.revision_id IS NULL)
		ORDER BY d.name, d.id, w.pos_y, w.pos_x`, e.modelID, e.revID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	sb.WriteString("Dashboards:\n")
	last := ""
	for rows.Next() {
		var id, name, folder string
		var tagList []string
		var wid *string
		var wtype, ref, title, shows string
		var x, y, w, h int
		if err := rows.Scan(&id, &name, &tagList, &folder, &wid, &wtype, &ref, &x, &y, &w, &h, &title, &shows); err != nil {
			return "", err
		}
		if id != last {
			last = id
			where := ""
			if folder != "" {
				where = " in folder " + folder
			}
			fmt.Fprintf(&sb, "  %s (id:%s)%s%s\n", name, id, where, tagSuffix(tagList))
		}
		if wid == nil {
			sb.WriteString("    no widgets\n")
			continue
		}
		line := fmt.Sprintf("    widget (id:%s) %s", *wid, wtype)
		if ref != "" {
			line += " → " + ref
		}
		line += fmt.Sprintf(" at (%d,%d) size %dx%d", x, y, w, h)
		if title != "" {
			line += fmt.Sprintf(" title %q", title)
		}
		if shows != "" {
			line += " showing metrics " + shows
		}
		sb.WriteString(line + "\n")
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
		SELECT id::text, name, formula, format = 'text' FROM model.metric_def
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
	type metricRow struct {
		id, name, formula string
		text              bool
	}
	var found []metricRow
	for rows.Next() {
		var m metricRow
		if rows.Scan(&m.id, &m.name, &m.formula, &m.text) != nil {
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
		// healthy model reported as broken. MetricID makes the check see the
		// metric's own place in the graph — the recurrence it belongs to
		// (contract C4) and its own grid placement.
		if _, vErr := metricformula.Validate(ctx, e.pool, metricformula.Request{
			ModelID: e.modelID, RevisionID: e.revID, MetricID: m.id, Name: m.name, Formula: m.formula, Text: m.text,
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
// state and propose a replacement without ever handling a UUID. Member rules
// are shown as they resolve (by lineage) in the active revision — the slice
// set_user_access_rules replaces; metric rules and member rules on rows not
// in the active revision are shown too, as rules that tool keeps.
func (e *ToolExecutor) listUsers(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT u.email, u.display_name,
		       COALESCE(string_agg(DISTINCT ra.role::text, ','), ''),
		       COALESCE((
		           SELECT string_agg(DISTINCT d.name || '/' || m.code || '=' || r.access, ', ')
		           FROM identity.user_access_rule r
		           JOIN model.dimension_member m ON (m.lineage_id = r.ref_lineage_id OR m.id::text = r.ref_id)
		           JOIN model.dimension_def d ON d.id = m.dimension_id
		           WHERE r.user_id = u.id AND r.rule_type = 'dimension_member'
		             AND d.model_id = $1::uuid
		             AND (d.revision_id = act.rev OR d.revision_id IS NULL)
		       ), ''),
		       COALESCE((
		           SELECT string_agg(DISTINCT md.name || '=' || r.access, ', ')
		           FROM identity.user_access_rule r
		           JOIN model.metric_def md ON (md.lineage_id = r.ref_lineage_id OR md.id::text = r.ref_id)
		           WHERE r.user_id = u.id AND r.rule_type = 'metric'
		             AND md.model_id = $1::uuid
		             AND (md.revision_id = act.rev OR md.revision_id IS NULL)
		       ), ''),
		       (SELECT count(*) FROM identity.user_access_rule r
		        WHERE r.user_id = u.id AND r.rule_type = 'dimension_member'
		          AND NOT EXISTS (
		              SELECT 1 FROM model.dimension_member m
		              JOIN model.dimension_def d ON d.id = m.dimension_id
		              WHERE d.model_id = $1::uuid
		                AND (d.revision_id = act.rev OR d.revision_id IS NULL)
		                AND (m.lineage_id = r.ref_lineage_id OR m.id::text = r.ref_id)))
		FROM identity.user u
		CROSS JOIN (
		    SELECT COALESCE(mo.active_revision_id,
		           (SELECT id FROM model.revision WHERE model_id = mo.id ORDER BY created_at LIMIT 1)) AS rev
		    FROM core.model mo WHERE mo.id = $1::uuid
		) act
		JOIN identity.role_assignment ra ON ra.user_id = u.id
		JOIN core.workspace w ON w.id = ra.workspace_id
		WHERE w.customer_id = (
		    SELECT COALESCE(a.customer_id, ws.customer_id)
		    FROM core.model mo
		    JOIN core.application a ON a.id = mo.application_id
		    LEFT JOIN core.workspace ws ON ws.id = a.workspace_id
		    WHERE mo.id = $1::uuid
		)
		GROUP BY u.id, u.email, u.display_name, act.rev
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
		var email, name, roles, rules, metricRules string
		var otherMemberRules int64
		if err := rows.Scan(&email, &name, &roles, &rules, &metricRules, &otherMemberRules); err != nil {
			return "", err
		}
		n++
		if rules == "" {
			rules = "none"
		}
		fmt.Fprintf(&b, "- %s (%s) — roles: %s — member access rules (active revision): %s", email, name, roles, rules)
		if metricRules != "" {
			fmt.Fprintf(&b, " — metric access rules (kept by set_user_access_rules; business admin console manages them): %s", metricRules)
		}
		if otherMemberRules > 0 {
			fmt.Fprintf(&b, " — %d other member rule(s) on members not in this model's active revision (kept by set_user_access_rules)", otherMemberRules)
		}
		b.WriteString("\n")
	}
	if n == 0 {
		return "No users found for this application's customer.", nil
	}
	return b.String(), nil
}

// MaxProposalSteps is the most steps one propose_actions call may carry;
// the gateway rejects a larger plan before it becomes a proposal and tells
// the model to send the first batch. A whole workbook stage (a sheet's
// inputs, grids and values) fits in one at 100; it was 50.
const MaxProposalSteps = 100
