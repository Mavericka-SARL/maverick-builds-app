// Tests that a workflow's notification step actually delivers to its
// configured recipient — closing a gap where the Designer's Recipient/
// Subject/Message fields (WorkflowsTab.tsx) were saved into the step JSON
// but the runtime (activateNextSteps in internal/workflow/store.go) never
// read them: it just auto-completed the step with a hardcoded "Auto-
// dispatched" comment, so nobody was ever actually notified regardless of
// what "Recipient" said. dispatchStepNotification/resolveNotificationRecipients
// now resolve the configured target and write a real notification.notification
// row (the same table /api/notifications reads), using the identical dual
// role-matching scheme as step assignee_roles: a platform identity.user_role
// value via role_assignment, or an identity.business_role name via
// business_role_member.
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type notifFixture struct {
	pool *pgxpool.Pool

	appID, wsID string

	requesterID          string // starts the workflow — the "requester" recipient target
	reviewerAID          string // holds business_role "Ops Reviewers"
	reviewerBID          string // holds business_role "Ops Reviewers"
	businessAdminID      string // holds platform role business_admin
	outsiderID           string // matches nothing — negative control
	opsReviewersRoleName string
}

func setupNotifFixture(t *testing.T) *notifFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	f := &notifFixture{pool: pool, opsReviewersRoleName: "Ops Reviewers"}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('NotifCo', 'enterprise') RETURNING id::text`)
	f.wsID = q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	f.appID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Notif App', 'planning') RETURNING id::text`, f.wsID, custID)

	f.requesterID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('notif-requester', 'requester@notif.co', 'Requester', $1::uuid) RETURNING id::text`, custID)
	f.reviewerAID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('notif-reviewer-a', 'reviewer-a@notif.co', 'Reviewer A', $1::uuid) RETURNING id::text`, custID)
	f.reviewerBID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('notif-reviewer-b', 'reviewer-b@notif.co', 'Reviewer B', $1::uuid) RETURNING id::text`, custID)
	f.businessAdminID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('notif-badmin', 'badmin@notif.co', 'Biz Admin', $1::uuid) RETURNING id::text`, custID)
	f.outsiderID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('notif-outsider', 'outsider@notif.co', 'Outsider', $1::uuid) RETURNING id::text`, custID)

	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_admin', $2::uuid)`, f.businessAdminID, f.wsID)

	roleID := q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, f.wsID, f.opsReviewersRoleName)
	exec(`INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, roleID, f.reviewerAID)
	exec(`INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, roleID, f.reviewerBID)

	return f
}

// mkNotifySteps builds a two-step workflow: a role-open task (anyone can
// complete it) that routes into a notification step configured per the
// given recipient type/role.
func mkNotifySteps(recipientType, recipientRole string) json.RawMessage {
	cfg := map[string]string{"recipient_type": recipientType, "subject": "Test Subject", "message": "Test Message"}
	if recipientRole != "" {
		cfg["recipient_role"] = recipientRole
	}
	cfgJSON, _ := json.Marshal(cfg)
	stepsJSON := `[
		{"id":"step-task","name":"Do Thing","type":"task","assignee_roles":[],"routes":{"next":"step-notify"}},
		{"id":"step-notify","name":"Notify","type":"notification","notification":` + string(cfgJSON) + `,"routes":{"next":"end-completed"}}
	]`
	return json.RawMessage(stepsJSON)
}

