package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// Sessions carry the work on: a new session continues the draft the console
// shows when it is open, never one an activated revision superseded; it
// takes over its predecessor's unfinished task plan, and a confirmed plan
// moves that plan's current stage.
func TestNewSessionsCarryTheDraftThePlanAndTheChecks(t *testing.T) {
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

	// The next session continues the draft and takes the unfinished plan.
	s2 := newSession("")
	if s2.DraftRevisionID != d1 {
		t.Fatalf("s2 works in %q, want the open draft %s", s2.DraftRevisionID, d1)
	}
	carried, _ := chat.TaskPlan(ctx, s2.ID)
	if carried == nil || carried.CarriedFrom != s1.ID || carried.Stages[0].Confirmed != 1 {
		t.Fatalf("s2's plan = %+v, want s1's carried over", carried)
	}
	if ids, err := chat.EarlierSessions(ctx, s2.ID, carried.CarriedFrom); err != nil || len(ids) != 1 || ids[0] != s1.ID {
		t.Errorf("earlier sessions of s2 = %v (%v), want [s1]", ids, err)
	}

	// A second draft; the console showing the first one continues the first.
	s3 := newSession("?new_draft=1")
	d3 := confirm(s3.ID, "Product")
	if d3 == d1 {
		t.Fatal("new_draft did not make a new draft")
	}
	if s := newSession(""); s.DraftRevisionID != d3 {
		t.Errorf("without a revision a session continues %s, want the newest draft %s", s.DraftRevisionID, d3)
	}
	if s := newSession("?revision_id=" + d1); s.DraftRevisionID != d1 {
		t.Errorf("a session opened on draft %s continues %s", d1, s.DraftRevisionID)
	}

	// Activating the newest draft supersedes the older one: a new session
	// continues neither (it makes the next draft from its first plan).
	if _, err := f.pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid WHERE id=$2::uuid`, d3, f.modelID); err != nil {
		t.Fatal(err)
	}
	if s := newSession("?revision_id=" + d3); s.DraftRevisionID != "" {
		t.Errorf("after %s was activated a session continues the older draft %s", d3, s.DraftRevisionID)
	}

	// A finished plan is not carried.
	for i := range carried.Stages {
		carried.Stages[i].Status = aiassistant.StageDone
	}
	_ = chat.SetTaskPlan(ctx, s2.ID, carried)
	if _, err := f.pool.Exec(ctx, `UPDATE ai_assistant.session SET task_plan = NULL WHERE id <> $1::uuid`, s2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid WHERE id=$2::uuid`, f.workingRevID, f.modelID); err != nil {
		t.Fatal(err)
	}
	s5 := newSession("?revision_id=" + d1)
	if p, _ := chat.TaskPlan(ctx, s5.ID); p != nil {
		t.Errorf("a finished plan was carried: %+v", p)
	}
}
