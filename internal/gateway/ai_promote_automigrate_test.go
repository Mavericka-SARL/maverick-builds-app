// Tests that promoting an AI draft triggers a schema migration, closing a
// real gap: the manual create_metric/update_metric handlers each call
// autoMigrate synchronously after every write, but write_executor's AI
// equivalents have no access to *handler across the aiassistant/gateway
// package boundary and never called it at all — an AI-authored metric/
// dimension change had no schema-migration path whatsoever until
// aiPromoteDraft was given a single autoMigrate pass right after the
// draft's structural writes become the live model.
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

func TestPromoteDraftTriggersAutoMigrate(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()

	// autoMigrate's Generate() step requires the model to have at least one
	// metric ("model has no metrics defined" otherwise) — a plain dimension
	// proposal wouldn't exercise the real path.
	chatStore := aiassistant.NewChatStore(f.pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	params, _ := json.Marshal(map[string]any{"name": "revenue", "is_input": true})
	pStore := aiassistant.NewProposalStore(f.pool)
	proposal, err := pStore.CreateProposal(ctx, sess.ID, []aiassistant.ProposalStep{
		{Tool: "create_metric", Description: "Create input metric 'revenue'", Params: params},
	})
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	sessionID, proposalID := sess.ID, proposal.ID

	if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/confirm", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("confirm: status %d, body %s", status, body)
	}

	var beforeCount int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM deployment.schema_migration WHERE model_id=$1::uuid`, f.modelID).Scan(&beforeCount); err != nil {
		t.Fatalf("count schema_migration before promote: %v", err)
	}
	if beforeCount != 0 {
		t.Fatalf("expected no schema_migration rows before promote, found %d", beforeCount)
	}

	status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/promote-draft", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("promote: status %d, body %s", status, body)
	}

	// autoMigrate runs in a background goroutine (go h.autoMigrate(...)) that
	// does store.Create (row lands as 'pending') then store.Apply (a
	// separate Get + pool.Begin + DDL exec + status update to 'applied')
	// as two sequential steps, not one atomic write — so poll until the
	// row exists AND has left 'pending', not just until it exists, or this
	// can observe (and wrongly fail on) the row mid-flight between Create
	// and Apply. That window is normally sub-millisecond but can widen
	// under heavy concurrent load (e.g. the full `-race ./...` suite),
	// which is exactly what surfaced this as flaky in CI.
	deadline := time.Now().Add(5 * time.Second)
	var afterCount int
	var migStatus string
	for time.Now().Before(deadline) {
		if err := f.pool.QueryRow(context.Background(), `
			SELECT count(*), COALESCE(MAX(status),'') FROM deployment.schema_migration WHERE model_id=$1::uuid
		`, f.modelID).Scan(&afterCount, &migStatus); err != nil {
			t.Fatalf("count schema_migration after promote: %v", err)
		}
		if afterCount > 0 && migStatus != "pending" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if afterCount == 0 {
		t.Fatal("expected promote-draft to trigger autoMigrate and create a deployment.schema_migration row, found none")
	}
	if migStatus != "applied" {
		t.Errorf("schema_migration status = %q, want 'applied'", migStatus)
	}
}
