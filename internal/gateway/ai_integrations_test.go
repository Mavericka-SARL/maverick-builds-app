package gateway

// The AI Developer sets up, tests, activates and runs a model link and a
// sign-in connection through its tools, as the developer would in the
// Integrations tab: a scripted model calls the tools, the real gateway
// checks, confirms and runs them. A secret goes from the confirmation card
// to the connection and nowhere else; a plan that reads another tenant's
// model or carries a secret is refused before the developer sees it.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

func toolCallResp(id, tool string, args map[string]any) providers.ChatResponse {
	raw, _ := json.Marshal(args)
	return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
		ToolCalls: []providers.ToolCall{{ID: id, Name: tool, Arguments: raw}}}}
}

func TestAIDeveloperBuildsTestsAndRunsAModelLink(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	fake := &multiScriptProvider{}
	f := setupLinkFixture(t, fake)
	ctx := context.Background()

	// The gateway's model-link runner, as cmd/gateway runs it.
	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go f.ml.Run(runCtx, f.pool, "", logger.New("test"), "test-gateway")

	chat := aiassistant.NewChatStore(f.pool)
	sess, err := chat.CreateSession(ctx, f.tgtApp, f.tgtModel, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chat.SetTitleIfEmpty(ctx, sess.ID, "links")
	proposals := aiassistant.NewProposalStore(f.pool)
	turn := func(content string, resps ...providers.ChatResponse) string {
		t.Helper()
		fake.resps = append(fake.resps, resps...)
		status, raw := f.req("POST", "/api/ai/sessions/"+sess.ID+"/messages", linkDev, f.tgtApp, "", map[string]string{"content": content})
		if status != http.StatusOK || !(strings.Contains(string(raw), "event: done") || strings.Contains(string(raw), "event: proposal")) {
			t.Fatalf("turn %q: %d\n%s", content, status, raw)
		}
		return string(raw)
	}
	latestProposal := func() aiassistant.Proposal {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, `SELECT id::text FROM ai_assistant.proposal WHERE session_id=$1::uuid ORDER BY created_at DESC LIMIT 1`, sess.ID).Scan(&id); err != nil {
			t.Fatalf("no proposal: %v", err)
		}
		p, err := proposals.GetProposal(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	confirm := func(p aiassistant.Proposal, body any) (int, aiassistant.Proposal) {
		t.Helper()
		status, raw := f.req("POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+p.ID+"/confirm", linkDev, f.tgtApp, "", body)
		var out struct {
			Proposal aiassistant.Proposal `json:"proposal"`
		}
		_ = json.Unmarshal(raw, &out)
		return status, out.Proposal
	}
	tools := func() string {
		msgs, _ := chat.ListMessages(ctx, sess.ID)
		var b strings.Builder
		for _, m := range msgs {
			if m.Role == "tool" {
				b.WriteString(m.Content + "\n")
			}
		}
		return b.String()
	}

	linkCfg := f.linkConfig("Sales › Sales plan")
	linkCfg["target_id"] = "Inbound"
	connStep := map[string]any{"tool": "create_connection", "description": "Billing API key",
		"params": map[string]any{"name": "Billing", "auth_type": "api_key"}}
	linkStep := map[string]any{"tool": "create_api_integration", "description": "Link Sales into P&L",
		"params": map[string]any{"name": "Sales into P&L", "config": linkCfg}}

	// ── A plan the check refuses never reaches the developer ──
	foreign := f.linkConfig(f.foreignModel)
	leak := map[string]any{"tool": "create_connection", "description": "Leaky", "params": map[string]any{
		"name": "Leaky", "auth_type": "api_key", "credential": map[string]any{"value": "pasted-secret"}}}
	turn("link their model", proposeResp("p0", map[string]any{"tool": "create_api_integration", "description": "Foreign",
		"params": map[string]any{"name": "Foreign", "config": foreign}}, leak),
		providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Cannot."}})
	if got := tools(); !strings.Contains(got, "not a model of this tenant") || !strings.Contains(got, "never put a secret") {
		t.Fatalf("the check did not refuse the foreign source and the secret:\n%s", got)
	}
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM ai_assistant.proposal WHERE session_id=$1::uuid`, sess.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d proposal(s) stored from a refused plan", n)
	}

	// ── Discover, propose, confirm with the secret typed into the card ──
	turn("link the Sales plan's Sales grid into Inbound",
		toolCallResp("s1", "list_model_link_sources", map[string]any{}),
		proposeResp("p1", connStep, linkStep))
	if !strings.Contains(tools(), "Sales plan") {
		t.Fatalf("list_model_link_sources did not offer the Sales plan:\n%s", tools())
	}
	p := latestProposal()
	if status, _ := confirm(p, nil); status != http.StatusOK {
		t.Fatalf("confirm without the card: %d", status)
	}
	if got := latestProposal(); got.Steps[0].Status != "failed" || !strings.Contains(got.Steps[0].Result, "card left value empty") {
		t.Fatalf("a connection confirmed without its secret: %+v", got.Steps[0])
	}
	turn("again", proposeResp("p2", connStep, linkStep))
	p = latestProposal()
	status, done := confirm(p, map[string]any{"secrets": map[string]any{"1": map[string]string{"value": "k-123"}}})
	if status != http.StatusOK || done.Status != "executed" {
		t.Fatalf("confirm: %d %+v", status, done)
	}
	var connID, appID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text, application_id::text FROM model.integration_connection WHERE name='Billing' ORDER BY created_at DESC LIMIT 1`).Scan(&connID, &appID); err != nil {
		t.Fatal(err)
	}
	if _, _, secret, err := integration.NewStore(f.pool).OpenCredential(ctx, appID, connID); err != nil || string(secret) != `{"value":"k-123"}` {
		t.Fatalf("stored credential %q (%v)", secret, err)
	}
	// The secret is in no proposal, message or audit row.
	for _, q := range []string{`SELECT count(*) FROM ai_assistant.proposal WHERE steps::text LIKE '%k-123%'`, `SELECT count(*) FROM ai_assistant.message WHERE content LIKE '%k-123%'`,
		`SELECT count(*) FROM audit.audit_event WHERE metadata::text LIKE '%k-123%'`} {
		if err := f.pool.QueryRow(ctx, q).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d (%v)", q, n, err)
		}
	}
	sess2, _ := chat.GetSession(ctx, sess.ID)
	var linkID, draftInbound string
	if err := f.pool.QueryRow(ctx, `
		SELECT i.id::text, i.target_id::text FROM model.integration_def i
		WHERE i.name='Sales into P&L' AND i.revision_id=$1::uuid AND i.source_model_id=$2::uuid AND i.status='draft'`,
		sess2.DraftRevisionID, f.srcModel).Scan(&linkID, &draftInbound); err != nil {
		t.Fatalf("the link is not a draft in the AI draft revision: %v", err)
	}

	// ── Test (a read tool: runs at once), activate, run ──
	turn("test it",
		toolCallResp("t1", "test_integration", map[string]any{"integration_id": "Sales into P&L"}),
		proposeResp("p3", map[string]any{"tool": "update_api_integration", "description": "Activate",
			"params": map[string]any{"integration_id": "Sales into P&L", "status": "active"}}))
	if got := tools(); !strings.Contains(got, "Test success") || !strings.Contains(got, `"revenue"`) {
		t.Fatalf("test_integration did not report a passing test with its records:\n%s", got)
	}
	if status, done := confirm(latestProposal(), nil); status != http.StatusOK || done.Status != "executed" {
		t.Fatalf("activate: %d %+v", status, done)
	}
	turn("run it", proposeResp("p4", map[string]any{"tool": "run_integration", "description": "Run now",
		"params": map[string]any{"integration_id": "Sales into P&L"}}))
	status, done = confirm(latestProposal(), nil)
	if status != http.StatusOK || done.Status != "executed" || !strings.Contains(done.Steps[0].Result, "4 written") {
		t.Fatalf("run: %d %+v", status, done)
	}
	// The values are in the draft revision's Inbound, as the developer reads it.
	var g struct {
		Cells map[string]float64 `json:"cells"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, raw := f.req("GET", "/api/grid?grid_def_id="+draftInbound+"&revision_id="+sess2.DraftRevisionID, linkDev, f.tgtApp, sess2.DraftRevisionID, nil)
		_ = json.Unmarshal(raw, &g)
		if len(g.Cells) >= 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	sum := 0.0
	for _, v := range g.Cells {
		sum += v
	}
	if len(g.Cells) != 4 || sum != 900 {
		t.Fatalf("draft Inbound after the AI's run: %v", g.Cells)
	}
}

// The assistant fires a rule's manual trigger as the developer would, reads
// the manuals, and attaches a Google Sheet the way the Sheets import fetches
// one.
func TestAIDeveloperTriggersReadsManualsAndAttachesASheet(t *testing.T) {
	fake := &multiScriptProvider{}
	f := setupLinkFixture(t, fake)
	ctx := context.Background()
	sheet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte("region,rev_in\nUK,5\nDE,6\n"))
	}))
	t.Cleanup(sheet.Close)
	f.ml.h.sheets = &importpkg.SheetFetcher{BaseURL: sheet.URL, Client: sheet.Client()}

	chat := aiassistant.NewChatStore(f.pool)
	sess, err := chat.CreateSession(ctx, f.tgtApp, f.tgtModel, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chat.SetTitleIfEmpty(ctx, sess.ID, "rules")
	proposals := aiassistant.NewProposalStore(f.pool)
	turn := func(content string, resps ...providers.ChatResponse) {
		t.Helper()
		fake.resps = append(fake.resps, resps...)
		if status, raw := f.req("POST", "/api/ai/sessions/"+sess.ID+"/messages", linkDev, f.tgtApp, "", map[string]string{"content": content}); status != http.StatusOK {
			t.Fatalf("turn %q: %d %s", content, status, raw)
		}
	}
	confirmLatest := func() aiassistant.Proposal {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, `SELECT id::text FROM ai_assistant.proposal WHERE session_id=$1::uuid ORDER BY created_at DESC LIMIT 1`, sess.ID).Scan(&id); err != nil {
			t.Fatalf("no proposal: %v", err)
		}
		f.ok("POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+id+"/confirm", linkDev, f.tgtApp, "", nil)
		p, _ := proposals.GetProposal(ctx, id)
		return p
	}
	lastTool := func() string {
		msgs, _ := chat.ListMessages(ctx, sess.ID)
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "tool" {
				return msgs[i].Content
			}
		}
		return ""
	}
	stop := providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Done."}}

	// The manuals, as the assistant explains the platform from them.
	turn("how do manual triggers work?", toolCallResp("m1", "read_manual", map[string]any{"query": "automation rule manual trigger"}), stop)
	if got := lastTool(); !strings.Contains(got, "developer manual") || !strings.Contains(strings.ToLower(got), "trigger") {
		t.Fatalf("read_manual: %.400s", got)
	}

	// A Google Sheet, fetched as the Sheets import does, attached to the chat.
	turn("import this sheet", toolCallResp("g1", "attach_google_sheet", map[string]any{
		"sheet_url": "https://docs.google.com/spreadsheets/d/1AbCdEfGhIjKlMnOpQrStUvWxYz0123456789/edit#gid=0"}), stop)
	if got := lastTool(); !strings.Contains(got, "Attached the sheet as google-sheet-") || !strings.Contains(got, "2 data row(s)") {
		t.Fatalf("attach_google_sheet: %s", got)
	}
	var docs int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM ai_assistant.document WHERE session_id=$1::uuid AND filename LIKE 'google-sheet-%'`, sess.ID).Scan(&docs)
	if docs != 1 {
		t.Fatalf("%d attached sheet document(s)", docs)
	}

	// A workflow and its manual rule; the developer promotes and publishes.
	turn("a manual ping workflow", proposeResp("w1",
		map[string]any{"tool": "create_workflow_def", "description": "Ping", "params": map[string]any{"name": "Ping", "trigger_event": "manual"}},
		map[string]any{"tool": "update_workflow_def", "description": "Ping steps", "params": map[string]any{"workflow_def_id": "<created in step 1>",
			"steps": []map[string]any{{"id": "notify", "name": "Notify", "type": "notification",
				"notification": map[string]any{"recipient_type": "requester", "subject": "Ping", "message": "Ping"},
				"routes":       map[string]string{"next": "end-completed"}}}}},
		map[string]any{"tool": "create_automation_rule", "description": "Ping by hand", "params": map[string]any{
			"name": "Ping now", "trigger_type": "manual", "workflow_name": "Ping"}}))
	if p := confirmLatest(); p.Status != "executed" {
		t.Fatalf("workflow proposal: %+v", p.Steps)
	}
	f.ok("POST", "/api/ai/sessions/"+sess.ID+"/promote-draft", linkDev, f.tgtApp, "", nil)
	var wfID, ruleID string
	_ = f.pool.QueryRow(ctx, `SELECT d.id::text FROM workflow.workflow_def d JOIN core.model m ON m.id=$1::uuid
		WHERE d.name='Ping' AND d.revision_id = m.active_revision_id`, f.tgtModel).Scan(&wfID)
	f.ok("POST", "/api/developer/workflows/"+wfID+"/publish", linkDev, f.tgtApp, "", nil)
	_ = f.pool.QueryRow(ctx, `SELECT r.id::text FROM workflow.automation_rule r JOIN core.model m ON m.id=$1::uuid
		WHERE r.name='Ping now' AND r.revision_id = m.active_revision_id`, f.tgtModel).Scan(&ruleID)

	// The assistant fires it — in a new session: promoting finished the first.
	sess, err = chat.CreateSession(ctx, f.tgtApp, f.tgtModel, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chat.SetTitleIfEmpty(ctx, sess.ID, "fire")
	turn("fire it", proposeResp("r1", map[string]any{"tool": "trigger_automation_rule", "description": "Fire Ping now",
		"params": map[string]any{"rule_id": ruleID, "payload": map[string]any{"note": "from the assistant"}}}))
	p := confirmLatest()
	if p.Status != "executed" || !strings.Contains(p.Steps[0].Result, "Rule triggered") {
		t.Fatalf("trigger: %+v", p.Steps)
	}
	var started int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM workflow.workflow_instance WHERE workflow_def_id=$1::uuid`, wfID).Scan(&started)
	if started != 1 {
		t.Fatalf("%d workflow instance(s) started", started)
	}
}
