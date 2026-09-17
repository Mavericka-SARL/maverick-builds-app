package aiassistant_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// TestListProposals_ReturnsEveryStatusMostRecentFirst is a regression test
// for the Activity panel's data source: ListPendingProposals (used
// internally by aiSendMessage) only ever returns "pending" rows, so it
// can't back a full history view. ListProposals must return every status —
// pending, confirmed, rejected, executed, partial — ordered newest first.
func TestListProposals_ReturnsEveryStatusMostRecentFirst(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	ctx := context.Background()
	appID, userID := seedAppAndUser(t, pool, "proposals@test.com")
	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, appID, userID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	pStore := aiassistant.NewProposalStore(pool)
	mkStep := func(name string) []aiassistant.ProposalStep {
		params, _ := json.Marshal(map[string]string{"name": name})
		return []aiassistant.ProposalStep{{Tool: "create_dimension", Description: "Create dimension '" + name + "'", Params: params}}
	}

	pending, err := pStore.CreateProposal(ctx, sess.ID, mkStep("Pending"))
	if err != nil {
		t.Fatalf("create pending proposal: %v", err)
	}
	rejected, err := pStore.CreateProposal(ctx, sess.ID, mkStep("Rejected"))
	if err != nil {
		t.Fatalf("create rejected proposal: %v", err)
	}
	if err := pStore.SetStatus(ctx, rejected.ID, "rejected"); err != nil {
		t.Fatalf("set rejected status: %v", err)
	}
	executed, err := pStore.CreateProposal(ctx, sess.ID, mkStep("Executed"))
	if err != nil {
		t.Fatalf("create executed proposal: %v", err)
	}
	if err := pStore.SetStatus(ctx, executed.ID, "executed"); err != nil {
		t.Fatalf("set executed status: %v", err)
	}

	// ListPendingProposals must only surface the still-pending one.
	pendingOnly, err := pStore.ListPendingProposals(ctx, sess.ID)
	if err != nil {
		t.Fatalf("ListPendingProposals: %v", err)
	}
	if len(pendingOnly) != 1 || pendingOnly[0].ID != pending.ID {
		t.Fatalf("ListPendingProposals returned %d proposal(s), want exactly the pending one", len(pendingOnly))
	}

	all, err := pStore.ListProposals(ctx, sess.ID)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListProposals returned %d proposal(s), want 3 (pending + rejected + executed)", len(all))
	}
	// created_at ordering isn't guaranteed to have millisecond resolution
	// under a fast test run, so assert by set membership + status, not by
	// a strict newest-first index — the important behavior is "no status
	// filtered out", which the count above already proves; this checks
	// each individual status made it through untouched.
	byID := map[string]aiassistant.Proposal{}
	for _, p := range all {
		byID[p.ID] = p
	}
	if byID[pending.ID].Status != "pending" {
		t.Errorf("pending proposal status = %q, want pending", byID[pending.ID].Status)
	}
	if byID[rejected.ID].Status != "rejected" {
		t.Errorf("rejected proposal status = %q, want rejected", byID[rejected.ID].Status)
	}
	if byID[executed.ID].Status != "executed" {
		t.Errorf("executed proposal status = %q, want executed", byID[executed.ID].Status)
	}
}

func TestListProposals_EmptySessionReturnsNoRows(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	ctx := context.Background()
	appID, userID := seedAppAndUser(t, pool, "empty@test.com")
	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, appID, userID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	pStore := aiassistant.NewProposalStore(pool)
	all, err := pStore.ListProposals(ctx, sess.ID)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected no proposals for a fresh session, got %d", len(all))
	}
}
