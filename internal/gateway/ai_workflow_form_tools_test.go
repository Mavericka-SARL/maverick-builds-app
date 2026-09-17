// Tests the AI Assistant's workflow/form proposal tools end to end through
// the real propose -> confirm -> promote HTTP path (bypassing the LLM call
// itself, the same way ai_promote_draft_test.go does), closing the "workflow
// and form proposal tools" bullet of the AI assistant completion item.
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/workflow"
)

func TestConfirmProposal_CreateAndUpdateWorkflowDefLandsAfterPromote(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()

	chatStore := aiassistant.NewChatStore(f.pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	createParams, _ := json.Marshal(map[string]any{"name": "Manager Approval", "trigger_event": "manual"})
	pStore := aiassistant.NewProposalStore(f.pool)
	proposal, err := pStore.CreateProposal(ctx, sess.ID, []aiassistant.ProposalStep{
		{Tool: "create_workflow_def", Description: "Create workflow 'Manager Approval'", Params: createParams},
	})
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}

	status, body := f.do(t, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+proposal.ID+"/confirm", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("confirm: status %d, body %s", status, body)
	}
	var confirmResp struct {
		Proposal struct {
			Steps []struct {
				Status    string `json:"status"`
				Result    string `json:"result"`
				CreatedID string `json:"created_id"`
			} `json:"steps"`
		} `json:"proposal"`
		Session struct {
			DraftRevisionID string `json:"draft_revision_id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(body, &confirmResp); err != nil {
		t.Fatalf("parse confirm response: %v", err)
	}
	if len(confirmResp.Proposal.Steps) != 1 || confirmResp.Proposal.Steps[0].Status != "success" {
		t.Fatalf("expected create_workflow_def to succeed through the real confirm endpoint, got: %+v (this is exactly the created_by/updated_by NULLIF gap NewWriteExecutorWithActor exists to close)", confirmResp.Proposal.Steps)
	}
	wfID := confirmResp.Proposal.Steps[0].CreatedID
	if wfID == "" {
		t.Fatal("create_workflow_def did not report a created id")
	}
	draftRevID := confirmResp.Session.DraftRevisionID
	if draftRevID == "" {
		t.Fatal("confirm did not create/report a draft revision")
	}

	// A second proposal in the same session adds steps to the workflow just
	// created — exercises update_workflow_def against a same-session id.
	updateParams, _ := json.Marshal(map[string]any{
		"workflow_def_id": wfID,
		"steps": []map[string]any{
			{"id": "s1", "name": "Approve", "type": "approval", "routes": map[string]string{"approve": "end-completed", "reject": "end-rejected"}},
		},
	})
	proposal2, err := pStore.CreateProposal(ctx, sess.ID, []aiassistant.ProposalStep{
		{Tool: "update_workflow_def", Description: "Add approval step", Params: updateParams},
	})
	if err != nil {
		t.Fatalf("create proposal 2: %v", err)
	}
	status, body = f.do(t, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+proposal2.ID+"/confirm", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("confirm 2: status %d, body %s", status, body)
	}

	// Promote: the draft (including the AI-authored workflow) becomes active.
	status, body = f.do(t, "POST", "/api/ai/sessions/"+sess.ID+"/promote-draft", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("promote: status %d, body %s", status, body)
	}

	ws := workflow.NewStore(f.pool)
	def, err := ws.GetWorkflowDefFull(ctx, wfID)
	if err != nil {
		t.Fatalf("load created workflow: %v", err)
	}
	if def.Status != "draft" {
		t.Errorf("workflow status = %q, want draft — an AI-created workflow must stay inert until a human publishes it, even after its revision is promoted", def.Status)
	}
	var steps []json.RawMessage
	if err := json.Unmarshal(def.Steps, &steps); err != nil {
		t.Fatalf("steps not valid JSON: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("workflow has %d step(s), want 1 (the update_workflow_def step)", len(steps))
	}

	var createdBy string
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(created_by::text,'') FROM workflow.workflow_def WHERE id=$1::uuid`, wfID).Scan(&createdBy); err != nil {
		t.Fatalf("query created_by: %v", err)
	}
	if createdBy != f.devID {
		t.Errorf("created_by = %q, want the confirming user %q", createdBy, f.devID)
	}
}

// TestConfirmProposal_UpdateWorkflowDefEditsPreSessionWorkflow proves Batch
// 0's createRevision copy extension actually enables what it was built for:
// without copying workflow_def into the AI's draft, update_workflow_def
// could only ever find workflows created earlier in the SAME session, never
// ones that predate it (every AI draft is an isolated copy, never the live
// revision).
//
// The copy assigns the copied row a brand-new UUID (name-joined remap, like
// every other entity createRevision copies) — so the real flow is two
// round trips, exactly as prompt.go instructs the model: (1) confirming ANY
// proposal lazily creates the draft and copies the pre-session workflow into
// it, (2) a follow-up list_workflows call (now scoped to the draft) is how
// the model would discover the COPY's id, never the original. This test
// models that same two-step lookup rather than the original id.
func TestConfirmProposal_UpdateWorkflowDefEditsPreSessionWorkflow(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()

	ws := workflow.NewStore(f.pool)
	preSession, err := ws.CreateWorkflowDefFull(ctx, f.appID, f.workingRevID, "Expense Approval", "original", "manual", f.devID)
	if err != nil {
		t.Fatalf("seed pre-session workflow: %v", err)
	}

	chatStore := aiassistant.NewChatStore(f.pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Step 1: confirm an unrelated proposal to lazily materialize the draft —
	// createRevision's copy step brings "Expense Approval" along under a new id.
	dimParams, _ := json.Marshal(map[string]any{"name": "Unrelated"})
	pStore := aiassistant.NewProposalStore(f.pool)
	proposal1, err := pStore.CreateProposal(ctx, sess.ID, []aiassistant.ProposalStep{
		{Tool: "create_dimension", Description: "Create dimension 'Unrelated'", Params: dimParams},
	})
	if err != nil {
		t.Fatalf("create proposal 1: %v", err)
	}
	status, body := f.do(t, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+proposal1.ID+"/confirm", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("confirm 1: status %d, body %s", status, body)
	}
	var confirmResp1 struct {
		Session struct {
			DraftRevisionID string `json:"draft_revision_id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(body, &confirmResp1); err != nil {
		t.Fatalf("parse confirm 1 response: %v", err)
	}
	draftRevID := confirmResp1.Session.DraftRevisionID
	if draftRevID == "" {
		t.Fatal("confirm did not create/report a draft revision")
	}

	// Step 2: look up the copy's id the same way list_workflows would report
	// it to the model (scoped to the draft revision).
	var copiedID string
	if err := f.pool.QueryRow(ctx, `
		SELECT id::text FROM workflow.workflow_def
		WHERE application_id=$1::uuid AND revision_id=$2::uuid AND name='Expense Approval'
	`, f.appID, draftRevID).Scan(&copiedID); err != nil {
		t.Fatalf("query copied workflow in draft: %v", err)
	}
	if copiedID == preSession.ID {
		t.Fatal("copied workflow kept the original id — the copy step should have assigned a new one")
	}

	updateParams, _ := json.Marshal(map[string]any{
		"workflow_def_id": copiedID,
		"description":     "updated by AI",
	})
	proposal2, err := pStore.CreateProposal(ctx, sess.ID, []aiassistant.ProposalStep{
		{Tool: "update_workflow_def", Description: "Update 'Expense Approval' description", Params: updateParams},
	})
	if err != nil {
		t.Fatalf("create proposal 2: %v", err)
	}
	status, body = f.do(t, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+proposal2.ID+"/confirm", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("confirm 2: status %d, body %s", status, body)
	}
	var confirmResp2 struct {
		Proposal struct {
			Steps []struct {
				Status string `json:"status"`
				Result string `json:"result"`
			} `json:"steps"`
		} `json:"proposal"`
	}
	if err := json.Unmarshal(body, &confirmResp2); err != nil {
		t.Fatalf("parse confirm 2 response: %v", err)
	}
	if len(confirmResp2.Proposal.Steps) != 1 || confirmResp2.Proposal.Steps[0].Status != "success" {
		t.Fatalf("expected update_workflow_def to succeed against the copied pre-session workflow, got: %+v", confirmResp2.Proposal.Steps)
	}

	// The ORIGINAL row (still in the working revision) must be untouched —
	// AI writes stay isolated in the draft copy, exactly like every other
	// write tool.
	var originalDesc string
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(description,'') FROM workflow.workflow_def WHERE id=$1::uuid`, preSession.ID).Scan(&originalDesc); err != nil {
		t.Fatalf("query original workflow: %v", err)
	}
	if originalDesc != "original" {
		t.Errorf("original workflow's description = %q, want unchanged 'original' — the AI must never write to the working revision directly", originalDesc)
	}

	var copiedDesc string
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(description,'') FROM workflow.workflow_def WHERE id=$1::uuid`, copiedID).Scan(&copiedDesc); err != nil {
		t.Fatalf("query copied workflow: %v", err)
	}
	if copiedDesc != "updated by AI" {
		t.Errorf("copied workflow's description in the draft = %q, want 'updated by AI'", copiedDesc)
	}
}

func TestConfirmProposal_CreateFormDefLandsAfterPromote(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()

	chatStore := aiassistant.NewChatStore(f.pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	createParams, _ := json.Marshal(map[string]any{
		"name": "expense_request", "label": "Expense Request",
		"fields": []map[string]any{
			{"name": "amount", "label": "Amount", "type": "number", "required": true},
		},
	})
	pStore := aiassistant.NewProposalStore(f.pool)
	proposal, err := pStore.CreateProposal(ctx, sess.ID, []aiassistant.ProposalStep{
		{Tool: "create_form_def", Description: "Create form 'expense_request'", Params: createParams},
	})
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}

	status, body := f.do(t, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+proposal.ID+"/confirm", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("confirm: status %d, body %s", status, body)
	}
	var confirmResp struct {
		Proposal struct {
			Steps []struct {
				Status string `json:"status"`
				Result string `json:"result"`
			} `json:"steps"`
		} `json:"proposal"`
	}
	if err := json.Unmarshal(body, &confirmResp); err != nil {
		t.Fatalf("parse confirm response: %v", err)
	}
	if len(confirmResp.Proposal.Steps) != 1 || confirmResp.Proposal.Steps[0].Status != "success" {
		t.Fatalf("expected create_form_def to succeed through the real confirm endpoint, got: %+v", confirmResp.Proposal.Steps)
	}

	status, body = f.do(t, "POST", "/api/ai/sessions/"+sess.ID+"/promote-draft", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("promote: status %d, body %s", status, body)
	}

	activeRev := activeRevisionID(t, f.pool, f.modelID)
	var fieldsJSON []byte
	if err := f.pool.QueryRow(ctx, `
		SELECT fields FROM model.form_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='expense_request'
	`, f.modelID, activeRev).Scan(&fieldsJSON); err != nil {
		t.Fatalf("query created form in active revision: %v", err)
	}
	var fields []map[string]any
	_ = json.Unmarshal(fieldsJSON, &fields)
	if len(fields) != 1 {
		t.Fatalf("fields = %s, want 1 field", fieldsJSON)
	}
}
