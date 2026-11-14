package aiassistant

import (
	"strings"
	"testing"
	"time"
)

// The developer asks for an attachment to be read again only in words that
// say so (owner, 2026-11-13: read once, again only when asked directly).
func TestAsksToReread(t *testing.T) {
	yes := []string{
		"Please re-read the hiring policy file",
		"reread the workbook",
		"Read the Hiring Plan sheet again",
		"go through the attachment once more",
		"re-analyse the workbook, I changed it",
		"take a fresh look at the file",
		"Перечитай файл с политикой",
		"прочитай таблицу заново",
		"Проанализируй ещё раз вложение",
		"повторно прочитай документ",
	}
	no := []string{
		"List the first five hires in the Hiring Plan sheet",
		"build the P&L from the workbook",
		"what does the policy say about on-cost?",
		"Read the manual on formulas",
		"Покажи строки листа Headcount",
	}
	for _, m := range yes {
		if !AsksToReread(m) {
			t.Errorf("AsksToReread(%q) = false, want true", m)
		}
	}
	for _, m := range no {
		if AsksToReread(m) {
			t.Errorf("AsksToReread(%q) = true, want false", m)
		}
	}
}

// A part of an attachment read once answers every later request it covers.
func TestFindReadCoversRows(t *testing.T) {
	m := &SessionMemory{}
	m.PutRead(MemoryRead{Tool: "read_attached_sheet", Key: "read_attached_sheet|doc1|plan|6|40", File: "wb.xlsx", Sheet: "Plan", FromRow: 6, ToRow: 40, Result: "rows"})
	m.PutRead(MemoryRead{Tool: "read_attached_sheet", Key: "read_attached_sheet|doc1|whole|0|0", File: "wb.xlsx", Sheet: "Whole", Result: "all"})
	cases := []struct {
		sheet    string
		from, to int
		want     bool
	}{
		{"Plan", 6, 40, true}, {"plan", 10, 20, true}, {"Plan", 1, 20, false}, {"Plan", 6, 0, false}, {"Plan", 30, 50, false},
		{"Whole", 1, 999, true}, {"Whole", 0, 0, true}, {"Other", 1, 5, false},
	}
	for _, c := range cases {
		key := "read_attached_sheet|doc1|" + strings.ToLower(c.sheet) + "|x"
		if got := m.FindRead("read_attached_sheet", key, "doc1", c.sheet, c.from, c.to) != nil; got != c.want {
			t.Errorf("FindRead(%s %d-%d) found=%v, want %v", c.sheet, c.from, c.to, got, c.want)
		}
	}
	// Another upload of the file is another document: nothing of the old one
	// answers for it.
	if m.FindRead("read_attached_sheet", "k", "doc2", "Plan", 6, 40) != nil {
		t.Error("a read of an earlier upload answered for a new one")
	}
	m.ForgetDocument("doc1")
	if len(m.Reads) != 0 {
		t.Errorf("reads kept after the document was removed: %d", len(m.Reads))
	}
}

// The memory in the prompt holds the analyses and notes whole, the reads by
// name only; the conversation after the summary is what goes on being sent.
func TestMemoryRenderAndSummaryCut(t *testing.T) {
	m := &SessionMemory{}
	m.SetAttachment("policy.md", "d1", "On-cost 21.5% in Germany", "ai")
	m.AddNote("percentages are stored in percent units")
	m.PutRead(MemoryRead{Tool: "read_attached_sheet", Key: "k", File: "wb.xlsx", Sheet: "Plan", FromRow: 1, ToRow: 9, Result: "SECRET-ROWS"})
	out := m.Render([]EarlierPlan{{Plan: TaskPlan{Goal: "Old goal", Stages: []TaskStage{{Title: "a", Status: StageDone}}}, State: "finished"}})
	for _, want := range []string{"On-cost 21.5% in Germany", "percent units", "wb.xlsx › Plan, rows 1–9", "1. Old goal — 1 of 1 stages done, finished", "resume_plan"} {
		if !strings.Contains(out, want) {
			t.Errorf("memory render lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SECRET-ROWS") {
		t.Error("a read's content went into the prompt; only its name should")
	}
	if m.AddNote("Percentages are stored in percent units") {
		t.Error("the same note was kept twice")
	}

	t0 := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	msgs := []ChatMessage{
		{Role: "user", Content: "one", CreatedAt: t0},
		{Role: "assistant", Content: "a", CreatedAt: t0.Add(time.Minute)},
		{Role: "user", Content: "two", CreatedAt: t0.Add(2 * time.Minute)},
		{Role: "assistant", Content: "b", CreatedAt: t0.Add(3 * time.Minute)},
		{Role: "user", Content: "three", CreatedAt: t0.Add(4 * time.Minute)},
	}
	split := CompactSplit(msgs)
	if split != 2 {
		t.Fatalf("CompactSplit = %d, want 2 (the last %d developer messages stay)", split, CompactKeepTurns)
	}
	upTo := msgs[split-1].CreatedAt
	m.SummaryUpTo = &upTo
	if got := m.SinceSummary(msgs); len(got) != 3 || got[0].Content != "two" {
		t.Errorf("SinceSummary = %+v, want from \"two\"", got)
	}
	st := m.Status(1000, nil)
	if st.UsedTokens < 1000 || st.Percent != 100*st.UsedTokens/st.CapacityTokens || len(st.Attachments) != 1 {
		t.Errorf("status = %+v", st)
	}
}

// Compaction shortens the reads of sheets no open stage needs, and keeps
// those an open stage builds.
func TestCompactReadsKeepsOpenStageSheets(t *testing.T) {
	long := strings.Repeat("x", 5000)
	m := &SessionMemory{Reads: []MemoryRead{
		{Tool: "read_attached_sheet", Key: "a", Sheet: "Hiring Plan", Result: long},
		{Tool: "read_attached_sheet", Key: "b", Sheet: "Revenue", Result: long},
	}}
	if n := m.CompactReads(map[string]bool{"revenue": true}); n != 1 {
		t.Fatalf("shortened %d reads, want 1", n)
	}
	if !m.Reads[0].Compacted || m.Reads[1].Compacted || len(m.Reads[1].Result) != 5000 {
		t.Errorf("reads after compaction: %+v / %+v", m.Reads[0].Compacted, m.Reads[1].Compacted)
	}
}
