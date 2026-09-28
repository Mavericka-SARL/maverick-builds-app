package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// The assistant edits members only in its draft, where nothing
// recalculates; promoting the draft must recompute the metrics that read
// what it changed (contract C8), here a property read as region.factor.
func TestPromoteDraftRecalculatesMetricsReadingAIMemberEdits(t *testing.T) {
	assertActivationRecalculatesAIDraft(t, func(f *promoteFixture, sessionID, _ string) (int, []byte) {
		return f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/promote-draft", f.devSub, nil)
	})
}

// The same draft can go live through the developer's generic activate
// endpoint instead of promote-draft; that door must recompute it too.
func TestDeveloperActivateRecalculatesAIDraft(t *testing.T) {
	assertActivationRecalculatesAIDraft(t, func(f *promoteFixture, _, draftRevID string) (int, []byte) {
		return f.do(t, "PUT", "/api/developer/revisions/"+draftRevID+"/activate", f.devSub, nil)
	})
}

func assertActivationRecalculatesAIDraft(t *testing.T, activate func(f *promoteFixture, sessionID, draftRevID string) (int, []byte)) {
	t.Helper()
	f := setupPromoteFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	region := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid,$2::uuid,'region','standard') RETURNING id::text`, f.modelID, f.workingRevID)
	q(`INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid,'factor','number') RETURNING id::text`, region)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label, properties) VALUES ($1::uuid,'EMEA','EMEA','{"factor":"2"}') RETURNING id::text`, region)
	metric := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula) VALUES ($1::uuid,$2::uuid,'factor_x10',false,'region.factor * 10') RETURNING id::text`, f.modelID, f.workingRevID)
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'G') RETURNING id::text`, f.modelID, f.workingRevID)
	q(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid,$2::uuid) RETURNING grid_id::text`, grid, region)
	q(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid,$2::uuid) RETURNING grid_id::text`, grid, metric)

	sess, err := aiassistant.NewChatStore(f.pool).CreateSession(ctx, f.appID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	params, _ := json.Marshal(map[string]any{"dimension_id": "region", "code": "EMEA", "properties": map[string]string{"factor": "7"}})
	proposal, err := aiassistant.NewProposalStore(f.pool).CreateProposal(ctx, sess.ID, []aiassistant.ProposalStep{
		{Tool: "update_dimension_member", Description: "Set EMEA's factor to 7", Params: params},
	})
	if err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	if status, body := f.do(t, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+proposal.ID+"/confirm", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("confirm: status %d, body %s", status, body)
	}
	confirmed, err := aiassistant.NewChatStore(f.pool).GetSession(ctx, sess.ID)
	if err != nil || confirmed.DraftRevisionID == "" {
		t.Fatalf("session has no draft revision after confirm (err %v)", err)
	}
	if status, body := activate(f, sess.ID, confirmed.DraftRevisionID); status != http.StatusOK {
		t.Fatalf("activate: status %d, body %s", status, body)
	}

	active := activeRevisionID(t, f.pool, f.modelID)
	if active == f.workingRevID {
		t.Fatal("promotion did not switch the active revision")
	}
	var last float64
	seen := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var v float64
		err := f.pool.QueryRow(ctx, `
			SELECT cr.value FROM runtime.calc_result cr
			JOIN model.metric_def m ON m.id = cr.metric_id AND m.name = 'factor_x10'
			JOIN model.dimension_def d ON d.revision_id = cr.revision_id AND d.name = 'region'
			WHERE cr.model_id=$1::uuid AND cr.revision_id=$2::uuid AND cr.dim_members = jsonb_build_object(d.id::text, 'EMEA')
		`, f.modelID, active).Scan(&v)
		if err == nil {
			last, seen = v, true
			if v == 70 {
				return
			}
		}
	}
	if !seen {
		t.Fatalf("no factor_x10 row at EMEA in the promoted revision (want 70)")
	}
	t.Fatalf("factor_x10 at EMEA in the promoted revision = %v, want 70", last)
}
