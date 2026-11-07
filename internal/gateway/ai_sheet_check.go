package gateway

// Check before "done" (2026-11-07): the AI Developer twice called sheets
// complete when a third of their rows did not match — Actual_PnL held 10 of
// the workbook's 36 rows — and only an outside comparison showed it. A reply
// that says an attached sheet is done is sent back once when the session's
// compare_with_sheet results do not show that sheet matching: none of its
// rows compared, or rows whose last comparison differs.

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// doneClaim is a reply saying work is finished or reproduces the source.
var doneClaim = regexp.MustCompile(`(?i)\b(complete[ds]?|done|finished|fully|all (?:the )?rows|reproduc(?:e|es|ed|ing)|populated|match(?:es|ed|ing)? the (?:workbook|sheet))\b`)

// compareLine is one row a compare_with_sheet result reports.
var compareLine = regexp.MustCompile(`against sheet "([^"]+)" row (\d+): (\d+) of (\d+) periods match`)

// sheetDoneNudge is what a reply claiming an attached sheet done is sent
// back with, or "" when it claims none or the session's comparisons show
// every sheet it names matching.
func (h *handler) sheetDoneNudge(ctx context.Context, store *aiassistant.ChatStore, sessionID, reply string) string {
	if !doneClaim.MatchString(reply) {
		return ""
	}
	sheets := h.attachedSheetNames(ctx, sessionID)
	low := strings.ToLower(reply)
	var named []string
	for _, s := range sheets {
		if len(s) >= 3 && strings.Contains(low, strings.ToLower(s)) {
			named = append(named, s)
		}
	}
	if len(named) == 0 {
		return ""
	}
	msgs, err := store.ListMessages(ctx, sessionID)
	if err != nil {
		return ""
	}
	return sheetDoneProblems(named, sheetComparisons(msgs))
}

// rowResult is one row's last comparison: periods matching of compared.
type rowResult struct{ matched, compared int }

// sheetComparisons are the last compare_with_sheet result of each row of
// each sheet (lower-cased) in msgs. A tool message stores its call's id, not
// its name: the name is the assistant's call of that id.
func sheetComparisons(msgs []aiassistant.ChatMessage) map[string]map[int]rowResult {
	nameOf := map[string]string{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			nameOf[tc.ID] = tc.Name
		}
	}
	last := map[string]map[int]rowResult{}
	for _, m := range msgs {
		if m.Role != "tool" || firstNonEmpty(m.ToolName, nameOf[m.ToolCallID]) != "compare_with_sheet" {
			continue
		}
		for _, g := range compareLine.FindAllStringSubmatch(m.Content, -1) {
			row, _ := strconv.Atoi(g[2])
			matched, _ := strconv.Atoi(g[3])
			compared, _ := strconv.Atoi(g[4])
			key := strings.ToLower(g[1])
			if last[key] == nil {
				last[key] = map[int]rowResult{}
			}
			last[key][row] = rowResult{matched, compared}
		}
	}
	return last
}

// sheetDoneProblems is the nudge for sheets named done whose comparisons in
// last do not show them matching, or "".
func sheetDoneProblems(named []string, last map[string]map[int]rowResult) string {
	var problems []string
	for _, s := range named {
		rows := last[strings.ToLower(s)]
		if len(rows) == 0 {
			problems = append(problems, fmt.Sprintf("%s: no row of it compared yet", s))
			continue
		}
		var bad []int
		for row, r := range rows {
			if r.matched < r.compared {
				bad = append(bad, row)
			}
		}
		if len(bad) > 0 {
			sort.Ints(bad)
			parts := make([]string, len(bad))
			for i, row := range bad {
				parts[i] = fmt.Sprintf("row %d (%d of %d periods)", row, rows[row].matched, rows[row].compared)
			}
			problems = append(problems, fmt.Sprintf("%s: %s differ at their last comparison", s, strings.Join(parts, ", ")))
		}
	}
	if len(problems) == 0 {
		return ""
	}
	return "(Sent by the server, not the developer.) Your reply calls work done that the comparisons do not show done:\n- " +
		strings.Join(problems, "\n- ") +
		"\nCheck each sheet you name by value with compare_with_sheet (\"rows\": every line of the sheet in one call), fix the rows " +
		"that differ, and say how many rows match. If some rows are not done, say which plainly instead of calling the sheet complete."
}

// attachedSheetNames are the worksheet names of the session's attached
// workbooks.
func (h *handler) attachedSheetNames(ctx context.Context, sessionID string) []string {
	docs, err := aiassistant.NewDocumentStore(h.db.For(ctx)).ListDocuments(ctx, sessionID)
	if err != nil {
		return nil
	}
	var names []string
	for _, d := range docs {
		if !d.Importable || !strings.HasSuffix(strings.ToLower(d.Filename), ".xlsx") && !strings.HasSuffix(strings.ToLower(d.Filename), ".xlsm") {
			continue
		}
		_, raw, err := aiassistant.NewDocumentStore(h.db.For(ctx)).GetRaw(ctx, sessionID, d.ID)
		if err != nil {
			continue
		}
		f, err := excelize.OpenReader(bytes.NewReader(raw))
		if err != nil {
			continue
		}
		names = append(names, f.GetSheetList()...)
		_ = f.Close()
	}
	return names
}
