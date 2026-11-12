package aiassistant

// What the developer's message is, and what it calls for (owner, 2026-11-08):
// "the AI Developer must define if the user gives direct orders (like create
// dimension) or gives power to make decisions (like create a model based on
// this Excel); must have specific instructions for specific tasks; must be
// able to plan and split complex tasks". Each message is classified before
// the turn — a direct order, a delegated goal or a question — and named by
// task; the turn's instructions then carry that mode and the playbook of each
// task, and the task plan when there is one.

import (
	"encoding/json"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"regexp"
	"slices"
	"strings"
)

// Request modes.
const (
	ModeDirect    = "direct"
	ModeDelegated = "delegated"
	ModeQuestion  = "question"
)

// Task kinds, each with a playbook.
var TaskKinds = []string{"workbook_build", "workbook_fix", "pages", "records", "workflows", "access"}

// RequestMode is how a message is read.
type RequestMode struct {
	Mode  string   `json:"mode"`
	Tasks []string `json:"tasks"`
}

// ClassifierPrompt asks a model to classify one message.
const ClassifierPrompt = `You classify a developer's message to an AI that builds planning models (dimensions, metrics,
grids, pages, tables of records, workflows) from workbooks and descriptions. Reply with JSON only:
{"mode": "direct" | "delegated" | "question", "tasks": [...]}.
- direct: it names the objects and operations to make or change — create dimension Region with members …, rename
  metric X, set Y's formula to …, delete page Z, add a widget showing W. The developer has decided; the AI executes.
- delegated: it states a goal and leaves the how to the AI — build a model from this workbook, make the P&L match
  the sheet, add the pages people need, fix whatever differs, finish the actuals. The AI decides, plans, checks.
- question: it asks for information or an explanation, and asks for no change.
tasks: those of workbook_build (a model or part of one from a workbook or description), workbook_fix (numbers
that must match a workbook), pages (dashboards, widgets, the guide), records (tables of records, postings,
loaders and imports), workflows (processes, approvals, triggers), access (roles, who sees or changes what) that
the message is about; [] for none.`

// ParseClassification reads a classifier's reply.
func ParseClassification(reply string) (RequestMode, bool) {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end <= start {
		return RequestMode{}, false
	}
	var m RequestMode
	if json.Unmarshal([]byte(reply[start:end+1]), &m) != nil {
		return RequestMode{}, false
	}
	switch m.Mode {
	case ModeDirect, ModeDelegated, ModeQuestion:
	default:
		return RequestMode{}, false
	}
	m.Tasks = knownTasks(m.Tasks)
	return m, true
}

