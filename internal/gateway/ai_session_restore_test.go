package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// A deleted session is hidden and kept: gone from the list and every read,
// listed as deleted with its time, brought back whole by restore, and
// removed for good only once kept 30 days. One click deleted it for good
// before (2026-11-15, an owner's session lost mid-build).
func TestAISessionDeleteKeepsItForRestore(t *testing.T) {
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Noted."}},
	}}
	env := newAITurnEnv(t, fake)
	if code, sse := env.send("Remember: the plan has four stages."); code != http.StatusOK || !strings.Contains(sse, "event: done") {
		t.Fatalf("send: %d\n%s", code, sse)
	}
	sid := env.session
	listed := func(query string) []string {
		t.Helper()
		code, body := env.get("/api/ai/sessions" + query)
		if code != http.StatusOK {
			t.Fatalf("list%s: %d %s", query, code, body)
		}
		var ss []struct {
			ID        string  `json:"id"`
			DeletedAt *string `json:"deleted_at"`
		}
		if err := json.Unmarshal([]byte(body), &ss); err != nil {
			t.Fatalf("list%s: %v\n%s", query, err, body)
		}
		var ids []string
		for _, s := range ss {
			if (query != "") != (s.DeletedAt != nil) {
				t.Errorf("list%s: %s deleted_at %v", query, s.ID, s.DeletedAt)
			}
			ids = append(ids, s.ID)
		}
		return ids
	}
	if ids := listed(""); len(ids) != 1 || ids[0] != sid {
		t.Fatalf("before delete: %v", ids)
	}

	if code, body := env.do("DELETE", "/api/ai/sessions/"+sid, nil); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	if ids := listed(""); len(ids) != 0 {
		t.Errorf("a deleted session is listed: %v", ids)
	}
	if code, _ := env.get("/api/ai/sessions/" + sid); code != http.StatusNotFound {
		t.Errorf("a deleted session reads: %d", code)
	}
	if code, _ := env.send("Go on."); code == http.StatusOK {
		t.Errorf("a deleted session takes a message")
	}
	if code, _ := env.do("DELETE", "/api/ai/sessions/"+sid, nil); code != http.StatusNotFound {
		t.Errorf("deleted twice: %d", code)
	}
	if ids := listed("?deleted=true"); len(ids) != 1 || ids[0] != sid {
		t.Fatalf("deleted list: %v", ids)
	}

	code, body := env.post("/api/ai/sessions/"+sid+"/restore", nil)
	if code != http.StatusOK || !strings.Contains(body, sid) {
		t.Fatalf("restore: %d %s", code, body)
	}
	if ids := listed(""); len(ids) != 1 || ids[0] != sid {
		t.Errorf("after restore: %v", ids)
	}
	if ids := listed("?deleted=true"); len(ids) != 0 {
		t.Errorf("still listed as deleted: %v", ids)
	}
	code, body = env.get("/api/ai/sessions/" + sid)
	if code != http.StatusOK || !strings.Contains(body, "the plan has four stages") || !strings.Contains(body, "Noted.") {
		t.Errorf("the restored session lost its messages: %d %s", code, body)
	}
	if code, _ := env.post("/api/ai/sessions/"+sid+"/restore", nil); code != http.StatusNotFound {
		t.Errorf("restoring a session not deleted: %d", code)
	}

	// Kept 30 days: then the list removes it for good, and it cannot be
	// restored.
	if code, _ := env.do("DELETE", "/api/ai/sessions/"+sid, nil); code != http.StatusOK {
		t.Fatal("second delete")
	}
	ctx := context.Background()
	if _, err := env.db.Exec(ctx, `UPDATE ai_assistant.session SET deleted_at = now() - interval '31 days' WHERE id = $1::uuid`, sid); err != nil {
		t.Fatal(err)
	}
	if ids := listed("?deleted=true"); len(ids) != 0 {
		t.Errorf("kept past 30 days in the deleted list: %v", ids)
	}
	if code, _ := env.post("/api/ai/sessions/"+sid+"/restore", nil); code != http.StatusNotFound {
		t.Errorf("restored after 30 days: %d", code)
	}
	listed("")
	var n int
	if err := env.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM ai_assistant.session WHERE id = $1::uuid) + (SELECT count(*) FROM ai_assistant.message WHERE session_id = $1::uuid)`, sid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows of the session left after the purge", n)
	}
}
