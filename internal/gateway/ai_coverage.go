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

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

const coverageSystemPrompt = `You check a plan against the request it answers. List each distinct thing the request asks to be
created, changed or removed — a row, a metric, a grid, a page, a posting, a role — as the request names it. For
each, decide whether some step of the plan does it. Reply with JSON only: {"missing": ["<item, as the request names
it>", …]}, [] when every item has a step. Something the request only mentions as context, says to leave alone, or
leaves for a later step is not an item. Do not judge whether the steps are right, only whether each item has one.`

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
func planCoverage(ctx context.Context, p providers.Provider, model, request string, stageItems []string, direct bool, steps []aiassistant.ProposalStep) (missing, extra []string, usage providers.Usage) {
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
that does something the order did not ask for and does not need (a step it needs to work — a grid for a metric
it orders, an ordering step — is not extra): {"missing": […], "extra": ["step N: what it does", …]}.`

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

// coverageWarning is the plan's send-back for missing items.
func coverageWarning(missing []string) string {
	return fmt.Sprintf("the plan has no step for: %s — add them, or say in your reply which you leave out and why", strings.Join(missing, "; "))
}
