package aiassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// PlanWorkflowProblems checks, on the dry run's transaction once a plan has
// run, every workflow the plan's update_workflow_def steps leave behind as
// Validate in Developer › Workflows does: a plan saving a workflow a
// developer could not publish is refused like a step that fails.
// gpt-5-mini routed a fork straight to its join (the branch tasks
// unreachable) and a rejection to the end instead of back to the fan-out,
// and the plan was saved (2026-10-20). One line per invalid workflow, named
// by the last step that changed it.
func PlanWorkflowProblems(ctx context.Context, tx pgx.Tx, modelID, revID string, steps []ProposalStep) []string {
	last := map[string]int{} // workflow def ID → index of the last step updating it
	var order []string
	for i, step := range steps {
		if step.Tool != "update_workflow_def" {
			continue
		}
		var p struct {
			WorkflowDefID string `json:"workflow_def_id"`
		}
		if json.Unmarshal(step.Params, &p) != nil || p.WorkflowDefID == "" {
			continue
		}
		id, err := resolveWorkflowDefRef(ctx, tx, modelID, revID, p.WorkflowDefID)
		if err != nil {
			continue // the step itself reports it
		}
		if _, seen := last[id]; !seen {
			order = append(order, id)
		}
		last[id] = i
	}
	store := workflow.NewStoreOn(tx)
	var out []string
	for _, id := range order {
		def, err := store.GetWorkflowDefFull(ctx, id)
		if err != nil {
			continue
		}
		errs := append(workflow.ValidateDef(def), store.CheckConditionNames(ctx, def)...)
		if len(errs) == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("step %d (update_workflow_def %s): the workflow would be saved invalid — a developer could not publish it: %s. "+
			"Every step must be reachable from the first, a parallel fan-out's branches each lead to the join, and a rejection goes back to a step of the flow",
			last[id]+1, def.Name, strings.Join(errs, "; ")))
	}
	return out
}

