package gateway

// Two guards found building the Creator & Agency workbook with the AI
// Developer (2026-11-08):
//
//   - Asked to fix rows that differed from the workbook, it overwrote two
//     Base assumptions (paid marketing 5,300 → 3,919.07, admin & travel
//     1,700 → 1,747.31) so one closed month matched, and broke every month of
//     three sheets. A value step that changes values already in an input goes
//     back once, naming them (inputOverwriteWarnings).
//   - The build moved only once each confirmed stage was checked by value and
//     the exact differences went back to the model. The server now does that
//     itself: the turn after a confirmed plan starts with every row the
//     session compared before compared again, the rows the plan broke named,
//     and the metrics it made that nothing compared yet listed
//     (stageValueCheck).

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// inputOverwriteWarnings names each write_input_values step that changes
// values an input already holds, unless the developer's request names that
// input (its name or label): an input is the source's assumption, and a
// calculated row that differs is fixed in its formula, never by moving an
// input until it matches. Read before the plan runs, on q.
func inputOverwriteWarnings(ctx context.Context, q *pgxpool.Pool, modelID, revID string, steps []aiassistant.ProposalStep, request string) []string {
	low := strings.ToLower(request)
	dimIDs := map[string]string{} // lower-cased dimension name → id
	if rows, err := q.Query(ctx, `SELECT id::text, lower(name) FROM model.dimension_def WHERE revision_id = $1::uuid`, revID); err == nil {
		for rows.Next() {
			var id, name string
			if rows.Scan(&id, &name) == nil {
				dimIDs[name] = id
			}
		}
		rows.Close()
	}
	store := calculation.NewStoreOn(q)
	var out []string
	for i, step := range steps {
		if step.Tool != "write_input_values" {
			continue
		}
		var p struct {
			MetricID string `json:"metric_id"`
			Values   []struct {
				Members map[string]string `json:"members"`
				Value   json.RawMessage   `json:"value"`
			} `json:"values"`
		}
		if json.Unmarshal(step.Params, &p) != nil || len(p.Values) == 0 {
			continue
		}
		var metricID, name, label string
		if q.QueryRow(ctx, `
			SELECT id::text, name, COALESCE(label,'') FROM model.metric_def
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND is_input AND (id::text=$3 OR lower(name)=lower($3))`,
			modelID, revID, strings.TrimSpace(p.MetricID)).Scan(&metricID, &name, &label) != nil {
			continue // made by this plan, or not an input: nothing to overwrite
		}
		if namesMetric(low, name, label) {
			continue // the developer named it
		}
		current, err := store.LoadInputValueMap(ctx, modelID, revID, metricID)
		if err != nil || len(current) == 0 {
			continue
		}
		var changes []string
		n := 0
		for _, v := range p.Values {
			want, err := strconv.ParseFloat(strings.Trim(strings.TrimSpace(string(v.Value)), `"`), 64)
			if err != nil {
				continue
			}
			at := map[string]string{}
			codes := make([]string, 0, len(v.Members))
			for dim, code := range v.Members {
				id, ok := dimIDs[strings.ToLower(strings.TrimSpace(dim))]
				if !ok {
					at = nil
					break
				}
				at[id] = code
				codes = append(codes, code)
			}
			if at == nil {
				continue
			}
			key, _ := json.Marshal(at)
			had, ok := current[string(key)]
			if !ok || had == 0 || math.Abs(had-want) <= 1e-9*math.Max(1, math.Abs(had)) {
				continue
			}
			n++
			if len(changes) < 3 {
				sort.Strings(codes)
				changes = append(changes, fmt.Sprintf("%s: %s → %s", strings.Join(codes, " · "),
					strconv.FormatFloat(had, 'f', -1, 64), strconv.FormatFloat(want, 'f', -1, 64)))
			}
		}
		if n == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("step %d (write_input_values %s): changes %d value(s) the input already holds (%s). "+
			"An input holds the source's assumption: when a calculated row differs from the workbook, its formula or the input row it reads is wrong — "+
			"fix that; never move an input until a row matches. If the developer asked for these values, propose the step again unchanged",
			i+1, name, n, strings.Join(changes, "; ")))
	}
	return out
}

