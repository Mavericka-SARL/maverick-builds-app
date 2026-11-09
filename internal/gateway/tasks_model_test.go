package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// Each Inbox task names the model its run works on, and its application:
// the Inbox lists every application's tasks (owner, 2026-11-09).
func TestInboxTasksNameTheirModel(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	store := workflow.NewStore(f.pool)
	def, err := store.CreateWorkflowDefFull(ctx, f.appID, f.workingRevID, "Close "+t.Name(), "", "manual", f.approverID)
	if err != nil {
		t.Fatal(err)
	}
	steps := json.RawMessage(`[{"id":"load","name":"Load GL postings","type":"task","assignee_roles":["business_user"],"routes":{"next":"end-completed"}}]`)
	if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, def.Name, "", "manual", "", f.approverID, steps, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishWorkflowDef(ctx, def.ID, f.approverID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartWorkflow(ctx, def.ID, f.managerID, nil); err != nil {
		t.Fatal(err)
	}
	var modelName string
	if err := f.pool.QueryRow(ctx, `SELECT name FROM core.model WHERE id=$1::uuid`, f.modelID).Scan(&modelName); err != nil {
		t.Fatal(err)
	}
	status, raw := doAs(t, f.rollupFixture, "GET", "/api/tasks", "test-manager", f.appID, nil)
	if status != http.StatusOK {
		t.Fatalf("tasks: %d %s", status, raw)
	}
	var tasks []struct {
		StepName      string `json:"step_name"`
		ModelID       string `json:"model_id"`
		ModelName     string `json:"model_name"`
		ApplicationID string `json:"application_id"`
	}
	_ = json.Unmarshal([]byte(raw), &tasks)
	found := false
	for _, tk := range tasks {
		if tk.StepName != "Load GL postings" {
			continue
		}
		found = true
		if tk.ModelID != f.modelID || tk.ModelName != modelName || tk.ApplicationID != f.appID {
			t.Errorf("task's model = %q %q, application %q; want %q %q, %q", tk.ModelID, tk.ModelName, tk.ApplicationID, f.modelID, modelName, f.appID)
		}
	}
	if !found {
		t.Fatalf("the task is not in the manager's inbox: %s", raw)
	}
}
