package gateway

// A workflow whose subject is a table of records drives the status of the
// record it runs on: submitting the record starts it (a form_submit rule),
// the approver's decision approves or rejects the record — which posts it,
// or not, through the table's mappings — and cancelling the run sends the
// record back to its creator as a draft.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWorkflowDrivesTheRecordStatus(t *testing.T) {
	f := setupRecordFixture(t)
	ctx := context.Background()
	mappingID, metricID := f.mapRecordsToMetric("approved")

	var wfID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO workflow.workflow_def (application_id, revision_id, name, trigger_event, steps, status, published_at, subject_type, subject_config)
		VALUES ($1::uuid, $2::uuid, 'Expense approval', 'form.submit', $3::jsonb, 'published', now(), 'form_record', jsonb_build_object('form_id', $4::text))
		RETURNING id::text`, f.appID, f.revID,
		// The requester is told after the decision: a notification step's
		// "sent" comes last, and decides nothing.
		`[{"id":"approve","name":"Approve","type":"approval","assignee_roles":["business_admin"],"routes":{"approve":"notify","reject":"notify"}},
		  {"id":"notify","name":"Tell the requester","type":"notification","notification":{"recipient_type":"requester","subject":"Decided","message":"Your expense was decided."},"routes":{"next":"end-completed"}}]`,
		f.formID).Scan(&wfID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO workflow.automation_rule (application_id, revision_id, name, trigger_type, workflow_name, workflow_def_id, enabled, source_form_id)
		VALUES ($1::uuid, $2::uuid, 'On submit', 'form_submit', 'Expense approval', $3::uuid, true, $4::uuid)`,
		f.appID, f.revID, wfID, f.formID); err != nil {
		t.Fatal(err)
	}

	status := func(rec string) string {
		var s string
		_ = f.pool.QueryRow(ctx, `SELECT COALESCE(status::text, '') FROM runtime.form_record WHERE id=$1::uuid`, rec).Scan(&s)
		return s
	}
	waitStatus := func(rec, want string) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); status(rec) != want; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("record %s is %q, want %q", rec, status(rec), want)
			}
		}
	}
	// submit makes a draft, submits it, and returns it with the approval
	// task the run opened on it.
	submit := func(amount float64) (rec, task, instance string) {
		t.Helper()
		rec = f.create(prOwner, "draft", amount)
		f.expect(http.StatusOK, http.MethodPut, "/api/records/"+rec, prOwner, map[string]any{"status": "submitted"})
		for deadline := time.Now().Add(10 * time.Second); task == ""; time.Sleep(50 * time.Millisecond) {
			_ = f.pool.QueryRow(ctx, `
				SELECT ws.id::text, wi.id::text FROM workflow.workflow_step ws
				JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
				WHERE wi.workflow_def_id = $1::uuid AND wi.context->>'record_id' = $2 AND ws.status = 'in_progress'`,
				wfID, rec).Scan(&task, &instance)
			if time.Now().After(deadline) {
				t.Fatalf("submitting record %s started no run", rec)
			}
		}
		return rec, task, instance
	}

	t.Run("approval approves and posts the record", func(t *testing.T) {
		rec, task, _ := submit(40)
		if status(rec) != "submitted" {
			t.Fatalf("record under review is %q, want submitted", status(rec))
		}
		f.waitPosted("before the decision", mappingID, metricID, 0, 0)
		f.expect(http.StatusOK, http.MethodPost, "/api/tasks/"+task+"/complete", prBA, map[string]any{"decision": "approve"})
		waitStatus(rec, "approved")
		f.waitPosted("after the approval", mappingID, metricID, 1, 40)
	})

	t.Run("rejection rejects the record", func(t *testing.T) {
		rec, task, _ := submit(7)
		f.expect(http.StatusOK, http.MethodPost, "/api/tasks/"+task+"/complete", prBA, map[string]any{"decision": "reject", "comment": "no receipt"})
		waitStatus(rec, "rejected")
		f.waitPosted("after the rejection", mappingID, metricID, 1, 40)
	})

	t.Run("an approval is refused when the creator has lost access where the record posts", func(t *testing.T) {
		rec, task, _ := submit(11)
		f.expect(http.StatusOK, http.MethodPut, "/api/business-admin/users/"+f.userID[prOwner]+"/access-rules", prBA,
			map[string]any{"rules": []map[string]string{{"rule_type": "metric", "ref_id": metricID, "access": "read"}}})
		raw := f.expect(http.StatusForbidden, http.MethodPost, "/api/tasks/"+task+"/complete", prBA, map[string]any{"decision": "approve"})
		if !strings.Contains(string(raw), "cannot be approved") || !strings.Contains(string(raw), "read-only for the record's creator") {
			t.Errorf("the refusal does not say why: %s", raw)
		}
		if got := status(rec); got != "submitted" {
			t.Errorf("after the refused approval the record is %q, want submitted", got)
		}
		var stepStatus string
		_ = f.pool.QueryRow(ctx, `SELECT status::text FROM workflow.workflow_step WHERE id = $1::uuid`, task).Scan(&stepStatus)
		if stepStatus != "in_progress" {
			t.Errorf("the refused approval's step is %q, want still in_progress", stepStatus)
		}
		// Rejecting it is still possible; and with access back, approving.
		f.expect(http.StatusOK, http.MethodPut, "/api/business-admin/users/"+f.userID[prOwner]+"/access-rules", prBA,
			map[string]any{"rules": []map[string]string{}})
		f.expect(http.StatusOK, http.MethodPost, "/api/tasks/"+task+"/complete", prBA, map[string]any{"decision": "approve"})
		waitStatus(rec, "approved")
		f.waitPosted("after the approval with access back", mappingID, metricID, 2, 51)
	})

	t.Run("a cancelled run sends the record back as a draft", func(t *testing.T) {
		rec, _, instance := submit(3)
		f.expect(http.StatusOK, http.MethodPost, "/api/workflow/instances/"+instance+"/cancel", prBA, map[string]any{"reason": "filed twice"})
		waitStatus(rec, "draft")
	})

	t.Run("cancelling one of two runs on a record leaves it under review", func(t *testing.T) {
		// A second rule on the same event starts a second run on each record.
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO workflow.automation_rule (application_id, revision_id, name, trigger_type, workflow_name, workflow_def_id, enabled, source_form_id)
			VALUES ($1::uuid, $2::uuid, 'On submit, again', 'form_submit', 'Expense approval', $3::uuid, true, $4::uuid)`,
			f.appID, f.revID, wfID, f.formID); err != nil {
			t.Fatal(err)
		}
		rec, _, _ := submit(2)
		var runs []string
		for deadline := time.Now().Add(10 * time.Second); len(runs) < 2; time.Sleep(50 * time.Millisecond) {
			runs = runs[:0]
			rows, err := f.pool.Query(ctx, `SELECT id::text FROM workflow.workflow_instance WHERE context->>'record_id' = $1 AND status = 'running' ORDER BY started_at`, rec)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var id string
				_ = rows.Scan(&id)
				runs = append(runs, id)
			}
			rows.Close()
			if time.Now().After(deadline) {
				t.Fatalf("record %s has %d open run(s), want 2", rec, len(runs))
			}
		}
		f.expect(http.StatusOK, http.MethodPost, "/api/workflow/instances/"+runs[0]+"/cancel", prBA, map[string]any{"reason": "duplicate"})
		time.Sleep(300 * time.Millisecond)
		if got := status(rec); got != "submitted" {
			t.Errorf("after cancelling one of its two runs the record is %q, want submitted", got)
		}
		f.expect(http.StatusOK, http.MethodPost, "/api/workflow/instances/"+runs[1]+"/cancel", prBA, map[string]any{"reason": "withdrawn"})
		waitStatus(rec, "draft")
	})
}