// calcToInputWarnings names each update_metric step that turns a calculated
// metric into an input, unless the developer's request names that metric or
// asks for an input: its formula goes, and the values typed into it stop
// following what it was calculated from. Asked to fix three Operating Costs
// rows that differed, gpt-5-mini proposed exactly that — three calculated
// rows made inputs and the workbook's 24 months typed into each — and the
// plan reached the developer with no warning (2026-11-09).
func calcToInputWarnings(ctx context.Context, q *pgxpool.Pool, modelID, revID string, steps []aiassistant.ProposalStep, request string) []string {
	low := strings.ToLower(request)
	var out []string
	for i, step := range steps {
		if step.Tool != "update_metric" {
			continue
		}
		var p struct {
			MetricID string `json:"metric_id"`
			IsInput  *bool  `json:"is_input"`
		}
		if json.Unmarshal(step.Params, &p) != nil || p.IsInput == nil || !*p.IsInput {
			continue
		}
		var name, label, formulaText string
		if q.QueryRow(ctx, `
			SELECT name, COALESCE(label,''), COALESCE(btrim(formula),'') FROM model.metric_def
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND NOT is_input AND (id::text=$3 OR lower(name)=lower($3))`,
			modelID, revID, strings.TrimSpace(p.MetricID)).Scan(&name, &label, &formulaText) != nil || formulaText == "" {
			continue
		}
		if namesMetric(low, name, label) || strings.Contains(low, "input") {
			continue // the developer asked for it
		}
		out = append(out, fmt.Sprintf("step %d (update_metric %s): turns the calculated metric %s into an input, dropping its formula %s — "+
			"values typed into it no longer follow what it is calculated from. A calculated row that differs from the workbook is fixed in its formula "+
			"(or the input row it reads); never by typing the workbook's values. If the developer asked for an input, propose the step again unchanged",
			i+1, name, name, formulaText))
	}
	return out
}

// namesMetric reports whether a (lower-cased) request names a metric: its
// name as written or with spaces for underscores ("monthly paid marketing"),
// or its label without a trailing unit in brackets.
func namesMetric(low, name, label string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	label = strings.ToLower(strings.TrimSpace(label))
	if i := strings.LastIndex(label, "("); i > 0 {
		label = strings.TrimSpace(label[:i])
	}
	for _, c := range []string{name, strings.ReplaceAll(name, "_", " "), label} {
		if len(c) >= 4 && strings.Contains(low, c) {
			return true
		}
	}
	return false
}

// stageCheckHead starts the server's value check in the chat after a
// confirmed plan; handoverHead starts the one a session's first turn gets
// for the rows its earlier sessions compared. The next check runs only after
// a plan confirmed since the last of either.
const (
	stageCheckHead = "(Value check by the server after the plan you confirmed.)"
	handoverHead   = "(Handover by the server: the rows your earlier sessions in this draft compared with the source, compared again.)"
)

// maxStageCheckRows bounds how many rows one check compares again.
const maxStageCheckRows = 240

// compareSpec is one row a compare_with_sheet call of the session compared.
type compareSpec struct {
	File        string            `json:"file"`
	Sheet       string            `json:"sheet"`
	Row         int               `json:"row"`
	FirstColumn string            `json:"first_column"`
	Metric      string            `json:"metric"`
	Members     map[string]string `json:"members,omitempty"`
}

