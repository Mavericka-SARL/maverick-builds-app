package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// A revision could not be renamed (2026-11-08): the AI-built application's
// header read "AI Draft 2026-11-08 10:12:55.576" to every business user.
// The developer renames any revision of their model — the live one too,
// whose name the model keeps beside its id for the header and the Live
// badge — and a name another revision has is refused, not a 500.
func TestDeveloperRenamesARevisionAndTheModelFollows(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()

	status, body := f.do(t, "PATCH", "/api/developer/revisions/"+f.workingRevID, f.devSub, map[string]string{"name": "  Production FP&A  "})
	if status != http.StatusOK {
		t.Fatalf("rename the live revision: %d %s", status, body)
	}
	var got struct{ ID, Name string }
	_ = json.Unmarshal(body, &got)
	if got.ID != f.workingRevID || got.Name != "Production FP&A" {
		t.Fatalf("rename answered %+v, want the revision and its trimmed name", got)
	}
	var revName, modelRevName string
	if err := f.pool.QueryRow(ctx, `
		SELECT r.name, m.active_revision_name FROM model.revision r JOIN core.model m ON m.id = r.model_id
		WHERE r.id = $1::uuid`, f.workingRevID).Scan(&revName, &modelRevName); err != nil {
		t.Fatal(err)
	}
	if revName != "Production FP&A" || modelRevName != "Production FP&A" {
		t.Fatalf("revision %q, model's live revision name %q; want both renamed", revName, modelRevName)
	}

	var otherRev string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Scratch') RETURNING id::text`, f.modelID).Scan(&otherRev); err != nil {
		t.Fatal(err)
	}
	if status, body := f.do(t, "PATCH", "/api/developer/revisions/"+otherRev, f.devSub, map[string]string{"name": "Production FP&A"}); status != http.StatusConflict {
		t.Fatalf("a name the live revision has: %d %s, want 409", status, body)
	}
	if status, body := f.do(t, "PATCH", "/api/developer/revisions/"+otherRev, f.devSub, map[string]string{"name": "   "}); status != http.StatusBadRequest {
		t.Fatalf("a blank name: %d %s, want 400", status, body)
	}
	// Renaming a revision that is not live leaves the model's name alone.
	if status, body := f.do(t, "PATCH", "/api/developer/revisions/"+otherRev, f.devSub, map[string]string{"name": "Plan 2027"}); status != http.StatusOK {
		t.Fatalf("rename a working revision: %d %s", status, body)
	}
	if err := f.pool.QueryRow(ctx, `SELECT active_revision_name FROM core.model WHERE id = $1::uuid`, f.modelID).Scan(&modelRevName); err != nil {
		t.Fatal(err)
	}
	if modelRevName != "Production FP&A" {
		t.Fatalf("renaming another revision changed the live name to %q", modelRevName)
	}
}

// The draft is named as the developer takes it; a name another revision has
// stops the promote before anything changes.
func TestPromoteDraftNamesTheRevision(t *testing.T) {
	f := setupPromoteFixture(t)
	sessionID, proposalID := f.createSessionWithProposal(t, f.devID, "Plants")
	if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/confirm", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("confirm: %d %s", status, body)
	}

	status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/promote-draft", f.devSub, map[string]string{"name": "Working"})
	if status != http.StatusConflict {
		t.Fatalf("promote under the live revision's name: %d %s, want 409", status, body)
	}
	sess, err := aiassistant.NewChatStore(f.pool).GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.DraftRevisionID == "" || sess.PromotedAt != nil {
		t.Fatal("a refused name still promoted the draft")
	}

	status, body = f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/promote-draft", f.devSub, map[string]string{"name": "Production FP&A"})
	if status != http.StatusOK {
		t.Fatalf("promote: %d %s", status, body)
	}
	var resp struct {
		Revision struct{ ID, Name string } `json:"revision"`
	}
	_ = json.Unmarshal(body, &resp)
	if resp.Revision.Name != "Production FP&A" {
		t.Fatalf("promote answered the revision %+v, want it named Production FP&A", resp.Revision)
	}
	var name string
	if err := f.pool.QueryRow(context.Background(), `SELECT name FROM model.revision WHERE id = $1::uuid`, resp.Revision.ID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Production FP&A" {
		t.Fatalf("promoted revision is named %q", name)
	}

	// No name keeps the draft's own.
	sessionID, proposalID = f.createSessionWithProposal(t, f.devID, "Lines")
	if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/confirm", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("confirm: %d %s", status, body)
	}
	if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/promote-draft", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("promote with no body: %d %s", status, body)
	}
}

