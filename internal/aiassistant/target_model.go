package aiassistant

// The target model (owner, 2026-11-12): building from a workbook, the AI
// Developer first analyses the whole workbook and records the TO-BE model —
// dimensions, each sheet's role, grid, dimensions and metrics, the pages,
// roles and workflows — and only then plans and builds, stage by stage,
// always in its context. It lives with the task plan (carried to every new
// session) and is shown in every turn.
//
// One agreed design before construction (owner, 2026-11-12, second round):
// the conventions every row follows (units, signs, percentages, opening
// balances, scenarios, monthly against annual), the substantive choices the
// developer resolves once (open decisions hold their sheets back), the
// business requirements the platform adds (scenarios, requests, access,
// approval, an approved revision, role pages) and the checks that make
// "done" mean verified behaviour.

import (
	"fmt"
	"strings"
)

// TargetModel is the TO-BE design of a model built from a workbook.
type TargetModel struct {
	Summary     string            `json:"summary"`
	Dimensions  []TargetDimension `json:"dimensions"`
	Conventions TargetConventions `json:"conventions"`
	Sheets      []TargetSheet     `json:"sheets"`
	Pages       []string          `json:"pages,omitempty"`
	Roles       []string          `json:"roles,omitempty"`
	Workflows   []string          `json:"workflows,omitempty"`
	// Decisions are the design choices the workbook leaves open; one
	// without a choice holds back the sheets it names.
	Decisions []TargetDecision `json:"decisions,omitempty"`
	// Requirements are the platform behaviour the business asked for,
	// built once the calculations they read reconcile.
	Requirements []TargetRequirement `json:"requirements,omitempty"`
	// Checks are the representative cases beyond the saved workbook (other
	// scenarios, a switch off, a delay, actual zero against missing, a
	// rejected approval, a restricted user) that verify the model behaves.
	Checks []TargetCheck `json:"checks,omitempty"`
}

// TargetDimension is one dimension of the target model.
type TargetDimension struct {
	Name    string `json:"name"`
	Members string `json:"members"` // what its members are, or where they come from
}

// TargetConventions are the rules every mapped row follows, agreed once.
type TargetConventions struct {
	Units           string `json:"units"`            // currency, scale, counts
	Signs           string `json:"signs"`            // costs negative or positive, cash in/out
	Percentages     string `json:"percentages"`      // percent units (5 = 5%) and how the workbook stores them
	OpeningBalances string `json:"opening_balances"` // where a balance starts, how a closing carries
	Scenarios       string `json:"scenarios"`        // how scenarios are held and which one the workbook shows
	Aggregation     string `json:"aggregation"`      // monthly against annual: sums, closings, averages
}

// missing names the conventions left empty.
func (c TargetConventions) missing() []string {
	var out []string
	for _, f := range []struct{ name, v string }{
		{"units", c.Units}, {"signs", c.Signs}, {"percentages", c.Percentages},
		{"opening_balances", c.OpeningBalances}, {"scenarios", c.Scenarios}, {"aggregation", c.Aggregation},
	} {
		if strings.TrimSpace(f.v) == "" {
			out = append(out, f.name)
		}
	}
	return out
}

// TargetSheet is what one workbook sheet becomes, in build order.
type TargetSheet struct {
	Sheet      string   `json:"sheet"`
	Role       string   `json:"role"` // input | calculation | report | actuals | guide | skip
	Grid       string   `json:"grid,omitempty"`
	Dimensions []string `json:"dimensions,omitempty"`
	Rows       string   `json:"rows,omitempty"` // the sheet rows it covers, e.g. "6-77"
	Metrics    []string `json:"metrics,omitempty"`
	Notes      string   `json:"notes,omitempty"`
	// Owner is the business role that owns the sheet's inputs (or reads its
	// results); Verification is how the sheet is shown to be right.
	Owner        string `json:"owner,omitempty"`
	Verification string `json:"verification,omitempty"`
	// RowNotes give rows their business meaning, and mark a row the model
	// changes on purpose (status "changed", with the reason).
	RowNotes []TargetRowNote `json:"row_notes,omitempty"`
}

// TargetRowNote is one workbook row's meaning, or a deliberate change.
type TargetRowNote struct {
	Row     int    `json:"row"`
	Meaning string `json:"meaning,omitempty"`
	Status  string `json:"status,omitempty"` // "" | changed
	Reason  string `json:"reason,omitempty"`
}

