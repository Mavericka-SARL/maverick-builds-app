package aiassistant

import (
	"context"
	"encoding/json"
	"fmt"
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