// sessionCompareRows are the rows the session's compare_with_sheet calls
// compared, the last call for each row and members winning, in the order
// first compared.
func sessionCompareRows(history []aiassistant.ChatMessage) []compareSpec {
	var order []string
	last := map[string]compareSpec{}
	for _, m := range history {
		for _, tc := range m.ToolCalls {
			if tc.Name != "compare_with_sheet" {
				continue
			}
			var a struct {
				File        string            `json:"file"`
				Sheet       string            `json:"sheet"`
				Row         int               `json:"row"`
				FirstColumn string            `json:"first_column"`
				Metric      string            `json:"metric"`
				Members     map[string]string `json:"members"`
				Rows        []compareRowArg   `json:"rows"`
			}
			if json.Unmarshal(tc.Arguments, &a) != nil {
				continue
			}
			rows := a.Rows
			if len(rows) == 0 && a.Row > 0 && a.Metric != "" {
				rows = []compareRowArg{{Row: a.Row, Metric: a.Metric, Members: a.Members}}
			}
			for _, r := range rows {
				spec := compareSpec{File: a.File, Sheet: a.Sheet, Row: r.Row, FirstColumn: cmpOr(r.FirstColumn, a.FirstColumn),
					Metric: r.Metric, Members: r.Members}
				if spec.Members == nil {
					spec.Members = a.Members
				}
				if spec.Row <= 0 || strings.TrimSpace(spec.Metric) == "" || spec.FirstColumn == "" {
					continue
				}
				key := strings.ToLower(spec.File + "\x00" + spec.Sheet + "\x00" + strconv.Itoa(spec.Row) + "\x00" + membersKey(spec.Members))
				if _, seen := last[key]; !seen {
					order = append(order, key)
				}
				last[key] = spec
			}
		}
	}
	out := make([]compareSpec, 0, len(order))
	for _, k := range order {
		out = append(out, last[k])
	}
	return out
}

type compareRowArg struct {
	Row         int               `json:"row"`
	Metric      string            `json:"metric"`
	Members     map[string]string `json:"members,omitempty"`
	FirstColumn string            `json:"first_column,omitempty"`
}

func cmpOr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func membersKey(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, strings.ToLower(k)+"="+m[k])
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// earlierHistory is the messages of the sessions sessionID goes on with
// (ChatStore.EarlierSessions), oldest first: their comparisons are checked
// again as this session's own — a session ends at its call limit, and the
// rows it checked used to end with it.
func earlierHistory(ctx context.Context, store *aiassistant.ChatStore, sessionID, carriedFrom string) []aiassistant.ChatMessage {
	ids, err := store.EarlierSessions(ctx, sessionID, carriedFrom)
	if err != nil {
		return nil
	}
	var out []aiassistant.ChatMessage
	for _, id := range ids {
		if msgs, err := store.ListMessages(ctx, id); err == nil {
			out = append(out, msgs...)
		}
	}
	return out
}

