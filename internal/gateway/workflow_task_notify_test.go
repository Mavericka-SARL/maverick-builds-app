package gateway

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/notification"
	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// The people a task or approval becomes the task of are told so: when the
// run starts, when the step before is decided, and again when a rejection
// sends the work back. The person who acted is left out, and a test run
// tells no one. Until 2026-11-20 none of them was told anything, in the
// console or by e-mail (OBSERVATIONS: "A person given an approval or task is
// told nothing").
func TestNewTasksNotifyTheirAssignees(t *testing.T) {
	f := setupNotifFixture(t)
	ctx := context.Background()
	store := workflow.NewStore(f.pool)

	// E-mail on for the tenant: each in-app notice gets its e-mail twin.
	var custID string
	if err := f.pool.QueryRow(ctx, `SELECT customer_id::text FROM core.workspace WHERE id = $1::uuid`, f.wsID).Scan(&custID); err != nil {
		t.Fatal(err)
	}
	if _, err := notification.NewStore(f.pool).UpdateSettings(ctx, custID, notification.Settings{EmailEnabled: true}); err != nil {
		t.Fatalf("turn e-mail on: %v", err)
	}

	steps := json.RawMessage(`[
		{"id":"prep","name":"Prepare the figures","type":"task","assignee_roles":["` + f.opsReviewersRoleName + `"],"routes":{"next":"approve"}},
		{"id":"approve","name":"Approve the figures","type":"approval","assignee_roles":["business_admin"],"routes":{"approve":"end-completed","reject":"prep"}}
	]`)
	def, err := store.CreateWorkflowDefFull(ctx, f.appID, "", "Task notice", "", "manual", f.requesterID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, def.Name, def.Description, def.TriggerEvent, "", f.requesterID, steps, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishWorkflowDef(ctx, def.ID, f.requesterID); err != nil {
		t.Fatal(err)
	}

	type notice struct{ channel, subject string }
	told := func(userID string) []notice {
		t.Helper()
		rows, err := f.pool.Query(ctx, `
			SELECT channel::text, template_vars->>'subject' FROM notification.notification
			WHERE recipient_user_id = $1::uuid AND template_id = $2 ORDER BY created_at, channel`, userID, workflow.TaskAssignedTemplate)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []notice
		for rows.Next() {
			var n notice
			if err := rows.Scan(&n.channel, &n.subject); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return out
	}
	subjects := func(userID, channel string) []string {
		var out []string
		for _, n := range told(userID) {
			if n.channel == channel {
				out = append(out, n.subject)
			}
		}
		sort.Strings(out)
		return out
	}
	stepID := func(instanceID, def string) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, `SELECT id::text FROM workflow.workflow_step WHERE instance_id = $1::uuid AND step_def_id = $2`, instanceID, def).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	// Start: the preparers are told, in the console and by e-mail.
	inst, err := store.StartWorkflow(ctx, def.ID, f.requesterID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{f.reviewerAID, f.reviewerBID} {
		if got := subjects(u, "in_app"); len(got) != 1 || got[0] != "New task: Prepare the figures" {
			t.Errorf("preparer %s in-app after start = %v", u, got)
		}
		if got := subjects(u, "email"); len(got) != 1 {
			t.Errorf("preparer %s e-mails after start = %v, want one", u, got)
		}
	}
	for _, u := range []string{f.requesterID, f.businessAdminID, f.outsiderID} {
		if got := told(u); len(got) != 0 {
			t.Errorf("%s was told %v at the start; only the preparers have work", u, got)
		}
	}

	// Reviewer A prepares: the approver is told; A, who acted, is not.
	if _, err := store.CompleteStep(ctx, stepID(inst.Id, "prep"), f.reviewerAID, "done", ""); err != nil {
		t.Fatal(err)
	}
	if got := subjects(f.businessAdminID, "in_app"); len(got) != 1 || got[0] != "Approval needed: Approve the figures" {
		t.Errorf("approver after preparation = %v", got)
	}
	if got := subjects(f.reviewerAID, "in_app"); len(got) != 1 {
		t.Errorf("reviewer A was told again after deciding: %v", got)
	}

	// Rejected: the work goes back to reviewer A, who did it (the step keeps
	// its assignee), and A is told again; B is not.
	if _, err := store.CompleteStep(ctx, stepID(inst.Id, "approve"), f.businessAdminID, "reject", "Check March"); err != nil {
		t.Fatal(err)
	}
	if got := subjects(f.reviewerAID, "in_app"); len(got) != 2 {
		t.Errorf("reviewer A after rework = %v, want the first notice and a second", got)
	}
	if got := subjects(f.reviewerBID, "in_app"); len(got) != 1 {
		t.Errorf("reviewer B after rework = %v, want only the first notice: the rework is A's", got)
	}
	if got := subjects(f.businessAdminID, "in_app"); len(got) != 1 {
		t.Errorf("the approver, who rejected, was told of the rework: %v", got)
	}

	// A completing request that finds nothing new tells no one twice.
	if n := store.NotifyNewTasks(ctx, inst.Id, ""); n != 0 {
		t.Errorf("a second announcement told %d people", n)
	}

	// A test run tells no one.
	before := len(told(f.reviewerAID))
	if _, err := store.StartTestRun(ctx, def.ID, f.requesterID, nil); err != nil {
		t.Fatal(err)
	}
	if got := told(f.reviewerAID); len(got) != before {
		t.Errorf("a test run told the preparer: %v", got[before:])
	}
}