// The AI Developer renames the draft it works in, when the developer names it.
func TestAIRenamesItsDraft(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()
	sessionID, proposalID := f.createSessionWithProposal(t, f.devID, "Plants")
	if status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+proposalID+"/confirm", f.devSub, nil); status != http.StatusOK {
		t.Fatalf("confirm: %d %s", status, body)
	}
	params, _ := json.Marshal(map[string]string{"name": "Production FP&A"})
	p, err := aiassistant.NewProposalStore(f.pool).CreateProposal(ctx, sessionID, []aiassistant.ProposalStep{
		{Tool: "rename_draft", Description: "Name the draft", Params: params},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	status, body := f.do(t, "POST", "/api/ai/sessions/"+sessionID+"/proposals/"+p.ID+"/confirm", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("confirm rename_draft: %d %s", status, body)
	}
	sess, err := aiassistant.NewChatStore(f.pool).GetSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := f.pool.QueryRow(ctx, `SELECT name FROM model.revision WHERE id = $1::uuid`, sess.DraftRevisionID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Production FP&A" {
		t.Fatalf("draft is named %q after rename_draft", name)
	}
	var working string
	_ = f.pool.QueryRow(ctx, `SELECT name FROM model.revision WHERE id = $1::uuid`, f.workingRevID).Scan(&working)
	if working != "Working" {
		t.Fatalf("rename_draft renamed the working revision to %q", working)
	}
}

// A system-managed revision holds an approval's copy: cell writes and
// imports refused it, but Build changed its members and definitions
// (2026-09-29). Every change through Build is refused now; reading is not.
func TestBuildRefusesASystemManagedRevision(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	dim := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Region') RETURNING id::text`, f.modelID, f.workingRevID)
	member := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'EU', 'Europe') RETURNING id::text`, dim)
	metric := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'sales', true) RETURNING id::text`, f.modelID, f.workingRevID)
	if _, err := f.pool.Exec(ctx, `UPDATE model.revision SET system_managed = true WHERE id = $1::uuid`, f.workingRevID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"PATCH", "/api/developer/metrics/" + metric, map[string]string{"label": "Sales"}},
		{"DELETE", "/api/developer/metrics/" + metric, nil},
		{"PATCH", "/api/developer/dimensions/" + dim, map[string]string{"name": "Regions"}},
		{"POST", "/api/developer/dimensions/" + dim + "/members", map[string]string{"code": "US", "label": "US"}},
		{"PATCH", "/api/developer/dimensions/" + dim + "/members/" + member, map[string]string{"label": "EU"}},
		{"DELETE", "/api/developer/dimensions/" + dim + "/members/" + member, nil},
		{"PUT", "/api/developer/dimensions/" + dim + "/members/order", map[string]any{"member_ids": []string{member}}},
		{"POST", "/api/developer/grids", map[string]any{"name": "G", "revision_id": f.workingRevID, "metric_ids": []string{metric}, "dimension_ids": []string{dim}}},
	} {
		if status, body := f.do(t, c.method, c.path, f.devSub, c.body); status != http.StatusForbidden {
			t.Errorf("%s %s on a system-managed revision: %d %s, want 403", c.method, c.path, status, body)
		}
	}
	if status, body := f.do(t, "GET", "/api/developer/dimensions/"+dim+"/properties", f.devSub, nil); status != http.StatusOK {
		t.Errorf("reading a system-managed revision's dimension: %d %s", status, body)
	}
	var label string
	_ = f.pool.QueryRow(ctx, `SELECT label FROM model.dimension_member WHERE id = $1::uuid`, member).Scan(&label)
	if label != "Europe" {
		t.Errorf("the member was changed to %q", label)
	}
}