// stageValueCheck is the server's value check at the start of a turn, in a
// session with an attached file: after a plan confirmed since the last check,
// or on the session's first turn when earlier sessions in its draft compared
// rows (a handover). It returns the note to show the developer and give the
// model, or "".
func (h *handler) stageValueCheck(ctx context.Context, store *aiassistant.ChatStore, ps *aiassistant.ProposalStore, exec *aiassistant.ToolExecutor,
	sessionID, revID string, history []aiassistant.ChatMessage, plan *aiassistant.TaskPlan, status func(string)) string {
	docs, err := aiassistant.NewDocumentStore(h.db.For(ctx)).ListDocuments(ctx, sessionID)
	if err != nil {
		return ""
	}
	attached := false
	for _, d := range docs {
		attached = attached || d.Importable
	}
	if !attached {
		return ""
	}
	var lastCheck time.Time
	userTurns := 0
	for _, m := range history {
		if m.Role == "assistant" && (strings.HasPrefix(m.Content, stageCheckHead) || strings.HasPrefix(m.Content, handoverHead)) {
			lastCheck = m.CreatedAt
		}
		if m.Role == "user" {
			userTurns++
		}
	}
	props, err := ps.ListProposals(ctx, sessionID)
	if err != nil {
		return ""
	}
	var fresh []aiassistant.Proposal
	for _, p := range props {
		if (p.Status == "executed" || p.Status == "partial") && p.ExecutedAt != nil && p.ExecutedAt.After(lastCheck) {
			fresh = append(fresh, p)
		}
	}
	var earlier []aiassistant.ChatMessage
	if len(fresh) > 0 || userTurns == 1 {
		carried := ""
		if plan != nil {
			carried = plan.CarriedFrom
		}
		earlier = earlierHistory(ctx, store, sessionID, carried)
	}
	handover := len(fresh) == 0 && userTurns == 1 && len(sessionCompareRows(earlier)) > 0
	if len(fresh) == 0 && !handover {
		return ""
	}
	all := append(append([]aiassistant.ChatMessage{}, earlier...), history...)
	status("checking values against the source")
	head := stageCheckHead
	if handover {
		head = handoverHead
	}
	if !h.waitRecalculated(ctx, revID, 2*time.Minute) {
		return head + " A recalculation is still running, so the values were not checked: compare the rows changed before calling them done."
	}

	specs := sessionCompareRows(all)
	before := sheetComparisons(all)
	// The workbook map's rows of every sheet the confirmed plans' metrics
	// were mapped on: rows compared in earlier sessions or drafts, and rows
	// on the same sheet the plans may have broken.
	if modelID, mErr := h.modelOfRevision(ctx, revID); mErr == nil {
		if mapRows, lErr := aiassistant.LoadWorkbookMap(ctx, h.db.For(ctx), modelID, revID, nil); lErr == nil {
			specs = append(specs, mapSpecsForPlans(mapRows, planMetricNames(fresh), specs, before)...)
		}
	}
	compared := map[string]bool{}
	for _, s := range specs {
		compared[strings.ToLower(s.Metric)] = true
	}
	uncompared := h.uncomparedMetrics(ctx, revID, fresh, compared)
	if len(specs) > maxStageCheckRows {
		specs = specs[len(specs)-maxStageCheckRows:]
	}

	var differNow, broke, notCompared []string
	matched, total := 0, 0
	for _, group := range groupSpecs(specs) {
		rows := make([]compareRowArg, len(group))
		for i, s := range group {
			rows[i] = compareRowArg{Row: s.Row, Metric: s.Metric, Members: s.Members, FirstColumn: s.FirstColumn}
		}
		call := map[string]any{"file": group[0].File, "first_column": group[0].FirstColumn, "rows": rows}
		if group[0].Sheet != "" {
			call["sheet"] = group[0].Sheet
		}
		args, _ := json.Marshal(call)
		res, err := exec.Execute(ctx, "compare_with_sheet", args)
		if err != nil {
			notCompared = append(notCompared, fmt.Sprintf("%s %q: %v", group[0].File, group[0].Sheet, err))
			continue
		}
		for _, line := range joinDiffLines(res) {
			if strings.Contains(line, "has no time dimension to run along the row") || strings.Contains(line, aiassistant.NotPeriodRow) {
				continue // never comparable: not news
			}
			if strings.Contains(line, ": not compared — ") || strings.Contains(line, ": no number from column") {
				notCompared = append(notCompared, line)
				continue
			}
			g := compareLine.FindStringSubmatch(line)
			if g == nil {
				continue
			}
			total++
			row, _ := strconv.Atoi(g[2])
			ok, _ := strconv.Atoi(g[3])
			of, _ := strconv.Atoi(g[4])
			if ok == of {
				matched++
				continue
			}
			if prev, seen := before[strings.ToLower(g[1])][row]; seen && prev.matched == prev.compared {
				broke = append(broke, line)
				continue
			}
			differNow = append(differNow, line)
		}
	}
	if total == 0 && len(uncompared) == 0 && len(notCompared) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(head)
	if total > 0 {
		fmt.Fprintf(&b, " Compared again: %d row(s) compared with the source in this draft — %d match every period, %d differ.", total, matched, total-matched)
	}
	list := func(title string, lines []string, limit int) {
		if len(lines) == 0 {
			return
		}
		b.WriteString("\n" + title)
		for i, l := range lines {
			if i == limit {
				fmt.Fprintf(&b, "\n- and %d more", len(lines)-limit)
				break
			}
			b.WriteString("\n- " + l)
		}
	}
	brokeTitle := "Matched at their last comparison and differ now — the confirmed plan changed them:"
	if handover {
		brokeTitle = "Matched at their last comparison and differ now:"
	}
	list(brokeTitle, broke, 15)
	list("Differ:", differNow, 20)
	list("Could not be compared (renamed or removed metric, or the row moved):", notCompared, 10)
	if len(uncompared) > 0 {
		shown := uncompared
		if len(shown) > 40 {
			shown = append(append([]string{}, shown[:40]...), fmt.Sprintf("and %d more", len(uncompared)-40))
		}
		fmt.Fprintf(&b, "\nMade or changed by the confirmed plan and never compared with the source (%d): %s — compare each with its row "+
			"(compare_with_sheet, \"rows\": a whole sheet in one call).", len(uncompared), strings.Join(shown, ", "))
	}
	if len(broke)+len(differNow)+len(notCompared)+len(uncompared) > 0 {
		b.WriteString("\nFix what differs before going on — in the formula, or the input row it reads; never move an input until a row matches — " +
			"unless the developer's message asks for something else.")
	}
	if cur := plan.Current(); cur >= 0 && !handover {
		fmt.Fprintf(&b, "\nStage %d of your task plan (%s) is current: once its rows match, mark it done with update_task_plan and go on to the next stage.",
			cur+1, plan.Stages[cur].Title)
	}
	return b.String()
}

