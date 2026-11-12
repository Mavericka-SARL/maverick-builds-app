package gateway

// Measurements on a real session (skipped unless PROMPT_DB and PROMPT_SESSION
// are set): what one AI turn sends, how much of a workbook plan_sheet
// translates, and the sheet_plan expansion — the numbers of 2026-11-12.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// Measures what one AI turn sends, on a real session (PROMPT_DB, PROMPT_SESSION).
func TestPromptSizes(t *testing.T) {
	dsn, sid := os.Getenv("PROMPT_DB"), os.Getenv("PROMPT_SESSION")
	if dsn == "" {
		t.Skip("PROMPT_DB not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	chat := aiassistant.NewChatStore(pool)
	sess, err := chat.GetSession(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	mc := aiassistant.FetchModelContext(ctx, pool, sess.ModelID, sess.DraftRevisionID)
	base := aiassistant.BuildSystemPrompt(mc)
	docs, _ := aiassistant.NewDocumentStore(pool).ListDocumentsWithContent(ctx, sid)
	dc := buildDocumentContext(docs)
	plan, _ := chat.TaskPlan(ctx, sid)
	brief := aiassistant.TurnBrief(aiassistant.RequestMode{Mode: aiassistant.ModeDelegated, Tasks: []string{"workbook_build"}}, plan)
	tools, _ := json.Marshal(aiassistant.TurnTools(aiassistant.RequestMode{Mode: aiassistant.ModeDelegated, Tasks: []string{"workbook_build"}}, "continue"))
	msgs, _ := chat.ListMessages(ctx, sid)
	msgs = append(msgs, aiassistant.ChatMessage{Role: "user", Content: "Confirmed. Continue with the next stage of the plan.", CreatedAt: time.Now()})
	hist := 0
	for _, m := range msgs {
		hist += len(m.Content)
		if m.ToolCalls != nil {
			b, _ := json.Marshal(m.ToolCalls)
			hist += len(b)
		}
	}
	targetPart := 0
	if plan != nil && plan.Target != nil {
		targetPart = len(plan.Target.Render())
	}
	t.Logf("chars: base prompt %d (model context part of it), documents %d (%d docs), turn brief %d (target %d), tools %d, stored history %d (%d messages)",
		len(base), len(dc), len(docs), len(brief), targetPart, len(tools), hist, len(msgs))
	static := aiassistant.BuildSystemPrompt(aiassistant.ModelContext{})
	stages := 0
	if plan != nil {
		stages = len(plan.Render()) - targetPart
	}
	t.Logf("static instructions %d, model context %d, plan stages+decisions %d, playbooks etc %d", len(static), len(base)-len(static), stages, len(brief)-stages-targetPart)
	t.Logf("≈ tokens (chars/4): system %d, tools %d, history %d", (len(base)+len(dc)+len(brief))/4, len(tools)/4, hist/4)
}

func TestPromptSizesTrimmed(t *testing.T) {
	dsn, sid := os.Getenv("PROMPT_DB"), os.Getenv("PROMPT_SESSION")
	if dsn == "" {
		t.Skip("PROMPT_DB not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	chat := aiassistant.NewChatStore(pool)
	sess, _ := chat.GetSession(ctx, sid)
	plan, _ := chat.TaskPlan(ctx, sid)
	cur := plan.Current()
	t.Logf("current stage %d %q sheet %q", cur+1, plan.Stages[cur].Title, plan.Stages[cur].Sheet)
	base := aiassistant.TrimPromptForSheet(aiassistant.BuildSystemPrompt(aiassistant.FetchModelContext(ctx, pool, sess.ModelID, sess.DraftRevisionID)))
	docs, _ := aiassistant.NewDocumentStore(pool).ListDocumentsWithContent(ctx, sid)
	wbs := map[string][]byte{}
	for _, d := range docs {
		if _, raw, err := aiassistant.NewDocumentStore(pool).GetRaw(ctx, sid, d.Filename); err == nil && raw != nil {
			wbs[d.Filename] = raw
		}
	}
	idx := buildDocumentIndex(docs, wbs)
	mode := aiassistant.RequestMode{Mode: aiassistant.ModeDelegated, Tasks: []string{"workbook_build"}}
	brief := aiassistant.TurnBrief(mode, plan)
	tools, _ := json.Marshal(aiassistant.SheetStageTools(aiassistant.TurnTools(mode, "continue")))
	sys := len(base) + len(idx) + len(brief)
	msgs, _ := chat.ListMessages(ctx, sid)
	msgs = append(msgs, aiassistant.ChatMessage{Role: "user", Content: "Confirmed. Continue with the next stage of the plan.", CreatedAt: time.Now()})
	var settled time.Time
	if props, err := aiassistant.NewProposalStore(pool).ListProposals(ctx, sid); err == nil {
		for _, p := range props {
			if p.ExecutedAt != nil && p.ExecutedAt.After(settled) {
				settled = *p.ExecutedAt
			}
		}
	}
	h := 0
	for _, m := range aiassistant.MessagesToProviderHistory(aiassistant.CompactHistory(aiassistant.SettleHistory(msgs, settled))) {
		h += len(m.Content)
		for _, c := range m.ToolCalls {
			h += len(c.Arguments)
		}
	}
	type big struct {
		role string
		n    int
		at   string
	}
	var bigs []big
	for _, m := range aiassistant.SettleHistory(msgs, settled) {
		n := len(m.Content)
		for _, c := range m.ToolCalls {
			n += len(c.Arguments)
		}
		if n > 4000 {
			bigs = append(bigs, big{m.Role, n, m.CreatedAt.Format("15:04:05")})
		}
	}
	h2 := 0
	for _, m := range aiassistant.MessagesToProviderHistory(aiassistant.CompactHistory(aiassistant.SettleHistory(msgs, time.Now().Add(-time.Second)))) {
		h2 += len(m.Content)
		for _, c := range m.ToolCalls {
			h2 += len(c.Arguments)
		}
	}
	t.Logf("history right after a confirmed plan: %d chars ≈ %d tokens", h2, h2/4)
	t.Logf("settled at %s; messages over 4000 chars after settling: %+v", settled.Format("15:04:05"), bigs)
	t.Logf("history replayed at the start of the next turn: %d chars ≈ %d tokens", h, h/4)
	t.Logf("TRIMMED chars: instructions %d, documents %d, brief %d (plan render %d), tools %d → ≈ %d tokens before history",
		len(base), len(idx), len(brief), len(plan.Render()), len(tools), (sys+len(tools))/4)
}

func TestPlanSheetCoverage(t *testing.T) {
	dsn, sid := os.Getenv("PROMPT_DB"), os.Getenv("PROMPT_SESSION")
	if dsn == "" {
		t.Skip("PROMPT_DB not set")
	}
	ctx := context.Background()
	pool, _ := pgxpool.New(ctx, dsn)
	defer pool.Close()
	chat := aiassistant.NewChatStore(pool)
	sess, _ := chat.GetSession(ctx, sid)
	plan, _ := chat.TaskPlan(ctx, sid)
	_, raw, err := aiassistant.NewDocumentStore(pool).GetRaw(ctx, sid, "High_Tech_Startup_Monthly_FPA_Model.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	pct := map[string]bool{}
	rows, _ := pool.Query(ctx, `SELECT lower(name) FROM model.metric_def WHERE revision_id=$1::uuid AND format='percentage'`, sess.DraftRevisionID)
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		pct[n] = true
	}
	rows.Close()
	labels := map[string]string{}
	rows, _ = pool.Query(ctx, `SELECT lower(name), COALESCE(label,'') FROM model.metric_def WHERE revision_id=$1::uuid`, sess.DraftRevisionID)
	for rows.Next() {
		var n, l string
		_ = rows.Scan(&n, &l)
		labels[n] = l
	}
	rows.Close()
	members := map[string]map[string]string{}
	rows, _ = pool.Query(ctx, `SELECT d.name, mb.code, COALESCE(mb.label,'') FROM model.dimension_def d JOIN model.dimension_member mb ON mb.dimension_id=d.id WHERE d.revision_id=$1::uuid AND COALESCE(d.dimension_type,'')<>'time'`, sess.DraftRevisionID)
	for rows.Next() {
		var d, c, l string
		_ = rows.Scan(&d, &c, &l)
		if members[d] == nil {
			members[d] = map[string]string{}
		}
		members[d][strings.ToLower(c)] = c
		if l != "" {
			members[d][strings.ToLower(l)] = c
		}
	}
	rows.Close()
	tot, totOK := 0, 0
	for _, s := range plan.Target.Sheets {
		out, err := aiassistant.PlanSheet(raw, aiassistant.SheetPlanInput{Sheet: s.Sheet, Target: plan.Target, Percent: pct, Scenario: "scenario", FirstYear: 2027, FirstMon: 1, Labels: labels, Members: members})
		if err != nil {
			t.Logf("%-18s %v", s.Sheet, err)
			continue
		}
		mapped, ok, inputs := 0, 0, 0
		reasons := map[string]int{}
		var sample []string
		for _, r := range out {
			if r.Metric != "" {
				mapped++
			}
			switch {
			case r.Input:
				inputs++
			case r.Formula != "":
				ok++
				if len(sample) < 2 {
					sample = append(sample, r.Metric+" = "+r.Formula)
				}
			default:
				k := r.Problem
				if len(k) > 60 {
					k = k[:60]
				}
				reasons[k]++
			}
		}
		calc := len(out) - inputs
		tot += calc
		totOK += ok
		t.Logf("%-18s rows %3d mapped %3d inputs %2d translated %3d/%3d  problems %v", s.Sheet, len(out), mapped, inputs, ok, calc, reasons)
		for _, x := range sample {
			t.Logf("      e.g. %s", x)
		}
	}
	t.Logf("TOTAL translated %d of %d calculated rows", totOK, tot)
}

func TestPlanSheetToolLive(t *testing.T) {
	dsn, sid := os.Getenv("PROMPT_DB"), os.Getenv("PROMPT_SESSION")
	if dsn == "" {
		t.Skip("PROMPT_DB not set")
	}
	ctx := context.Background()
	pool, _ := pgxpool.New(ctx, dsn)
	defer pool.Close()
	h := &handler{db: tenantdb.NewHandle(pool, nil)}
	out, err := h.aiPlanSheet(ctx, sid, "Balance Sheet", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan_sheet Balance Sheet:\n%s", out)
	sess, _ := aiassistant.NewChatStore(pool).GetSession(ctx, sid)
	params := json.RawMessage(`{"sheet":"Balance Sheet"}`)
	_, err = h.expandSheetPlans(ctx, sid, sess.DraftRevisionID, []aiassistant.ProposalStep{{Tool: "sheet_plan", Params: params}})
	t.Logf("expansion without fixes: %v", err)
	rows, _, _ := h.sheetPlanRows(ctx, sid, sess.DraftRevisionID, "Balance Sheet", nil)
	fixes := map[string]string{}
	for _, r := range rows {
		if r.Formula == "" && !r.Input && r.Metric != "" {
			fixes[r.Metric] = "0"
		}
	}
	fp, _ := json.Marshal(map[string]any{"sheet": "Balance Sheet", "fixes": fixes})
	steps, err := h.expandSheetPlans(ctx, sid, sess.DraftRevisionID, []aiassistant.ProposalStep{{Tool: "sheet_plan", Params: fp}})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, s := range steps {
		counts[s.Tool]++
	}
	t.Logf("expanded into %d steps %v; first: %s %s", len(steps), counts, steps[0].Tool, steps[0].Params)
}
