package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// The AI Developer fills a grid with sample values, hands the same rows over
// as a file, writes a CSV of its own into the chat, and clears values under
// a member — asked to clear test data, it used to save a full-reload
// integration that never ran, and it could not hand over a file (a tester,
// 2026-10-15).
func TestAIDeveloperClearsFillsAndWritesFiles(t *testing.T) {
	fake := &multiScriptProvider{}
	f := setupLinkFixture(t, fake)
	ctx := context.Background()
	chat := aiassistant.NewChatStore(f.pool)
	sess, err := chat.CreateSession(ctx, f.tgtApp, f.tgtModel, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chat.SetTitleIfEmpty(ctx, sess.ID, "values")
	// A grid without input metrics, before the session's draft copies the
	// revision: clearing "everything" must step over it.
	calendar := f.ok("POST", "/api/developer/grids", linkDev, f.tgtApp, f.tgtRev, map[string]any{"name": "Calendar", "revision_id": f.tgtRev})
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
	values := func(metric string) map[string]float64 {
		t.Helper()
		rows, err := f.pool.Query(ctx, `SELECT DISTINCT ON (dim_members) (SELECT value FROM jsonb_each_text(dim_members) LIMIT 1), value::float8
			FROM runtime.fact_input WHERE metric_id = $1::uuid ORDER BY dim_members, entered_at DESC, id DESC`, metric)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]float64{}
		for rows.Next() {
			var c string
			var v float64
			_ = rows.Scan(&c, &v)
			out[c] = v
		}
		return out
	}

	turn("fill Inbound with test data", proposeResp("p1", map[string]any{"tool": "fill_sample_values", "description": "Sample data",
		"params": map[string]any{"grid_id": f.inbound, "metrics": map[string]any{"rev_in": map[string]any{"value": 7}, "dbl_in": map[string]any{"min": 1, "max": 9}}}}))
	if p := confirmLatest(); p.Status != "executed" || !strings.Contains(p.Steps[0].Result, "Filled 4 cell(s)") {
		t.Fatalf("fill: %+v", p.Steps)
	}
	if got := values(f.tgtMetric["rev_in"]); got["UK"] != 7 || got["DE"] != 7 {
		t.Fatalf("rev_in after the fill = %v, want 7 in UK and DE", got)
	}

	turn("the same as a file", proposeResp("p2", map[string]any{"tool": "fill_sample_values", "description": "Sample file",
		"params": map[string]any{"grid_id": f.inbound, "as_file": true}}))
	if p := confirmLatest(); p.Status != "executed" || !strings.Contains(p.Steps[0].Result, "Attached sample values - Inbound.csv") {
		t.Fatalf("as file: %+v", p.Steps)
	}

	turn("write me a load file", toolCallResp("c1", "create_csv_file", map[string]any{
		"filename": "load", "columns": []string{"region", "rev_in"}, "rows": [][]any{{"UK", 1}, {"DE", 2}}}),
		providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Attached."}})
	var docs, conversions int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM ai_assistant.document WHERE session_id=$1::uuid AND raw_data IS NOT NULL`, sess.ID).Scan(&docs)
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM ai_assistant.conversion WHERE session_id=$1::uuid`, sess.ID).Scan(&conversions)
	if docs != 2 || conversions != 2 {
		t.Fatalf("%d importable attachment(s) and %d downloadable file(s), want 2 of each", docs, conversions)
	}
	status, raw := f.req("GET", "/api/ai/sessions/"+sess.ID+"/conversions", linkDev, f.tgtApp, "", nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"load"`) {
		t.Fatalf("the chat's files: %d %s", status, raw)
	}

	var regionDim string
	_ = f.pool.QueryRow(ctx, `SELECT dimension_id::text FROM model.grid_dimension WHERE grid_id=$1::uuid`, f.inbound).Scan(&regionDim)
	turn("clear the UK values", proposeResp("p3",
		map[string]any{"tool": "clear_input_values", "description": "Clear Calendar", "params": map[string]any{"grid_id": calendar}},
		map[string]any{"tool": "clear_input_values", "description": "Clear UK",
			"params": map[string]any{"grid_id": f.inbound, "members": map[string][]string{regionDim: {"UK"}}}}))
	if p := confirmLatest(); p.Status != "executed" || !strings.HasPrefix(p.Steps[0].Result, "Nothing to clear: this grid has no input metric") ||
		!strings.Contains(p.Steps[1].Result, "Cleared 2 cell(s)") {
		t.Fatalf("clear: %+v", p.Steps)
	}
	if got := values(f.tgtMetric["rev_in"]); got["UK"] != 0 || got["DE"] != 7 {
		if _, still := got["UK"]; still || got["DE"] != 7 {
			t.Fatalf("rev_in after clearing UK = %v, want DE alone", got)
		}
	}
}
