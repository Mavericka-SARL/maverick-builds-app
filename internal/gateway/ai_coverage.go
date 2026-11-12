package gateway

// Plans that leave out part of the request (2026-11-07/08: in two AI builds of
// one workbook, six plans dropped requested items — two of four grids, ten of
// sixteen rows, one of five postings — and reached the developer as if
// complete). Before a plan for a request naming several things goes to the
// developer, one call of its own asks the model which requested items no step
// does; the plan goes back once with them. Checking a list against a plan is a
// narrower task than writing the plan, and the same model does it reliably.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

const coverageSystemPrompt = `You check a plan against the request it answers. List each distinct thing the request asks to be
created, changed or removed — a row, a metric, a grid, a page, a posting, a role — as the request names it. For
each, decide whether some step of the plan does it. Reply with JSON only: {"missing": ["<item, as the request names
it>", …]}, [] when every item has a step. Do not judge whether the steps are right, only whether each item has one.
These are not items:
- something the request only mentions as context, says to leave alone, or leaves for a later step;
- something it names as an input or a source to read from ("using X", "from X", "reads X", "based on X") without
  asking to create or change it;
- a heading, a section title or a blank row of a sheet: it holds no values of its own;
- something listed under "Already in the model", unless the request asks to change it;
- a check — compare, verify, reconcile, review: it is done by reading, never by a step of a plan.
A range ("rows 6-39", "every row of the sheet") is covered row by row: a step creating or changing the metric of a
row covers that row, whatever the step calls it.`

// manyItems: the request enumerates — numbered points, a list, or names
// several rows, grids or pages — and is long enough for a plan to drop part.
var manyItems = regexp.MustCompile(`(?m)(^\s*(\d+[.)]|[-*•])\s)|\brows?\s+\d+\s*[,–-]|\b(each|every|all)\b`)

func requestNamesSeveral(request string) bool {
	return len(request) >= 160 && manyItems.MatchString(request)
}