// joinDiffLines is a compare_with_sheet result one line per row: a call of
// one row lists its differing periods on lines of their own ("- 2026-01:
// sheet …"), which join their row's line as a call of several rows writes
// them, the first three and a count of the rest.
func joinDiffLines(res string) []string {
	var out []string
	extra := 0
	flush := func() {
		if extra > 0 && len(out) > 0 {
			out[len(out)-1] += fmt.Sprintf("; and %d more", extra)
		}
		extra = 0
	}
	shown := 0
	for _, line := range strings.Split(res, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") && len(out) > 0 {
			if strings.HasPrefix(line, "- and ") {
				continue
			}
			if shown < 3 {
				sep := "; "
				if strings.HasSuffix(out[len(out)-1], "Different:") {
					sep = " "
				}
				out[len(out)-1] += sep + strings.TrimPrefix(line, "- ")
				shown++
			} else {
				extra++
			}
			continue
		}
		flush()
		shown = 0
		if line != "" {
			out = append(out, line)
		}
	}
	flush()
	return out
}

// groupSpecs groups rows by file and sheet, in at most 80 rows a call.
func groupSpecs(specs []compareSpec) [][]compareSpec {
	var order []string
	by := map[string][]compareSpec{}
	for _, s := range specs {
		k := strings.ToLower(s.File + "\x00" + s.Sheet)
		if _, ok := by[k]; !ok {
			order = append(order, k)
		}
		by[k] = append(by[k], s)
	}
	var out [][]compareSpec
	for _, k := range order {
		g := by[k]
		for len(g) > 80 {
			out = append(out, g[:80])
			g = g[80:]
		}
		out = append(out, g)
	}
	return out
}