// runToNotify creates+publishes a workflow with the given notification
// config, starts it as f.requesterID, completes the open task step, and
// returns the instance ID (by which point step-notify has been dispatched).
func (f *notifFixture) runToNotify(t *testing.T, store *workflow.Store, recipientType, recipientRole string) string {
	t.Helper()
	ctx := context.Background()

	def, err := store.CreateWorkflowDefFull(ctx, f.appID, "", "Notify Test "+t.Name(), "", "manual", f.requesterID)
	if err != nil {
		t.Fatalf("create def: %v", err)
	}
	if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, def.Name, def.Description, def.TriggerEvent, "", f.requesterID, mkNotifySteps(recipientType, recipientRole), nil, nil); err != nil {
		t.Fatalf("set steps: %v", err)
	}
	if _, err := store.PublishWorkflowDef(ctx, def.ID, f.requesterID); err != nil {
		t.Fatalf("publish: %v", err)
	}

	inst, err := store.StartWorkflow(ctx, def.ID, f.requesterID, nil)
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	_, steps, err := store.GetWorkflowInstance(ctx, inst.Id)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	var taskStepID string
	for _, s := range steps {
		if s.StepDefId == "step-task" {
			taskStepID = s.Id
		}
	}
	if taskStepID == "" {
		t.Fatal("step-task not found in instance")
	}
	if _, err := store.CompleteStep(ctx, taskStepID, f.requesterID, "done", ""); err != nil {
		t.Fatalf("complete task step: %v", err)
	}
	return inst.Id
}

func notifRowsFor(t *testing.T, pool *pgxpool.Pool, userID string) (count int, subject, message, resourceType, resourceID string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT template_vars, COALESCE(resource_type,''), COALESCE(resource_id,'') FROM notification.notification
		WHERE recipient_user_id = $1::uuid AND template_id = 'workflow_step_notification'`, userID)
	if err != nil {
		t.Fatalf("query notifications: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var varsJSON []byte
		if err := rows.Scan(&varsJSON, &resourceType, &resourceID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		count++
		var vars map[string]string
		_ = json.Unmarshal(varsJSON, &vars)
		subject, message = vars["subject"], vars["message"]
	}
	return count, subject, message, resourceType, resourceID
}

func TestNotificationStepNotifiesRequester(t *testing.T) {
	f := setupNotifFixture(t)
	store := workflow.NewStore(f.pool)

	instID := f.runToNotify(t, store, "requester", "")

	n, subject, message, resourceType, resourceID := notifRowsFor(t, f.pool, f.requesterID)
	if n != 1 {
		t.Fatalf("notifications for requester = %d, want 1", n)
	}
	if subject != "Test Subject" || message != "Test Message" {
		t.Errorf("subject/message = %q/%q, want the configured values", subject, message)
	}
	if resourceType != "workflow_instance" || resourceID != instID {
		t.Errorf("resource_type/resource_id = %q/%q, want workflow_instance/%s", resourceType, resourceID, instID)
	}

	var decision, status string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT status::text, COALESCE(decision,'') FROM workflow.workflow_step
		WHERE instance_id=$1::uuid AND step_def_id='step-notify'`, instID).Scan(&status, &decision); err != nil {
		t.Fatalf("load notify step: %v", err)
	}
	if status != "completed" || decision != "sent" {
		t.Errorf("notify step = %s/%s, want completed/sent", status, decision)
	}

	// Nobody else was notified.
	if n, _, _, _, _ := notifRowsFor(t, f.pool, f.outsiderID); n != 0 {
		t.Errorf("outsider received %d notification(s), want 0", n)
	}
}

func TestNotificationStepNotifiesRole(t *testing.T) {
	// Each subtest gets its own fixture/DB — sharing one fixture across
	// subtests would leak recipients between them (reviewer A's
	// notification from one subtest would still be sitting in the table
	// when the next subtest checks for its absence).
	t.Run("named business_role reaches every member", func(t *testing.T) {
		f := setupNotifFixture(t)
		store := workflow.NewStore(f.pool)
		instID := f.runToNotify(t, store, "role", f.opsReviewersRoleName)

		for name, userID := range map[string]string{"reviewer A": f.reviewerAID, "reviewer B": f.reviewerBID} {
			n, _, _, resourceType, resourceID := notifRowsFor(t, f.pool, userID)
			if n != 1 {
				t.Errorf("%s notifications = %d, want 1", name, n)
			}
			if resourceType != "workflow_instance" || resourceID != instID {
				t.Errorf("%s resource_type/resource_id = %q/%q, want workflow_instance/%s", name, resourceType, resourceID, instID)
			}
		}
		if n, _, _, _, _ := notifRowsFor(t, f.pool, f.requesterID); n != 0 {
			t.Errorf("requester received %d notification(s) for a role-targeted step, want 0", n)
		}
	})

	t.Run("platform role name matches role_assignment, same as assignee_roles", func(t *testing.T) {
		f := setupNotifFixture(t)
		store := workflow.NewStore(f.pool)
		f.runToNotify(t, store, "role", "business_admin")

		if n, _, _, _, _ := notifRowsFor(t, f.pool, f.businessAdminID); n != 1 {
			t.Errorf("business_admin notifications = %d, want 1", n)
		}
		if n, _, _, _, _ := notifRowsFor(t, f.pool, f.reviewerAID); n != 0 {
			t.Errorf("reviewer A (not business_admin) received %d notification(s), want 0", n)
		}
	})
}