// TargetDecision is a substantive design choice, resolved once.
type TargetDecision struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
	Choice   string   `json:"choice,omitempty"` // "" while open
	Sheets   []string `json:"sheets,omitempty"` // the sheets it holds back while open
}

// Open reports whether the decision waits for the developer.
func (d TargetDecision) Open() bool { return strings.TrimSpace(d.Choice) == "" }

// TargetRequirement is one business requirement the platform implements.
type TargetRequirement struct {
	Area      string `json:"area"`      // scenarios, hiring, access, budget approval, approved plan, reporting…
	Behaviour string `json:"behaviour"` // what people can do
	Status    string `json:"status,omitempty"`
}

// TargetCheck is one representative check of the model's behaviour.
type TargetCheck struct {
	Name     string `json:"name"`
	How      string `json:"how"`
	Status   string `json:"status,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

var (
	targetRoles       = map[string]bool{"input": true, "calculation": true, "report": true, "actuals": true, "guide": true, "skip": true}
	requirementStates = map[string]bool{"": true, "open": true, "built": true, "verified": true}
	checkStates       = map[string]bool{"": true, "open": true, "passed": true, "failed": true, "not_possible": true}
)

// CheckTarget checks a target the AI wrote; sheets (may be nil) are the
// attached workbooks' sheets: every sheet named must be one of them, and the
// ones the target leaves out are returned so the AI can place them. A target
// of an attached workbook states its conventions.
func CheckTarget(t *TargetModel, sheets []string) (missing []string, err error) {
	if t == nil || strings.TrimSpace(t.Summary) == "" {
		return nil, fmt.Errorf("set_target_model needs a summary: the model to build, in two or three sentences")
	}
	if len(t.Sheets) == 0 {
		return nil, fmt.Errorf("set_target_model needs sheets: what each workbook sheet becomes, in build order")
	}
	if len(sheets) > 0 {
		if m := t.Conventions.missing(); len(m) > 0 {
			return nil, fmt.Errorf("set_target_model needs conventions agreed once for every row: %s — say how the workbook does it and what the model does "+
				"(\"n/a\" where nothing applies)", strings.Join(m, ", "))
		}
	}
	known := map[string]string{}
	for _, s := range sheets {
		known[strings.ToLower(strings.TrimSpace(s))] = s
	}
	seen := map[string]bool{}
	for i := range t.Sheets {
		s := &t.Sheets[i]
		s.Role = strings.ToLower(strings.TrimSpace(s.Role))
		if !targetRoles[s.Role] {
			return nil, fmt.Errorf("set_target_model: sheet %q: role is input, calculation, report, actuals, guide or skip", s.Sheet)
		}
		k := strings.ToLower(strings.TrimSpace(s.Sheet))
		if len(known) > 0 {
			name, ok := known[k]
			if !ok {
				return nil, fmt.Errorf("set_target_model: no attached sheet is named %q — the workbook's sheets: %s", s.Sheet, strings.Join(sheets, ", "))
			}
			s.Sheet = name
		}
		if seen[k] {
			return nil, fmt.Errorf("set_target_model: sheet %q is listed twice", s.Sheet)
		}
		seen[k] = true
		if (s.Role == "input" || s.Role == "calculation" || s.Role == "report" || s.Role == "actuals") && strings.TrimSpace(s.Grid) == "" {
			return nil, fmt.Errorf("set_target_model: sheet %q (%s) needs the grid it becomes", s.Sheet, s.Role)
		}
		// plan_sheet maps a sheet's rows to the target's metric names: a
		// target of a workbook without them made it fail for every sheet
		// until the target was sent again (High-Tech rebuild 4, 2026-11-12).
		if len(known) > 0 && (s.Role == "calculation" || s.Role == "report") && len(s.Metrics) == 0 {
			return nil, fmt.Errorf("set_target_model: sheet %q (%s) needs its metric names (snake_case, one per row or row group): plan_sheet translates its formulas onto them", s.Sheet, s.Role)
		}
		for _, n := range s.RowNotes {
			if n.Status != "" && n.Status != "changed" {
				return nil, fmt.Errorf("set_target_model: sheet %q row %d: a row note's status is \"changed\" or empty", s.Sheet, n.Row)
			}
			if n.Status == "changed" && strings.TrimSpace(n.Reason) == "" {
				return nil, fmt.Errorf("set_target_model: sheet %q row %d is changed on purpose: say why (reason)", s.Sheet, n.Row)
			}
		}
	}
	if err := t.checkLists(seen); err != nil {
		return nil, err
	}
	for _, s := range sheets {
		if !seen[strings.ToLower(strings.TrimSpace(s))] {
			missing = append(missing, s)
		}
	}
	return missing, nil
}

// checkLists checks the decisions, requirements and checks; placed are the
// target's sheets (lower-cased).
func (t *TargetModel) checkLists(placed map[string]bool) error {
	for _, d := range t.Decisions {
		if strings.TrimSpace(d.Question) == "" {
			return fmt.Errorf("set_target_model: a decision needs its question")
		}
		for _, s := range d.Sheets {
			if len(placed) > 0 && !placed[strings.ToLower(strings.TrimSpace(s))] {
				return fmt.Errorf("set_target_model: decision %q names sheet %q, which the target does not place", d.Question, s)
			}
		}
	}
	for _, r := range t.Requirements {
		if strings.TrimSpace(r.Area) == "" || strings.TrimSpace(r.Behaviour) == "" {
			return fmt.Errorf("set_target_model: a requirement needs its area and behaviour")
		}
		if !requirementStates[r.Status] {
			return fmt.Errorf("set_target_model: requirement %q: status is open, built or verified", r.Area)
		}
	}
	for _, c := range t.Checks {
		if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.How) == "" {
			return fmt.Errorf("set_target_model: a check needs its name and how it is made")
		}
		if !checkStates[c.Status] {
			return fmt.Errorf("set_target_model: check %q: status is open, passed, failed or not_possible", c.Name)
		}
	}
	return nil
}

// TargetUpdate is update_target_model's change: decisions resolved,
// requirements and checks moved on (or added), rows given a meaning or
// marked changed on purpose.
type TargetUpdate struct {
	Decisions    []TargetDecision    `json:"decisions,omitempty"`
	Requirements []TargetRequirement `json:"requirements,omitempty"`
	Checks       []TargetCheck       `json:"checks,omitempty"`
	Rows         []struct {
		Sheet string `json:"sheet"`
		TargetRowNote
	} `json:"rows,omitempty"`
}

// Apply merges u into t: a decision, requirement or check is found by its
// question, area or name (case-insensitive) and updated, or added; a row
// note by sheet and row. It returns the decisions it resolved.
func (t *TargetModel) Apply(u TargetUpdate) (resolved []TargetDecision, err error) {
	if t == nil {
		return nil, fmt.Errorf("update_target_model: no target model yet — set_target_model first")
	}
	for _, d := range u.Decisions {
		i := findBy(len(t.Decisions), func(i int) string { return t.Decisions[i].Question }, d.Question)
		wasOpen := true
		if i < 0 {
			t.Decisions = append(t.Decisions, d)
			i = len(t.Decisions) - 1
		} else {
			wasOpen = t.Decisions[i].Open()
			if strings.TrimSpace(d.Choice) != "" {
				t.Decisions[i].Choice = d.Choice
			}
			if len(d.Options) > 0 {
				t.Decisions[i].Options = d.Options
			}
			if len(d.Sheets) > 0 {
				t.Decisions[i].Sheets = d.Sheets
			}
		}
		if wasOpen && !t.Decisions[i].Open() {
			resolved = append(resolved, t.Decisions[i])
		}
	}
	for _, r := range u.Requirements {
		if i := findBy(len(t.Requirements), func(i int) string { return t.Requirements[i].Area }, r.Area); i >= 0 {
			if r.Behaviour != "" {
				t.Requirements[i].Behaviour = r.Behaviour
			}
			if r.Status != "" {
				t.Requirements[i].Status = r.Status
			}
		} else {
			t.Requirements = append(t.Requirements, r)
		}
	}
	for _, c := range u.Checks {
		if i := findBy(len(t.Checks), func(i int) string { return t.Checks[i].Name }, c.Name); i >= 0 {
			if c.How != "" {
				t.Checks[i].How = c.How
			}
			if c.Status != "" {
				t.Checks[i].Status = c.Status
			}
			if c.Evidence != "" {
				t.Checks[i].Evidence = c.Evidence
			}
		} else {
			t.Checks = append(t.Checks, c)
		}
	}
	for _, r := range u.Rows {
		s := t.sheet(r.Sheet)
		if s == nil {
			return nil, fmt.Errorf("update_target_model: the target does not place sheet %q", r.Sheet)
		}
		if j := findBy(len(s.RowNotes), func(j int) string { return fmt.Sprint(s.RowNotes[j].Row) }, fmt.Sprint(r.Row)); j >= 0 {
			if r.Meaning != "" {
				s.RowNotes[j].Meaning = r.Meaning
			}
			if r.Status != "" || r.Reason != "" {
				s.RowNotes[j].Status, s.RowNotes[j].Reason = r.Status, r.Reason
			}
		} else {
			s.RowNotes = append(s.RowNotes, r.TargetRowNote)
		}
	}
	placed := map[string]bool{}
	for _, s := range t.Sheets {
		placed[strings.ToLower(s.Sheet)] = true
		for _, n := range s.RowNotes {
			if n.Status == "changed" && strings.TrimSpace(n.Reason) == "" {
				return nil, fmt.Errorf("update_target_model: sheet %q row %d is changed on purpose: say why (reason)", s.Sheet, n.Row)
			}
		}
	}
	if err := t.checkLists(placed); err != nil {
		return nil, fmt.Errorf("update_target_model: %s", strings.TrimPrefix(err.Error(), "set_target_model: "))
	}
	return resolved, nil
}

func findBy(n int, key func(int) string, want string) int {
	want = strings.ToLower(strings.TrimSpace(want))
	for i := 0; i < n; i++ {
		if strings.ToLower(strings.TrimSpace(key(i))) == want {
			return i
		}
	}
	return -1
}

func (t *TargetModel) sheet(name string) *TargetSheet {
	for i := range t.Sheets {
		if strings.EqualFold(strings.TrimSpace(t.Sheets[i].Sheet), strings.TrimSpace(name)) {
			return &t.Sheets[i]
		}
	}
	return nil
}

// OpenDecisionsFor are the open decisions that hold sheet back.
func (t *TargetModel) OpenDecisionsFor(sheet string) []TargetDecision {
	if t == nil {
		return nil
	}
	var out []TargetDecision
	for _, d := range t.Decisions {
		if !d.Open() {
			continue
		}
		for _, s := range d.Sheets {
			if strings.EqualFold(strings.TrimSpace(s), strings.TrimSpace(sheet)) {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// HeldBack is the error a sheet's open decisions give ("" when none): the
// sheet is not loaded or translated until the developer has chosen.
func (t *TargetModel) HeldBack(sheet string) string {
	ds := t.OpenDecisionsFor(sheet)
	if len(ds) == 0 {
		return ""
	}
	var qs []string
	for _, d := range ds {
		qs = append(qs, fmt.Sprintf("%q", d.Question))
	}
	return fmt.Sprintf("sheet %q waits for the developer's decision on %s: ask it (with the options), then record the choice with update_target_model",
		sheet, strings.Join(qs, " and "))
}

// OpenChecks are the checks not yet passed, failed or found impossible.
func (t *TargetModel) OpenChecks() []string {
	if t == nil {
		return nil
	}
	var out []string
	for _, c := range t.Checks {
		if c.Status == "" || c.Status == "open" {
			out = append(out, c.Name)
		}
	}
	return out
}

// BuildSheets are the target's sheets that are built (not guide or skip),
// in build order.
func (t *TargetModel) BuildSheets() []string {
	if t == nil {
		return nil
	}
	var out []string
	for _, s := range t.Sheets {
		if s.Role != "guide" && s.Role != "skip" {
			out = append(out, s.Sheet)
		}
	}
	return out
}

// MetricNames are every metric the target names, lower-cased.
func (t *TargetModel) MetricNames() map[string]bool {
	out := map[string]bool{}
	if t == nil {
		return out
	}
	for _, s := range t.Sheets {
		for _, m := range s.Metrics {
			if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
				out[m] = true
			}
		}
	}
	return out
}

// Render is the whole target (set_target_model's answer).
func (t *TargetModel) Render() string { return t.RenderFor() }

// RenderFor is the target as a turn reads it: with current sheets (the
// stage's module), those whole and the others one line each (2026-11-12:
// the whole target, 18k characters, went with every call).
func (t *TargetModel) RenderFor(current ...string) string {
	if t == nil {
		return ""
	}
	cur := map[string]bool{}
	for _, c := range current {
		if c = strings.ToLower(strings.TrimSpace(c)); c != "" {
			cur[c] = true
		}
	}
	var b strings.Builder
	b.WriteString("TARGET MODEL (the TO-BE design you build towards — every plan stays within it; change it with set_target_model when the workbook shows it must, " +
		"or update_target_model for decisions, requirements, checks and row notes):\n")
	fmt.Fprintf(&b, "%s\n", strings.TrimSpace(t.Summary))
	if len(t.Dimensions) > 0 {
		var ds []string
		for _, d := range t.Dimensions {
			ds = append(ds, d.Name+" ("+d.Members+")")
		}
		fmt.Fprintf(&b, "Dimensions: %s\n", strings.Join(ds, "; "))
	}
	c := t.Conventions
	if len(c.missing()) < 6 {
		fmt.Fprintf(&b, "Conventions: units %s; signs %s; percentages %s; opening balances %s; scenarios %s; monthly vs annual %s\n",
			orDash(c.Units), orDash(c.Signs), orDash(c.Percentages), orDash(c.OpeningBalances), orDash(c.Scenarios), orDash(c.Aggregation))
	}
	b.WriteString("Sheets in build order:\n")
	for _, s := range t.Sheets {
		fmt.Fprintf(&b, "- %s [%s]", s.Sheet, s.Role)
		if s.Grid != "" {
			fmt.Fprintf(&b, " → grid %q", s.Grid)
			if len(s.Dimensions) > 0 {
				fmt.Fprintf(&b, " by %s", strings.Join(s.Dimensions, " × "))
			}
		}
		if s.Rows != "" {
			fmt.Fprintf(&b, ", rows %s", s.Rows)
		}
		if len(cur) > 0 && !cur[strings.ToLower(s.Sheet)] {
			if n := len(s.Metrics); n > 0 {
				fmt.Fprintf(&b, ", %d metrics", n)
			}
			b.WriteByte('\n')
			continue
		}
		if n := len(s.Metrics); n > 0 {
			shown := s.Metrics
			if n > 12 {
				shown = append(append([]string{}, s.Metrics[:12]...), fmt.Sprintf("… %d more", n-12))
			}
			fmt.Fprintf(&b, ", %d metrics: %s", n, strings.Join(shown, ", "))
		}
		if s.Owner != "" {
			fmt.Fprintf(&b, "; owner %s", s.Owner)
		}
		if s.Verification != "" {
			fmt.Fprintf(&b, "; verified by %s", s.Verification)
		}
		if s.Notes != "" {
			fmt.Fprintf(&b, " — %s", s.Notes)
		}
		b.WriteByte('\n')
		for _, n := range s.RowNotes {
			fmt.Fprintf(&b, "    row %d", n.Row)
			if n.Meaning != "" {
				fmt.Fprintf(&b, ": %s", n.Meaning)
			}
			if n.Status == "changed" {
				fmt.Fprintf(&b, " [changed on purpose: %s]", n.Reason)
			}
			b.WriteByte('\n')
		}
	}
	for _, l := range []struct {
		name  string
		items []string
	}{{"Dashboards", t.Pages}, {"Roles", t.Roles}, {"Workflows and triggers", t.Workflows}} {
		if len(l.items) > 0 {
			fmt.Fprintf(&b, "%s: %s\n", l.name, strings.Join(l.items, "; "))
		}
	}
	var open, chosen []string
	for _, d := range t.Decisions {
		if d.Open() {
			line := d.Question
			if len(d.Options) > 0 {
				line += " (options: " + strings.Join(d.Options, " | ") + ")"
			}
			if len(d.Sheets) > 0 {
				line += " — holds back " + strings.Join(d.Sheets, ", ")
			}
			open = append(open, line)
		} else {
			chosen = append(chosen, d.Question+" → "+d.Choice)
		}
	}
	if len(open) > 0 {
		b.WriteString("OPEN DECISIONS (ask the developer once, with the options; record the choice with update_target_model — the sheets they hold back wait):\n- " +
			strings.Join(open, "\n- ") + "\n")
	}
	if len(chosen) > 0 {
		fmt.Fprintf(&b, "Decided: %s\n", strings.Join(chosen, "; "))
	}
	if len(t.Requirements) > 0 {
		var rs []string
		for _, r := range t.Requirements {
			rs = append(rs, fmt.Sprintf("%s: %s [%s]", r.Area, r.Behaviour, orState(r.Status)))
		}
		fmt.Fprintf(&b, "Business requirements (built after the calculations they read reconcile): %s\n", strings.Join(rs, "; "))
	}
	if len(t.Checks) > 0 {
		var cs []string
		for _, c := range t.Checks {
			cs = append(cs, fmt.Sprintf("%s [%s]", c.Name, orState(c.Status)))
		}
		fmt.Fprintf(&b, "Verification checks (record each with update_target_model: passed, failed or not_possible, with evidence): %s\n", strings.Join(cs, "; "))
	}
	return b.String()
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return strings.TrimSpace(s)
}

func orState(s string) string {
	if s == "" {
		return "open"
	}
	return s
}
