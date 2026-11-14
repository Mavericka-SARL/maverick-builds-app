package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// recordingProvider answers every main call with reply and records the
// system prompts, and counts the attachment analyses asked for.
type recordingProvider struct {
	mu       sync.Mutex
	reply    string
	prompts  []string
	analyses int
}

func (p *recordingProvider) Chat(_ context.Context, req providers.ChatRequest) (providers.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if req.SystemPrompt == attachmentAnalysisPrompt {
		p.analyses++
	}
	if r, ok := sideCall(req); ok {
		return r, nil
	}
	p.prompts = append(p.prompts, req.SystemPrompt)
	return providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: p.reply}}, nil
}

func (p *recordingProvider) lastPrompt() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.prompts) == 0 {
		return ""
	}
	return p.prompts[len(p.prompts)-1]
}

// An attachment is gone through once, when it arrives: its analysis is kept
// in the session's memory and goes into every later turn in place of the
// file; the file is read again only when the developer asks for it
// (owner, 2026-11-13).
func TestMemory_AttachmentGoneThroughOnce(t *testing.T) {
	p := &recordingProvider{reply: "ok"}
	env := newAITurnEnv(t, p)
	ctx := context.Background()
	if _, err := env.docs.CreateDocument(ctx, env.session, "hiring-policy.md", "text/markdown", "RULE-TEXT: on-cost 21.5% in Germany", false, nil); err != nil {
		t.Fatal(err)
	}
	if status, body := env.send("what does the policy say?"); status != http.StatusOK || !strings.Contains(body, "event: memory") {
		t.Fatalf("turn 1: %d %s", status, body)
	}
	if p.analyses != 1 {
		t.Fatalf("analyses after turn 1 = %d, want 1", p.analyses)
	}
	mem, _ := env.chat.Memory(ctx, env.session)
	if a := mem.Attachment("hiring-policy.md"); a == nil || a.By != "ai" || !strings.Contains(a.Analysis, "ANALYSIS") {
		t.Fatalf("memory holds %+v, want the AI's analysis of the policy", mem.Attachments)
	}
	if pr := p.lastPrompt(); !strings.Contains(pr, "ANALYSIS: a test attachment") || strings.Contains(pr, "RULE-TEXT") {
		t.Errorf("turn 1's prompt should carry the analysis and not the file's text")
	}
	msgs, _ := env.chat.ListMessages(ctx, env.session)
	noted := false
	for _, m := range msgs {
		noted = noted || strings.Contains(m.Content, "I went through hiring-policy.md")
	}
	if !noted {
		t.Error("the developer was not told the attachment was gone through")
	}

	// Turn 2: from memory, no new analysis.
	if status, _ := env.send("and the start dates?"); status != http.StatusOK {
		t.Fatalf("turn 2: %d", status)
	}
	if p.analyses != 1 || strings.Contains(p.lastPrompt(), "RULE-TEXT") {
		t.Errorf("turn 2 analysed again (%d) or sent the file's text", p.analyses)
	}

	// Asked directly, the file is read again.
	if status, _ := env.send("Please re-read the hiring policy"); status != http.StatusOK {
		t.Fatalf("turn 3: %d", status)
	}
	if p.analyses != 2 || !strings.Contains(p.lastPrompt(), "RULE-TEXT") {
		t.Errorf("asked to re-read: analyses %d, file text in prompt %v — want a second analysis and the text", p.analyses, strings.Contains(p.lastPrompt(), "RULE-TEXT"))
	}

	// The memory's status goes to the console with the session.
	status, body := env.get("/api/ai/sessions/" + env.session)
	var opened struct {
		Memory aiassistant.MemoryStatus `json:"memory"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &opened) != nil || len(opened.Memory.Attachments) != 1 || opened.Memory.CapacityTokens == 0 {
		t.Fatalf("session memory: %d %s", status, body)
	}
}

// A part of an attachment read once is answered from memory when it is
// asked for again — never from the file — unless the developer asked for
// the file to be read again.
func TestMemory_ReadsAnswerFromMemory(t *testing.T) {
	env := newAITurnEnv(t, &recordingProvider{reply: "ok"})
	ctx := context.Background()
	if _, err := env.docs.CreateDocument(ctx, env.session, "wb.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "overview", false, []byte("raw")); err != nil {
		t.Fatal(err)
	}
	reads := 0
	rh := aiassistant.ReadHooks{ReadAttachedSheet: func(_ context.Context, file, sheet string, from, to int) (string, error) {
		reads++
		return "ROWS of " + sheet, nil
	}}
	h := &handler{}
	wrapped := h.memoryReads(rh, env.chat, env.session, nil)
	first, err := wrapped.ReadAttachedSheet(ctx, "wb.xlsx", "Hiring Plan", 6, 40)
	if err != nil || first != "ROWS of Hiring Plan" || reads != 1 {
		t.Fatalf("first read: %q %v reads=%d", first, err, reads)
	}
	again, _ := wrapped.ReadAttachedSheet(ctx, "WB.xlsx", "Hiring Plan", 10, 20)
	if reads != 1 || !strings.HasPrefix(again, "(From your memory") || !strings.Contains(again, "ROWS of Hiring Plan") {
		t.Errorf("a covered range read the file again (reads=%d) or did not say it came from memory: %q", reads, again)
	}
	if _, _ = wrapped.ReadAttachedSheet(ctx, "wb.xlsx", "Hiring Plan", 1, 60); reads != 2 {
		t.Errorf("a range not read before: reads=%d, want 2", reads)
	}
	rereading := h.memoryReads(rh, env.chat, env.session, map[string]bool{"wb.xlsx": true})
	if _, _ = rereading.ReadAttachedSheet(ctx, "wb.xlsx", "Hiring Plan", 6, 40); reads != 3 {
		t.Errorf("asked to re-read, the file was not read: reads=%d", reads)
	}
}

// One plan per request (owner, 2026-11-13): the plan under way is worked,
// not planned again; a different request sets it aside in the session, and
// it can be taken up again.
func TestPlanTask_OnePlanPerRequest(t *testing.T) {
	env := newAITurnEnv(t, &recordingProvider{reply: "ok"})
	ctx := context.Background()
	run := func(args string) (string, error) {
		return runTool(ctx, env.chat, env.session, taskHooks{}, nil, aiassistant.PlanTaskTool, json.RawMessage(args))
	}
	stages := `[{"title":"Structure","items":["dimensions"]},{"title":"P&L","items":["rows"]}]`
	if _, err := run(`{"goal":"Build the P&L","stages":` + stages + `}`); err != nil {
		t.Fatal(err)
	}
	if _, err := run(`{"goal":"Build the P&L","stages":` + stages + `}`); err == nil || !strings.Contains(err.Error(), "already has its plan") {
		t.Fatalf("planning the same request again: %v, want it refused", err)
	}
	if p, _ := env.chat.TaskPlan(ctx, env.session); p == nil || p.Goal != "Build the P&L" || len(p.Stages) != 2 {
		t.Fatalf("the plan under way changed: %+v", p)
	}
	out, err := run(`{"goal":"Add the dashboards","new_request":true,"stages":[{"title":"Board","items":["board dashboard"]},{"title":"Teams","items":["team dashboards"]}]}`)
	if err != nil || !strings.Contains(out, "set aside") {
		t.Fatalf("a new request: %q %v", out, err)
	}
	cur, _ := env.chat.TaskPlan(ctx, env.session)
	earlier, _ := env.chat.EarlierPlans(ctx, env.session)
	if cur == nil || cur.Goal != "Add the dashboards" || len(earlier) != 1 || earlier[0].Plan.Goal != "Build the P&L" || earlier[0].State != "set_aside" {
		t.Fatalf("after a new request: current %+v, earlier %+v", cur, earlier)
	}
	if _, err := run(`{"resume_plan":1}`); err != nil {
		t.Fatal(err)
	}
	cur, _ = env.chat.TaskPlan(ctx, env.session)
	earlier, _ = env.chat.EarlierPlans(ctx, env.session)
	if cur.Goal != "Build the P&L" || len(earlier) != 1 || earlier[0].Plan.Goal != "Add the dashboards" {
		t.Fatalf("after resume: current %q, earlier %+v", cur.Goal, earlier)
	}
	// Re-cut because the developer asked: the same plan, its open stages new.
	if _, err := run(`{"goal":"Build the P&L","replace":true,"stages":[{"title":"Revenue","items":["revenue rows"]}]}`); err != nil {
		t.Fatal(err)
	}
	cur, _ = env.chat.TaskPlan(ctx, env.session)
	if len(cur.Stages) != 1 || cur.Stages[0].Title != "Revenue" {
		t.Errorf("after replace: %+v", cur.Stages)
	}
}

// remember keeps a note and an attachment's analysis in the memory.
func TestRememberTool(t *testing.T) {
	env := newAITurnEnv(t, &recordingProvider{reply: "ok"})
	ctx := context.Background()
	if _, err := env.docs.CreateDocument(ctx, env.session, "spec.txt", "text/plain", "x", false, nil); err != nil {
		t.Fatal(err)
	}
	run := func(args string) (string, error) {
		return runTool(ctx, env.chat, env.session, taskHooks{}, nil, aiassistant.RememberTool, json.RawMessage(args))
	}
	if _, err := run(`{"note":"Sales caps are per quarter"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := run(`{"attachment":"SPEC.txt","analysis":"three rules"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := run(`{"attachment":"nope.txt","analysis":"x"}`); err == nil {
		t.Error("an analysis of a file not attached was kept")
	}
	mem, _ := env.chat.Memory(ctx, env.session)
	if len(mem.Notes) != 1 || mem.Attachment("spec.txt") == nil || mem.Attachment("spec.txt").Analysis != "three rules" {
		t.Errorf("memory = %+v", mem)
	}
}

// Compact now: the earlier conversation becomes the memory's summary, the
// developer is told, and later turns send only what came after it.
func TestMemory_CompactNow(t *testing.T) {
	p := &recordingProvider{reply: "ok"}
	env := newAITurnEnv(t, p)
	ctx := context.Background()
	for _, m := range []string{"first request", "second request", "third request"} {
		if status, _ := env.send(m); status != http.StatusOK {
			t.Fatalf("send %q: %d", m, status)
		}
	}
	status, body := env.post("/api/ai/sessions/"+env.session+"/memory/compact", nil)
	if status != http.StatusOK || !strings.Contains(body, "Memory compacted") {
		t.Fatalf("compact: %d %s", status, body)
	}
	mem, _ := env.chat.Memory(ctx, env.session)
	if mem.Summary == "" || mem.SummaryUpTo == nil || mem.Compactions != 1 {
		t.Fatalf("memory after compaction: %+v", mem)
	}
	if status, _ := env.send("fourth request"); status != http.StatusOK {
		t.Fatal("send after compaction")
	}
	if pr := p.lastPrompt(); !strings.Contains(pr, "SUMMARY: the earlier conversation") {
		t.Error("the summary is not in the prompt after compaction")
	}
}

// "Re-read the workbook" names no file: the attached workbooks are read
// again, not every file (the word test held a backspace for \b and never
// matched an English word).
func TestRereadFiles_WorkbookWord(t *testing.T) {
	docs := []aiassistant.Document{{Filename: "plan.xlsx"}, {Filename: "policy.md"}}
	got := rereadFiles("Please re-read the workbook", docs)
	if !got["plan.xlsx"] || got["policy.md"] {
		t.Errorf("re-read = %v, want the workbook only", got)
	}
}