// uncomparedMetrics are the numeric metrics on a time grid that the steps
// of the fresh plans which ran created or changed and no comparison of the
// session names.
func (h *handler) uncomparedMetrics(ctx context.Context, revID string, fresh []aiassistant.Proposal, compared map[string]bool) []string {
	var refs []string
	for _, p := range fresh {
		created := make([]string, len(p.Steps))
		for i, s := range p.Steps {
			created[i] = s.CreatedID
		}
		for i, s := range p.Steps {
			if s.Status != "success" {
				continue
			}
			var a struct {
				Name     string `json:"name"`
				MetricID string `json:"metric_id"`
			}
			if json.Unmarshal(resolveParamRefs(s.Params, created[:i]), &a) != nil {
				continue
			}
			switch s.Tool {
			case "create_metric":
				refs = append(refs, strings.ToLower(a.Name), s.CreatedID)
			case "update_metric", "write_input_values":
				refs = append(refs, strings.ToLower(a.MetricID))
			}
		}
	}
	if len(refs) == 0 {
		return nil
	}
	rows, err := h.db.For(ctx).Query(ctx, `
		SELECT m.name FROM model.metric_def m
		WHERE m.revision_id = $1::uuid AND (m.id::text = ANY($2) OR lower(m.name) = ANY($2))
		  AND COALESCE(m.format, 'number') IN ('number', 'currency', 'percentage')
		  AND EXISTS (SELECT 1 FROM model.grid_metric gm JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
		              JOIN model.dimension_def d ON d.id = gd.dimension_id
		              WHERE gm.metric_id = m.id AND d.dimension_type = 'time')
		ORDER BY m.name`, revID, refs)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil && !compared[strings.ToLower(name)] {
			out = append(out, name)
		}
	}
	return out
}

// countRan is how many of a confirmed plan's steps ran.
func countRan(steps []aiassistant.ProposalStep) int {
	n := 0
	for _, s := range steps {
		if s.Status == "success" {
			n++
		}
	}
	return n
}

// sourceFilterProblems refuses a saved loader whose reshape.skip_rows drops
// data rows by a value — an account, a code, a member — unless the
// developer's request names that value. Refused account 6450 by the GL
// loader, gpt-5.2 saved skip_rows "GL code = 6450" into the monthly loader:
// every run reported all rows imported, and the model's unmapped-account
// check could never fire (2026-11-11). skip_rows is for rows that are not
// data: totals, subtotals, notes, a repeated header, blank lines.
func sourceFilterProblems(steps []aiassistant.ProposalStep, request string) (problems []string, failed []int) {
	low := strings.ToLower(request)
	for i, step := range steps {
		if step.Tool != "create_file_integration" && step.Tool != "update_integration" {
			continue
		}
		var p struct {
			Name    string `json:"name"`
			Reshape *struct {
				SkipRows []importpkg.RowFilter `json:"skip_rows"`
			} `json:"reshape"`
		}
		if json.Unmarshal(step.Params, &p) != nil || p.Reshape == nil {
			continue
		}
		for _, f := range p.Reshape.SkipRows {
			v := strings.TrimSpace(f.Equals)
			if v == "" {
				v = strings.TrimSpace(f.Contains)
			}
			if f.Blank || v == "" || notDataValue(v, f.Column) || strings.Contains(low, strings.ToLower(v)) {
				continue
			}
			problems = append(problems, fmt.Sprintf("step %d (%s %s): reshape.skip_rows drops every row where %s, in every file this loader runs — that hides source data. "+
				"A value the model lacks (an account, a member, a code) is added (add_dimension_member, or a mapping row) or asked about, never filtered out of a saved loader: "+
				"the model's checks must see it. skip_rows is for rows that are not data — totals, subtotals, notes, a repeated header, blank lines. Leave this filter out",
				i+1, step.Tool, p.Name, f.Describe()))
			failed = append(failed, i)
			break
		}
	}
	return problems, failed
}

