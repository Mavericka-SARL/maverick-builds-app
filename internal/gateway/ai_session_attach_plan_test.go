package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// A session is its draft (owner, 2026-11-13): a new session starts a fresh
// draft and no plan — a plan of another session is attached only when the
// developer chooses to (attach-plan), with its attachments and their
// analyses if they like. A session whose draft became the working revision
// goes on. A confirmed plan moves the current stage.
func TestNewSessionStartsWithoutAPlan_AttachIsOptional(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()
	chat := aiassistant.NewChatStore(f.pool)
	docs := aiassistant.NewDocumentStore(f.pool)
	pStore := aiassistant.NewProposalStore(f.pool)
	type session struct {
		ID              string `json:"id"`
		DraftRevisionID string `json:"draft_revision_id"`
	}
	newSession := func(query string) session {
		t.Helper()
		status, body := f.do(t, "POST", "/api/ai/sessions"+query, f.devSub, nil)
		var s session
		if status != http.StatusOK || json.Unmarshal(body, &s) != nil {
			t.Fatalf("new session%s: %d %s", query, status, body)
		}
		return s
	}
	confirm := func(sessionID, name string) string {
		t.Helper()
		params, _ := json.Marshal(map[string]string{"name": name})
		p, err := pStore.CreateProposal(ctx, sessionID, []aiassistant.ProposalStep{{Tool: "create_dimension", Description: "Create " + name, Params: params}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+p.ID+"/confirm", f.devSub, nil)
		var resp struct {
			Session session `json:"session"`
		}
		if status != http.StatusOK || json.Unmarshal(body, &resp) != nil {
			t.Fatalf("confirm: %d %s", status, body)
		}
		return resp.Session.DraftRevisionID
	}

	s1 := newSession("")
	plan, err := aiassistant.NewTaskPlan("Build the model", []aiassistant.TaskStage{
		{Title: "Structure", Items: []string{"dimensions"}}, {Title: "P&L", Items: []string{"P&L rows"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := chat.SetTaskPlan(ctx, s1.ID, plan); err != nil {
		t.Fatal(err)
	}
	d1 := confirm(s1.ID, "Region")
	got, _ := chat.TaskPlan(ctx, s1.ID)
	if st := got.Stages[0]; st.Status != aiassistant.StageInProgress || st.Confirmed != 1 || st.ConfirmedSteps != 1 {
		t.Errorf("stage 1 after a confirmed plan: %+v", st)
	}

	// A new session starts without a draft and without a plan.
	s2 := newSession("")
	if s2.DraftRevisionID != "" {
		t.Fatalf("s2 starts in draft %s, want none", s2.DraftRevisionID)
	}
	if p, _ := chat.TaskPlan(ctx, s2.ID); p != nil {
		t.Fatalf("s2 took a plan it was not given: %+v", p)
	}

	// Once s1's draft is the working revision, s1 goes on in a fresh draft,
	// and a new session still starts with no plan.
	if status, body := f.do(t, "POST", "/api/ai/sessions/"+s1.ID+"/promote-draft", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("promote: %d %s", status, body)
	}
	if again := confirm(s1.ID, "Channel"); again == "" || again == d1 {
		t.Fatalf("s1 after promotion confirmed into draft %q, want a fresh one (its promoted draft was %s)", again, d1)
	}
	s3 := newSession("")
	if p, _ := chat.TaskPlan(ctx, s3.ID); p != nil {
		t.Fatalf("s3 took s1's plan automatically: %+v", p)
	}

	// s1's workbook and what s1 learned from it.
	doc, err := docs.CreateDocument(ctx, s1.ID, "model.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "overview", false, []byte("raw"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chat.UpdateMemory(ctx, s1.ID, func(m *aiassistant.SessionMemory) error {
		m.SetAttachment("model.xlsx", doc.ID, "SHEETS: Plan, P&L", "server")
		m.PutRead(aiassistant.MemoryRead{Tool: "read_attached_sheet", Key: "read_attached_sheet|" + doc.ID + "|plan|1|9", File: "model.xlsx", Sheet: "Plan", FromRow: 1, ToRow: 9, Result: "rows"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The developer's plans in other sessions are offered.
	status, body := f.do(t, "GET", "/api/ai/plans?exclude="+s3.ID, f.devSub, nil)
	var refs []aiassistant.PlanRef
	if status != http.StatusOK || json.Unmarshal(body, &refs) != nil || len(refs) != 1 || refs[0].SessionID != s1.ID || refs[0].Index != -1 {
		t.Fatalf("plans offered: %d %s", status, body)
	}

	// Attached, with its attachments: the plan as s1 left it, the file, and
	// its analysis — nothing to go through again.
	status, body = f.do(t, "POST", "/api/ai/sessions/"+s3.ID+"/attach-plan", f.devSub, map[string]any{"session_id": s1.ID, "index": -1, "attachments": true})
	if status != http.StatusOK {
		t.Fatalf("attach-plan: %d %s", status, body)
	}
	attached, _ := chat.TaskPlan(ctx, s3.ID)
	// Stage 1 was confirmed twice in s1: before and after its promotion.
	if attached == nil || attached.CarriedFrom != s1.ID || attached.Stages[0].Confirmed != 2 {
		t.Fatalf("s3's plan = %+v, want s1's as s1 left it", attached)
	}
	if ids, err := chat.EarlierSessions(ctx, s3.ID, attached.CarriedFrom); err != nil || len(ids) != 1 || ids[0] != s1.ID {
		t.Errorf("earlier sessions of s3 = %v (%v), want [s1]", ids, err)
	}
	copied, _ := docs.ListDocuments(ctx, s3.ID)
	mem, _ := chat.Memory(ctx, s3.ID)
	if len(copied) != 1 || mem.Attachment("model.xlsx") == nil || mem.Attachment("model.xlsx").DocumentID != copied[0].ID ||
		len(mem.Reads) != 1 || !strings.Contains(mem.Reads[0].Key, copied[0].ID) {
		t.Fatalf("s3's files %+v, memory %+v — want model.xlsx with its analysis and read under the copy's id", copied, mem)
	}
	if d3 := confirm(s3.ID, "Product"); d3 == "" || d3 == d1 {
		t.Fatalf("s3's first plan made draft %q, want a fresh one (s1's was %s)", d3, d1)
	}

	// The console reads task_plan.stages.length when it opens a session: a
	// decisions-only plan goes out with stages [], never null (it stopped
	// the AI developer tab, 2026-11-13).
	s4 := newSession("")
	_ = chat.SetTaskPlan(ctx, s4.ID, &aiassistant.TaskPlan{Decisions: []string{"store percentages in percent units"}})
	status, body = f.do(t, "GET", "/api/ai/sessions/"+s4.ID, f.devSub, nil)
	var opened struct {
		TaskPlan map[string]json.RawMessage `json:"task_plan"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &opened) != nil {
		t.Fatalf("open s4: %d %s", status, body)
	}
	if got := string(opened.TaskPlan["stages"]); got != "[]" {
		t.Errorf("s4's task_plan.stages = %s, want []", got)
	}
}
