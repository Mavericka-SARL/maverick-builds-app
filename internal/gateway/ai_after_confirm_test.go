package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

func TestIferrorFallbacks(t *testing.T) {
	got := iferrorFallbacks(`IF(a > 0, IFERROR(ROUND(spend / LOOKUP(cpt, ch, "Paid, search")), 0), IFERROR(x, -1))`)
	if len(got) != 2 || got[0] != 0 || got[1] != -1 {
		t.Fatalf("fallbacks = %v, want [0 -1]", got)
	}
	if fb, ok := everyCellIsFallback("IFERROR(a / b, 0)", map[string]float64{"x": 0, "y": 0, "z": 0}); !ok || fb != 0 {
		t.Fatalf("all zeros not seen as the fallback")
	}
	if _, ok := everyCellIsFallback("IFERROR(a / b, 0)", map[string]float64{"x": 0, "y": 3}); ok {
		t.Fatalf("a computed value read as the fallback")
	}
}

// After a confirmed plan, the next turn starts with the server's check of
// its results: an IFERROR returning its fallback in every cell is named.
func TestSendMessage_AfterConfirmNamesIferrorFallbacks(t *testing.T) {
	plan := []map[string]any{
		{"tool": "create_dimension", "description": "Regions", "params": map[string]any{"name": "region",
			"members": []map[string]any{{"code": "N", "label": "North"}, {"code": "S", "label": "South"}}}},
		turnMetricStep("spend", map[string]any{"is_input": true}),
		turnMetricStep("trials", map[string]any{"formula": "IFERROR(spend / (spend - spend), 0)"}),
		{"tool": "create_grid", "description": "Funnel", "params": map[string]any{"name": "Funnel", "dimensions": []string{"region"}, "metrics": []string{"spend", "trials"}}},
		{"tool": "write_input_values", "description": "Spend", "params": map[string]any{"metric_id": "spend", "values": []map[string]any{
			{"members": map[string]string{"region": "N"}, "value": 100}, {"members": map[string]string{"region": "S"}, "value": 200}}}},
	}
	reply := providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Looking at it."}}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{proposeResp("c1", plan...), proposeResp("c1b", plan...), reply}}
	env := newAITurnEnv(t, fake)
	if status, sse := env.send("build the funnel"); status != http.StatusOK || !strings.Contains(sse, "event: proposal") {
		t.Fatalf("send: %d\n%s\n%q", status, sse, toolResults(t, env))
	}
	ps, _ := env.proposals.ListProposals(context.Background(), env.session)
	if status, body := env.confirm(ps[len(ps)-1].ID); status != http.StatusOK || !strings.Contains(body, `"executed"`) {
		t.Fatalf("confirm: %d %s", status, body)
	}
	if status, sse := env.send("what next?"); status != http.StatusOK {
		t.Fatalf("send: %d\n%s", status, sse)
	}
	msgs, _ := env.chat.ListMessages(context.Background(), env.session)
	found := ""
	for _, m := range msgs {
		if m.Role == "assistant" && strings.HasPrefix(m.Content, afterConfirmHead) {
			found = m.Content
		}
	}
	if !strings.Contains(found, "IFERROR returns its fallback in every cell") || !strings.Contains(found, "cell(s), every one 0") || !strings.Contains(found, "- trials:") {
		t.Fatalf("the after-confirm check said: %q", found)
	}
}