// notDataValue reports a skip_rows value naming rows that are not data: a
// total, a note, a repeated header (the column's own name), a dash.
func notDataValue(v, column string) bool {
	l := strings.ToLower(strings.TrimSpace(v))
	if l == strings.ToLower(strings.TrimSpace(column)) || strings.Trim(l, "-–—_ .*=") == "" {
		return true
	}
	for _, w := range []string{"total", "subtotal", "sum", "grand", "header", "heading", "title", "note", "comment", "memo", "blank", "empty", "n/a", "page"} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

// additionAsk: a request asking to add to what a formula does — "add the
// approved requests to the formula", "include", "plus", "also".
var additionAsk = regexp.MustCompile(`(?i)\b(add|adds|adding|include|including|plus|extend|also|as well|on top of|in addition)\b`)

// replaceAsk: a request or step saying a formula is to be replaced whole.
var replaceAsk = regexp.MustCompile(`(?i)\b(replace|replaces|replacing|rewrite|rewrites|new formula|instead of|from scratch|redo)\b`)

// formulaIdent finds the names a formula reads (quoted text left out).
var (
	formulaIdent  = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	formulaQuoted = regexp.MustCompile(`"[^"]*"`)
)

// formulaReplacedProblems refuses an update_metric whose new formula keeps
// none of the metrics its formula reads now, unless the request names the
// metric without asking to add to it, or asks for a replacement. Asked to
// "add the approved requests" to hc_new_fte_joined, gpt-5.4-mini replaced
// the formula with the requests term alone: every FTE read 0 and 49 rows
// went wrong, in a plan that also built the requests table (2026-11-11).
// Read before the plan runs, on q.
func formulaReplacedProblems(ctx context.Context, q *pgxpool.Pool, modelID, revID string, steps []aiassistant.ProposalStep, request string) (problems []string, failed []int) {
	low := strings.ToLower(request)
	type def struct{ name, label, formula string }
	var names map[string]bool
	for i, step := range steps {
		if step.Tool != "update_metric" {
			continue
		}
		var p struct {
			MetricID string  `json:"metric_id"`
			Formula  *string `json:"formula"`
		}
		if json.Unmarshal(step.Params, &p) != nil || p.Formula == nil || strings.TrimSpace(*p.Formula) == "" {
			continue
		}
		var d def
		if q.QueryRow(ctx, `
			SELECT name, COALESCE(label,''), COALESCE(formula,'') FROM model.metric_def
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND (id::text=$3 OR lower(name)=lower($3))`,
			modelID, revID, p.MetricID).Scan(&d.name, &d.label, &d.formula) != nil || strings.TrimSpace(d.formula) == "" {
			continue
		}
		if names == nil {
			names = map[string]bool{}
			if rows, err := q.Query(ctx, `SELECT lower(name) FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`, modelID, revID); err == nil {
				for rows.Next() {
					var n string
					if rows.Scan(&n) == nil {
						names[n] = true
					}
				}
				rows.Close()
			}
		}
		reads := func(f string) map[string]bool {
			out := map[string]bool{}
			for _, id := range formulaIdent.FindAllString(formulaQuoted.ReplaceAllString(f, `""`), -1) {
				if l := strings.ToLower(id); names[l] && l != strings.ToLower(d.name) {
					out[l] = true
				}
			}
			return out
		}
		before, after := reads(d.formula), reads(*p.Formula)
		if len(before) == 0 {
			continue
		}
		kept := false
		for n := range before {
			if after[n] {
				kept = true
				break
			}
		}
		if kept || replaceAsk.MatchString(request) || replaceAsk.MatchString(step.Description) {
			continue
		}
		if namesMetric(low, d.name, d.label) && !additionAsk.MatchString(request) {
			continue
		}
		lost := make([]string, 0, len(before))
		for n := range before {
			lost = append(lost, n)
		}
		sort.Strings(lost)
		problems = append(problems, fmt.Sprintf("step %d (update_metric %s): the new formula drops everything the formula reads now (%s): %q would replace %q. "+
			"To add a term, keep the formula and add to it (old formula + new term); replace a whole formula only when the request asks for that",
			i+1, d.name, strings.Join(lost, ", "), *p.Formula, d.formula))
		failed = append(failed, i)
	}
	return problems, failed
}

// namedFile finds file names a request names: "hightech-actuals-2027-01-06.xlsx".
var namedFile = regexp.MustCompile(`(?i)[\w][\w.()&+-]*\.(xlsx|xlsm|xls|csv)\b`)

// missingFileProblems refuses a plan that imports a file while the request
// names a file the chat does not have: asked to load the actuals file, which
// was not attached in that session, the AI loaded the main workbook's empty
// Import BS template instead and said nothing (2026-11-11).
func missingFileProblems(steps []aiassistant.ProposalStep, request string, attached []string) (problems []string, failed []int) {
	have := map[string]bool{}
	for _, a := range attached {
		have[strings.ToLower(strings.TrimSpace(a))] = true
	}
	var missing []string
	for _, f := range namedFile.FindAllString(request, -1) {
		f = strings.TrimSpace(f)
		low := strings.ToLower(f)
		found := have[low]
		for a := range have {
			if strings.HasSuffix(low, a) || strings.HasSuffix(a, low) {
				found = true
			}
		}
		if !found {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	for i, step := range steps {
		if step.Tool != "import_file_data" {
			continue
		}
		var p struct {
			File string `json:"file"`
		}
		_ = json.Unmarshal(step.Params, &p)
		list := "none"
		if len(attached) > 0 {
			list = strings.Join(attached, ", ")
		}
		problems = append(problems, fmt.Sprintf("step %d (import_file_data %s): the request names %s, which is not attached to this chat (attached: %s). "+
			"Never load another file in its place: say that the file is missing and ask the developer to attach it",
			i+1, p.File, strings.Join(missing, ", "), list))
		failed = append(failed, i)
	}
	return problems, failed
}

// aiTypedFiles are the session's files the assistant wrote itself
// (create_csv_file, sample values) — a converted-file record with no sheet,
// reshape or column map — by lower-cased file name; none while the session
// has no spreadsheet of the developer's attached, where there is nothing
// to retype.
func (h *handler) aiTypedFiles(ctx context.Context, sessionID string) map[string]bool {
	if len(h.attachedSheetNames(ctx, sessionID)) == 0 {
		return nil
	}
	rows, err := h.db.For(ctx).Query(ctx, `
		SELECT d.filename FROM ai_assistant.document d
		JOIN ai_assistant.conversion c ON c.document_id = d.id
		WHERE d.session_id = $1::uuid AND COALESCE(c.sheet, '') = '' AND c.reshape IS NULL AND c.column_map IS NULL`, sessionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			out[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	return out
}

// typedFileProblems refuses importing a file the assistant typed into a
// grid or table while the developer's workbook is attached, unless the
// request names the file or asks for sample data: gpt-5-mini, its sheet
// conversion failing, retyped the Assumptions sheet with invented numbers
// (ARPU 10/8/12 for the workbook's 59/49/69) into create_csv_file and
// imported it as the sheet — twice, the second time after the developer
// said never to retype, and with the real conversion already made
// (2026-11-12).
func typedFileProblems(steps []aiassistant.ProposalStep, request string, typed map[string]bool) (problems []string, failed []int) {
	if len(typed) == 0 {
		return nil, nil
	}
	low := strings.ToLower(request)
	if strings.Contains(low, "sample") || strings.Contains(low, "test data") || strings.Contains(low, "dummy") {
		return nil, nil
	}
	for i, step := range steps {
		if step.Tool != "import_file_data" {
			continue
		}
		var p struct {
			File string `json:"file"`
		}
		if json.Unmarshal(step.Params, &p) != nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(p.File))
		if name == "" || !typed[name] || strings.Contains(low, name) {
			continue
		}
		problems = append(problems, fmt.Sprintf("step %d (import_file_data %s): you wrote this file yourself (create_csv_file) — its values are typed, not read from the developer's workbook. "+
			"Import the source: convert the attached sheet (prepare_converted_file, or import_file_data on the workbook with reshape and column_map) and import that. "+
			"Never retype a sheet; if the conversion fails, fix its parameters or ask the developer", i+1, p.File))
		failed = append(failed, i)
	}
	return problems, failed
}