// planCoverage asks which items of the request — and of the task plan's
// current stage, when it has one — no step does, and, for a direct order,
// which steps do something the order did not ask for (nil when the request
// names one thing and is no order, or the check cannot be made).
func planCoverage(ctx context.Context, p providers.Provider, model, request string, stageItems []string, direct bool, steps []aiassistant.ProposalStep, built string) (missing, extra []string, usage providers.Usage) {
	if p == nil || (!direct && len(stageItems) == 0 && !requestNamesSeveral(request)) {
		return nil, nil, providers.Usage{}
	}
	var plan strings.Builder
	for i, s := range steps {
		params := string(s.Params)
		if len(params) > 360 {
			params = params[:360] + "…"
		}
		fmt.Fprintf(&plan, "%d. %s %s\n", i+1, s.Tool, params)
	}
	content := "Request:\n" + request
	if len(stageItems) > 0 {
		content += "\n\nItems of the stage this plan is for:\n- " + strings.Join(stageItems, "\n- ")
	}
	if built != "" {
		content += "\n\nAlready in the model:\n" + built
	}
	content += "\n\nPlan:\n" + plan.String()
	system := coverageSystemPrompt
	if direct {
		system += directExtraPrompt
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := p.Chat(cctx, providers.ChatRequest{
		Model:        model,
		SystemPrompt: system,
		Messages:     []providers.Message{{Role: "user", Content: content}},
	})
	if err != nil {
		return nil, nil, providers.Usage{}
	}
	missing, extra = parseCoverageReply(resp.Message.Content)
	if !direct {
		extra = nil
	}
	return missing, extra, resp.Usage
}

// directExtraPrompt adds, for a direct order, the steps it did not ask for.
const directExtraPrompt = `
The request is a direct order: the developer decided what to make or change. Also list, as "extra", each step
that does something the order did not ask for and does not need: {"missing": […], "extra": ["step N: what it does", …]}.
Not extra: a step the order needs to work (a grid for a metric it orders, placing such a metric on its grid, a
helper metric its formula reads, an ordering step), and a step for a row or item inside a range or group the order
names ("rows 6-39", "the P&L lines").`

// extraStepNum reads the step number of an "extra" item ("step 7: …").
var extraStepNum = regexp.MustCompile(`(?i)^\s*step\s*(\d+)`)

// unaskedChanges splits a direct order's extra steps: those that change what
// the model already holds — a formula rewritten, something deleted, values
// written or cleared — are left out by the server (0-based, with notes); the
// rest stay warnings. Inside a loader-only order, gpt-5.4-mini proposed five
// formula rewrites the order never asked for, and a busy developer confirms
// a warned plan (2026-11-11).
func unaskedChanges(extra []string, steps []aiassistant.ProposalStep) (drop map[int]bool, notes, rest []string) {
	for _, e := range extra {
		m := extraStepNum.FindStringSubmatch(e)
		n := 0
		if m != nil {
			_, _ = fmt.Sscanf(m[1], "%d", &n)
		}
		if n < 1 || n > len(steps) || !changesWhatExists(steps[n-1]) {
			rest = append(rest, e)
			continue
		}
		if drop == nil {
			drop = map[int]bool{}
		}
		drop[n-1] = true
		notes = append(notes, fmt.Sprintf("Left out by the server: %s — the order did not ask for it, and it changes what the model holds. Ask for it on its own if it is wanted.", strings.TrimSpace(e)))
	}
	return drop, notes, rest
}

// changesWhatExists reports a step that rewrites or removes what the model
// already holds.
func changesWhatExists(s aiassistant.ProposalStep) bool {
	switch {
	case s.Tool == "update_metric":
		var p struct {
			Formula *string `json:"formula"`
			IsInput *bool   `json:"is_input"`
		}
		return json.Unmarshal(s.Params, &p) == nil && (p.Formula != nil || p.IsInput != nil)
	case strings.HasPrefix(s.Tool, "delete_"), s.Tool == "write_input_values", s.Tool == "clear_input_values", s.Tool == "remove_grid_metric":
		return true
	}
	return false
}

// extraWarning is the plan's send-back for steps a direct order did not ask for.
func extraWarning(extra []string) string {
	return fmt.Sprintf("the developer gave a direct order, and the plan also does what it did not ask: %s — drop those steps, or say in your reply why the order needs them",
		strings.Join(extra, "; "))
}

// parseCoverage reads {"missing": […]} from a reply, tolerating text around it.
func parseCoverage(reply string) []string {
	missing, _ := parseCoverageReply(reply)
	return missing
}

// parseCoverageReply reads {"missing": […], "extra": […]}.
func parseCoverageReply(reply string) (missing, extra []string) {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end <= start {
		return nil, nil
	}
	var out struct {
		Missing []string `json:"missing"`
		Extra   []string `json:"extra"`
	}
	if json.Unmarshal([]byte(reply[start:end+1]), &out) != nil {
		return nil, nil
	}
	return nonBlank(out.Missing), nonBlank(out.Extra)
}

func nonBlank(list []string) []string {
	var out []string
	for _, m := range list {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// splitCoverage parts the coverage check's missing items into those of the
// request and those of the task plan's current stage (named by the stage's
// own wording when the checker reworded one).
func splitCoverage(missing, stageItems []string) (request, stage []string) {
	for _, m := range missing {
		hit := ""
		for _, it := range stageItems {
			if aiassistant.SameItem(m, it) {
				hit = it
				break
			}
		}
		switch {
		case hit == "":
			request = append(request, m)
		case !itemListed(stage, hit):
			stage = append(stage, hit)
		}
	}
	return request, stage
}

// itemListed reports whether item is in list (as SameItem reads them).
func itemListed(list []string, item string) bool {
	for _, x := range list {
		if aiassistant.SameItem(x, item) {
			return true
		}
	}
	return false
}

// coverageWarning is the plan's send-back for missing items.
func coverageWarning(missing []string) string {
	return fmt.Sprintf("the plan has no step for: %s — add them, or say in your reply which you leave out and why", strings.Join(missing, "; "))
}

// modelInventory lists what the revision already has, by kind, for the
// coverage check: items built by earlier stages were reported missing again
// ("the plan has no step for: Calendar…" two proposals after it was built,
// 2026-11-08). At most maxInventory characters.
func modelInventory(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, revID string) string {
	const maxInventory = 6000
	rows, err := q.Query(ctx, `
		SELECT 'Dimensions', name FROM model.dimension_def WHERE revision_id = $1::uuid
		UNION ALL SELECT 'Grids', name FROM model.grid_def WHERE revision_id = $1::uuid
		UNION ALL SELECT 'Tables', COALESCE(NULLIF(label,''), name) FROM model.form_def WHERE revision_id = $1::uuid
		UNION ALL SELECT 'Pages', name FROM model.dashboard_def WHERE revision_id = $1::uuid
		UNION ALL SELECT 'Workflows', name FROM workflow.workflow_def WHERE revision_id = $1::uuid
		UNION ALL SELECT 'Metrics', name || COALESCE(' ("' || NULLIF(label,'') || '")', '') FROM model.metric_def WHERE revision_id = $1::uuid`, revID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	kinds := []string{"Dimensions", "Grids", "Tables", "Pages", "Workflows", "Metrics"}
	byKind := map[string][]string{}
	for rows.Next() {
		var kind, name string
		if rows.Scan(&kind, &name) == nil {
			byKind[kind] = append(byKind[kind], name)
		}
	}
	var b strings.Builder
	for _, k := range kinds {
		if len(byKind[k]) == 0 {
			continue
		}
		line := k + ": " + strings.Join(byKind[k], ", ") + "\n"
		if b.Len()+len(line) > maxInventory {
			line = line[:max(0, maxInventory-b.Len())] + "…\n"
		}
		b.WriteString(line)
		if b.Len() >= maxInventory {
			break
		}
	}
	return b.String()
}