func knownTasks(tasks []string) []string {
	var out []string
	for _, t := range tasks {
		if slices.Contains(TaskKinds, t) && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

var (
	goalWords    = regexp.MustCompile(`(?i)\b(build|create|make|set up|model)\b.{0,40}\b(from|based on|out of)\b.{0,30}\b(workbook|excel|spreadsheet|file|sheet|attached)|\b(whatever|as needed|everything|all that|finish|complete the|match(es)? the (workbook|sheet))\b`)
	continuation = regexp.MustCompile(`(?i)^\s*(confirmed|continue|go on|next|proceed|ok|yes)\b`)
)

// ClassifyByRules reads a message without a model: a question when it asks,
// a goal when it leaves the how to the AI, a direct order otherwise. A short
// "confirmed, continue" while a plan has stages left goes on with the plan.
func ClassifyByRules(msg string, plan *TaskPlan) RequestMode {
	t := strings.TrimSpace(msg)
	switch {
	case plan.Current() >= 0 && continuation.MatchString(t) && len(t) < 400:
		return RequestMode{Mode: ModeDelegated}
	case strings.HasSuffix(t, "?") && !goalWords.MatchString(t):
		return RequestMode{Mode: ModeQuestion}
	case goalWords.MatchString(t):
		return RequestMode{Mode: ModeDelegated, Tasks: []string{"workbook_build"}}
	}
	return RequestMode{Mode: ModeDirect}
}

// TurnBrief is the turn's instructions for m, with plan when the session
// has one.
func TurnBrief(m RequestMode, plan *TaskPlan) string {
	var b strings.Builder
	b.WriteString("## This request\n")
	switch m.Mode {
	case ModeDirect:
		b.WriteString(directBrief)
	case ModeQuestion:
		b.WriteString(questionBrief)
	default:
		b.WriteString(delegatedBrief)
	}
	// A direct order is done as written: the playbooks, which say what a
	// goal calls for, would invite more than it asks.
	for _, t := range m.Tasks {
		if m.Mode == ModeDirect {
			break
		}
		if p := playbooks[t]; p != "" {
			b.WriteString("\n")
			b.WriteString(p)
		}
	}
	if r := plan.Render(); r != "" {
		b.WriteString("\n### Your task plan (plan_task, update_task_plan)\n")
		b.WriteString(r)
		switch {
		case len(plan.Stages) == 0:
			// Decisions only: they say all there is.
		case plan.Current() >= 0 && m.Mode == ModeDirect:
			b.WriteString("The developer's order comes first: do exactly what it asks now, whether or not it is the CURRENT stage, and never argue it with the plan or add the plan's work to it. " +
				"The plan stays as it is and goes on after; mark a stage the order finishes with update_task_plan.\n")
		case plan.Current() >= 0:
			b.WriteString("Work on the CURRENT stage: one proposal for it (or for its next part), then check it and mark it with update_task_plan. A new goal: plan_task adds its stages after these.\n")
		default:
			b.WriteString("Every stage is done or skipped: report what was built and checked, and what the developer has to do (a business admin's access rules, values to type); " +
				"for another goal, suggest a new session.\n")
		}
	}
	return b.String()
}

const directBrief = `The developer gave a DIRECT ORDER: they decided what to make or change. Do exactly that — the
objects, names, values and places as written — in one plan, and nothing beyond it: no extra metrics, renames, pages,
layouts or "improvements" (name in your reply anything you would add, for them to order). Ask only when the order
lacks a value it needs or reads two ways. A direct order needs no task plan.
`

const delegatedBrief = `The developer DELEGATED A GOAL: the decisions within it are yours — structure, names, labels,
formats, placement, order — following the workbook and the playbooks below. First make sure you have what the goal
needs: the files it names (a month's actuals, a workbook), the values it depends on. Something missing — a file not
attached, a period the attached data does not hold — is asked for before planning, never planned around. Before building anything that takes more
than one plan, call plan_task {"goal", "stages": [{"title", "items": [what the stage delivers, named], "check": how you
will check it}]}: stages in build order (what a later stage reads comes first), each one proposal of at most a few
dozen steps. Then propose the current stage; once it is confirmed, check it as the stage says (compare_with_sheet,
describe_dashboard, list_*), mark it with update_task_plan {"stage", "status", "note"} and propose the next. Take
ordinary defaults yourself and list them in your reply; ask only where the workbook leaves a choice that changes
numbers (then propose the rest). Report at the end of each stage: what it built, what the check showed, what is next.
`

const questionBrief = `The developer ASKED A QUESTION: answer it from the model and the attached files (read tools),
plainly and briefly. Propose nothing unless they ask for a change.
`

// playbooks are the specific instructions of each task kind.
var playbooks = map[string]string{
	"workbook_build": `### Playbook: a model from a workbook
1. Read the workbook first: the Guide (owners, process), every sheet's layout and formulas (read_attached_sheet).
2. Plan the stages: dimensions and members; inputs and settings with their grids; data (imports, tables of
   records, postings); calculations sheet by sheet in the workbook's order; pages; the process (workflows);
   who opens which page. Each sheet people read gets a page or a place on one.
3. Every metric shown gets the workbook's row text as its label, its number format (decimals, currency,
   percentages in percent units); the workbook's own tests and guards stay in the formulas. Every metric and
   grid gets the tag of its sheet's area ("tags" on create): a model of hundreds is navigated by them.
4. After each calculation stage, compare_with_sheet every row of the sheets it built; a stage is done when they
   match, not when its metrics exist. Never type a workbook value into a calculated metric.
5. Who may change which inputs is a business admin's to set: list it in your report, per the Guide.
`,
	"workbook_fix": `### Playbook: numbers that must match a workbook
1. compare_with_sheet the whole sheet first ("rows": every row) to see what differs and from which period.
2. Read the differing rows' workbook formulas (read_attached_sheet) and the model's; find the root cause, which is
   often one shared input or test feeding many rows: fix it there, once.
3. Copy the workbook's own logic (its closed-month test, its guards); never type a workbook value into a
   calculated metric, never "if the actual is 0, use the plan", and never move an input (an assumption, a driver)
   until a row matches: inputs change only where they differ from their own row in the workbook.
4. Compare again after the fix; report rows matching before and after.
`,
	"pages": `### Playbook: pages
1. A page per sheet people read, or a place on a page with the sheets it belongs with; KPI pages show what the
   workbook's dashboard sheet shows (compare_with_sheet its rows to pick the metrics).
2. Each page in the plan that creates it: its widgets, arrange_dashboard, reorder_dashboards, and
   set_role_dashboards for the roles that use it.
3. Grids show labelled metrics (the workbook's row text), the workbook's formats, a title with the unit; charts
   name their series (series_names) and mark actuals (actual_metric_id).
   One grid, one widget: what a page shows from one grid with the same rows, columns and scale goes in one
   widget (its metrics, or the rows' members, together), not a widget per metric or per group of rows — two
   only for a different number scale or a group that needs its own total row.
4. A change to a text (the guide) changes only the lines asked: keep the rest word for word.
`,
	"records": `### Playbook: tables of records, postings and loads
1. A table's numbers reach the model through postings; each maps every dimension of its target grid.
2. A register keeping several measures in one table (a code column): one metric and one posting per measure, with
   row_filter on the code. A workbook lookup through a mapping sheet: a property of the dimension, read with
   SUMIFS(metric, {Dim}.property, "value"); give the mapping's owner editor_roles on that dimension.
3. A loader of a file that is loaded again (a month's actuals): replace_by its period field.
4. Counting records (an import check): aggregation "count" on a grid with only the dimensions mapped.
5. After a change, backfill and compare what the postings feed.
`,
	"workflows": `### Playbook: workflows and triggers
1. Steps name roles exactly as list_workflow_roles gives them; a workflow is a draft until published
   (publish_workflow_def as the plan's last step when it should run).
2. A start button on a page (automation_button) for each process people start; the guide says who does what.
`,
	"access": `### Playbook: roles and access
1. Who opens which page: set_role_dashboards (keeps a role's pages and adds). Who may change a dimension's
   members: update_dimension "editor_roles".
2. Which inputs each role may change (role access rules) is a business admin's to set: list them per role, as
   the workbook's Guide names the owners, for the business admin.
`,
}

// connectorTools are read tools only a request about connectors, exports or
// outside sources needs.
var connectorTools = map[string]bool{
	"test_integration": true, "list_connections": true, "list_model_link_sources": true,
	"attach_google_sheet": true, "preview_export": true,
}

// connectorWords: a request naming a connector, an export or an outside
// source keeps every tool.
var connectorWords = regexp.MustCompile(`(?i)\b(api|rest|connector|connection|oauth|google|sheets?\s+url|export|link(ed)?\s+model|sftp|webhook|endpoint)\b`)

// TurnTools are the tools a turn offers: every one, except the connector
// tools when the request is about building, fixing, pages, workflows or
// access and names no connector — each call re-sends the tool list, and a
// build never used them (2026-11-11). A question keeps every tool.
func TurnTools(m RequestMode, request string) []providers.ToolDef {
	all := AllTools()
	if m.Mode == ModeQuestion || len(m.Tasks) == 0 || connectorWords.MatchString(request) {
		return all
	}
	for _, t := range m.Tasks {
		switch t {
		case "workbook_build", "workbook_fix", "pages", "workflows", "access":
		default:
			return all
		}
	}
	out := make([]providers.ToolDef, 0, len(all))
	for _, t := range all {
		if !connectorTools[t.Name] {
			out = append(out, t)
		}
	}
	return out
}
