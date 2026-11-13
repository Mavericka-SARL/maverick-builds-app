package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// A session is its draft (owner, 2026-11-13): a new session starts a fresh
// draft, so it takes over an earlier session's unfinished task plan only when
// that session's draft became the working revision — then the work the plan
// did is in what the new draft copies. A confirmed plan moves the current
// stage; a finished plan is not carried.
func TestNewSessionsCarryThePlanOfAPromotedDraft(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()
	chat := aiassistant.NewChatStore(f.pool)
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

	// A new session starts without a draft, and an open draft's plan stays
	// with its session.
	s2 := newSession("")
	if s2.DraftRevisionID != "" {
		t.Fatalf("s2 starts in draft %s, want none", s2.DraftRevisionID)
	}
	if p, _ := chat.TaskPlan(ctx, s2.ID); p != nil {
		t.Fatalf("s2 took the plan of s1, whose draft %s is still open: %+v", d1, p)
	}

	// Once s1's draft is the working revision, the next session takes its plan.
	if status, body := f.do(t, "POST", "/api/ai/sessions/"+s1.ID+"/promote-draft", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("promote: %d %s", status, body)
	}
	s3 := newSession("")
	carried, _ := chat.TaskPlan(ctx, s3.ID)
	if carried == nil || carried.CarriedFrom != s1.ID || carried.Stages[0].Confirmed != 1 {
		t.Fatalf("s3's plan = %+v, want s1's carried over", carried)
	}
	if ids, err := chat.EarlierSessions(ctx, s3.ID, carried.CarriedFrom); err != nil || len(ids) != 1 || ids[0] != s1.ID {
		t.Errorf("earlier sessions of s3 = %v (%v), want [s1]", ids, err)
	}
	if d3 := confirm(s3.ID, "Product"); d3 == "" || d3 == d1 {
		t.Fatalf("s3's first plan made draft %q, want a fresh one (s1's was %s)", d3, d1)
	}

	// A finished plan is not carried.
	for i := range carried.Stages {
		carried.Stages[i].Status = aiassistant.StageDone
	}
	_ = chat.SetTaskPlan(ctx, s3.ID, carried)
	if status, body := f.do(t, "POST", "/api/ai/sessions/"+s3.ID+"/promote-draft", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("promote s3: %d %s", status, body)
	}
	s4 := newSession("")
	if p, _ := chat.TaskPlan(ctx, s4.ID); p != nil && len(p.Stages) > 0 {
		t.Errorf("a finished plan was carried: %+v", p)
	}
}