func TestNotificationStepSkipsUnresolvableRecipient(t *testing.T) {
	f := setupNotifFixture(t)
	store := workflow.NewStore(f.pool)

	instID := f.runToNotify(t, store, "role", "Nonexistent Role")

	var totalNotifs int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM notification.notification`).Scan(&totalNotifs); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	if totalNotifs != 0 {
		t.Errorf("notifications sent = %d, want 0 for an unresolvable recipient role", totalNotifs)
	}

	// The step must not hang forever waiting for a recipient that will
	// never resolve — it completes (truthfully marked, not claiming
	// delivery) and the instance still reaches a terminal state.
	var status, decision string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT status::text, COALESCE(decision,'') FROM workflow.workflow_step
		WHERE instance_id=$1::uuid AND step_def_id='step-notify'`, instID).Scan(&status, &decision); err != nil {
		t.Fatalf("load notify step: %v", err)
	}
	if status != "completed" || decision != "skipped" {
		t.Errorf("notify step = %s/%s, want completed/skipped", status, decision)
	}

	var instStatus string
	if err := f.pool.QueryRow(context.Background(), `SELECT status::text FROM workflow.workflow_instance WHERE id=$1::uuid`, instID).Scan(&instStatus); err != nil {
		t.Fatalf("load instance: %v", err)
	}
	if instStatus != "completed" {
		t.Errorf("instance status = %q, want completed (must not hang on an unresolvable notification)", instStatus)
	}
}

// TestWorkflowMyHistoryIncludesRoleMatchedNotificationRecipient closes the
// gap where a role-matched notification recipient (neither the requester
// nor a step assignee) would not find the instance they were notified
// about in their own GET /api/workflow/my-history — "navigate to workflow"
// from the notification center would silently land on nothing for this
// real, already-shipped recipient type.
func TestWorkflowMyHistoryIncludesRoleMatchedNotificationRecipient(t *testing.T) {
	f := setupNotifFixture(t)
	store := workflow.NewStore(f.pool)
	instID := f.runToNotify(t, store, "role", f.opsReviewersRoleName)

	// Sanity: reviewer A is neither the requester nor a step assignee for
	// this instance (step-task, the only completed step, has assignee_roles
	// []  and was completed by the requester in runToNotify).
	var assigneeCount int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM workflow.workflow_step
		WHERE instance_id=$1::uuid AND assignee_user_id=$2::uuid`, instID, f.reviewerAID).Scan(&assigneeCount); err != nil {
		t.Fatalf("check assignee: %v", err)
	}
	if assigneeCount != 0 {
		t.Fatalf("reviewer A unexpectedly assigned a step — test fixture assumption broken")
	}

	t.Setenv("DEV_MODE", "true")
	srv := httptest.NewServer(NewHandler(logger.New("test"), f.pool, nil))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/api/workflow/my-history", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", "notif-reviewer-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("my-history: status=%d", resp.StatusCode)
	}
	var instances []historyInstance
	if err := json.NewDecoder(resp.Body).Decode(&instances); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, inst := range instances {
		if inst.ID == instID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("role-matched notification recipient's my-history does not include instance %s they were notified about", instID)
	}
}