// planWorkflowRefs are the workflows a plan creates or changes, and those
// its automation rules start, as resolved on the dry run's transaction.
func planWorkflowRefs(ctx context.Context, tx pgx.Tx, modelID, revID string, steps []ProposalStep) []string {
	seen := map[string]bool{}
	var out []string
	for _, step := range steps {
		var p struct {
			Name          string `json:"name"`
			WorkflowDefID string `json:"workflow_def_id"`
			WorkflowName  string `json:"workflow_name"`
		}
		if json.Unmarshal(step.Params, &p) != nil {
			continue
		}
		var refs []string
		switch step.Tool {
		case "create_workflow_def":
			refs = []string{p.Name}
		case "update_workflow_def", "publish_workflow_def":
			refs = []string{p.WorkflowDefID}
		case "create_automation_rule", "update_automation_rule":
			refs = []string{p.WorkflowDefID, p.WorkflowName}
		}
		for _, ref := range refs {
			if strings.TrimSpace(ref) == "" {
				continue
			}
			id, err := resolveWorkflowDefRef(ctx, tx, modelID, revID, ref)
			if err != nil || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// PlanRecordWorkflowProblems refuses a plan leaving a workflow that a
// table's form_submit rule starts, with an approval step, while its subject
// is not that table's record: approved, the record's status never moves
// and nothing it posts reaches the model. The High-Tech build's hiring
// approval was saved that way although the request named its table
// (2026-11-11); both approvals completed and the request stayed "submitted".
func PlanRecordWorkflowProblems(ctx context.Context, tx pgx.Tx, modelID, revID string, steps []ProposalStep) []string {
	store := workflow.NewStoreOn(tx)
	var out []string
	for _, id := range planWorkflowRefs(ctx, tx, modelID, revID, steps) {
		def, err := store.GetWorkflowDefFull(ctx, id)
		if err != nil || !hasApproval(def.Steps) {
			continue
		}
		var subj struct {
			FormID string `json:"form_id"`
		}
		_ = json.Unmarshal(def.SubjectConfig, &subj)
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT r.source_form_id::text, fd.name
			FROM workflow.automation_rule r
			JOIN model.form_def fd ON fd.id = r.source_form_id
			WHERE r.trigger_type = 'form_submit' AND (r.workflow_def_id = $1::uuid OR r.workflow_name = $2)
			  AND (r.revision_id IS NULL OR r.revision_id = $3::uuid)
			  AND EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(fd.fields) = 'array' THEN fd.fields ELSE '[]'::jsonb END) e
			              WHERE e->>'type' = 'status')`, id, def.Name, revID)
		if err != nil {
			continue
		}
		for rows.Next() {
			var formID, table string
			if rows.Scan(&formID, &table) != nil {
				continue
			}
			if (def.SubjectType == "form_record" || def.SubjectType == "form_records") && subj.FormID == formID {
				continue
			}
			out = append(out, fmt.Sprintf("workflow %s is started when a record of the table %s is submitted and has an approval, but its subject is not that table's record "+
				"(subject_type %q): approving a request would never set the record's status, so nothing it posts reaches the model. "+
				"Set subject_type \"form_record\" and subject_config {\"form_id\": \"%s\"} on the workflow (update_workflow_def)",
				def.Name, table, def.SubjectType, table))
		}
		rows.Close()
	}
	return out
}

// hasApproval reports a workflow with a step that decides (approve or reject).
func hasApproval(steps json.RawMessage) bool {
	var list []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(steps, &list) != nil {
		return false
	}
	for _, s := range list {
		if s.Type == "approval" {
			return true
		}
	}
	return false
}

// scenarioLike matches a dimension whose members are alternatives — a
// scenario, a version, a case — that are never added up.
var scenarioLike = regexp.MustCompile(`(?i)\b(scenarios?|versions?|cases?|plan[ _-]?types?)\b|^(scenario|version|case)`)

// PlanConditionTotalWarnings names each workflow condition that reads a
// metric over a scenario-like dimension no context variable picks and that
// totals by adding up: the condition reads Base + Downside + Upside. A 1/0
// PASS flag read 3 at the Scenario total, "= 1" was false, and the June close
// took the exception path (2026-11-11).
func PlanConditionTotalWarnings(ctx context.Context, tx pgx.Tx, modelID, revID string, steps []ProposalStep) []string {
	store := workflow.NewStoreOn(tx)
	var out []string
	for _, id := range planWorkflowRefs(ctx, tx, modelID, revID, steps) {
		def, err := store.GetWorkflowDefFull(ctx, id)
		if err != nil {
			continue
		}
		var schema []struct {
			DataType    string `json:"data_type"`
			DimensionID string `json:"dimension_id"`
		}
		_ = json.Unmarshal(def.ContextSchema, &schema)
		pinned := []string{} // never nil: = ANY(NULL) would drop every row
		for _, v := range schema {
			if v.DataType == "Dimension member" && v.DimensionID != "" {
				pinned = append(pinned, v.DimensionID)
			}
		}
		var list []map[string]any
		if json.Unmarshal(def.Steps, &list) != nil {
			continue
		}
		for _, st := range list {
			raw, _ := json.Marshal(st["condition"])
			var c struct {
				Formula string `json:"formula"`
			}
			if json.Unmarshal(raw, &c) != nil || strings.TrimSpace(c.Formula) == "" {
				continue
			}
			label, _ := st["name"].(string)
			low := strings.ToLower(c.Formula)
			rows, err := tx.Query(ctx, `
				SELECT m.name, COALESCE(NULLIF(d.label,''), d.name), d.name
				FROM model.metric_def m
				JOIN model.grid_metric gm ON gm.metric_id = m.id
				JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
				JOIN model.dimension_def d ON d.id = gd.dimension_id
				LEFT JOIN model.metric_dimension_rule r ON r.metric_id = m.id AND r.dimension_id = d.id
				WHERE m.model_id = $1::uuid AND m.revision_id = $2::uuid
				  AND $3 ~ ('(^|[^a-z0-9_])' || lower(m.name) || '([^a-z0-9_]|$)')
				  AND COALESCE(d.dimension_type, '') <> 'time'
				  AND NOT (d.id::text = ANY($4))
				  AND COALESCE(r.rule, d.agg_rule, 'sum') IN ('sum', '')
				ORDER BY m.name`, modelID, revID, low, pinned)
			if err != nil {
				continue
			}
			for rows.Next() {
				var metric, dimLabel, dimName string
				if rows.Scan(&metric, &dimLabel, &dimName) != nil || !scenarioLike.MatchString(dimName+" "+dimLabel) || strings.Contains(low, strings.ToLower(dimName)) {
					continue
				}
				out = append(out, fmt.Sprintf("workflow %s, condition %q: reads %s over %s, which no context variable picks, so it reads the %s total — every member added up (a 1/0 flag reads 3 for three scenarios). "+
					"Give %s a total rule (update_dimension agg_rule \"first\", the member a total stands for), or read one member: LOOKUP(%s, %s, \"<member>\")",
					def.Name, label, metric, dimLabel, dimLabel, dimName, metric, dimName))
			}
			rows.Close()
		}
	}
	return out
}

// ScenarioTotalWarnings names each scenario-like dimension a plan creates
// that adds its members up (the default): a total over it — a KPI tile, a
// year view, a workflow condition — reads the scenarios added together.
func ScenarioTotalWarnings(steps []ProposalStep) []string {
	var out []string
	for i, step := range steps {
		if step.Tool != "create_dimension" {
			continue
		}
		var p struct {
			Name    string `json:"name"`
			Label   string `json:"label"`
			AggRule string `json:"agg_rule"`
		}
		if json.Unmarshal(step.Params, &p) != nil || !scenarioLike.MatchString(p.Name+" "+p.Label) || (p.AggRule != "" && p.AggRule != "sum") {
			continue
		}
		out = append(out, fmt.Sprintf("step %d (create_dimension %s): its members are alternatives, and a dimension adds its members up by default — every total over it (a KPI tile, a workflow condition, a summary grid) would read them added together. "+
			"Give it agg_rule \"first\" (its total reads the first member, e.g. Base) or \"none\"", i+1, p.Name))
	}
	return out
}
