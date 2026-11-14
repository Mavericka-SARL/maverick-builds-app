package aiassistant

// Think multidimensionally (owner, 2026-11-13): an OLAP model holds a version
// (actual, budget, forecast), a period or a member of any dimension as a
// member of that dimension — never in a metric's name. actuals_cost_2027 is
// one metric, cost, on a grid with a Version dimension and the time
// dimension. The AI is sent back when a plan or a target model names metrics
// that way; a name the developer wrote in their own message is theirs.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// MultidimensionalRule is the modelling rule every turn's prompt carries.
const MultidimensionalRule = `

## Think multidimensionally (OLAP)
A measure is ONE metric; what varies is a dimension. Never put a version, a period or a member of a dimension in a
metric's name: not actuals_cost_2027, budget_revenue, revenue_q1, salary_jan, revenue_emea and revenue_apac — but
one metric cost (revenue, salary) on a grid with the dimensions it varies by:
- Version: Actual, Budget, Forecast (and scenarios such as Base, Upside) are members of a Version dimension. A model
  with actuals and a plan has one; create it when it has none. Variances read the versions of one metric.
- Time: years, quarters and months are the time dimension's members (a year is the total of its months).
- Every other breakdown (region, product, department, channel, cost centre) is a dimension with members.
The server sends back a plan or target model whose INPUT metric names hold a version or a period, and asks once
about inputs that differ only by members of one dimension. Only a name the developer wrote in their message is kept.
`

var (
	versionTokens = map[string]string{
		"actual": "Actual", "actuals": "Actual", "act": "Actual",
		"budget": "Budget", "budgeted": "Budget", "bud": "Budget", "bgt": "Budget", "budg": "Budget",
		"forecast": "Forecast", "forecasted": "Forecast", "forecasts": "Forecast", "fcst": "Forecast", "fct": "Forecast",
		"outlook": "Forecast", "reforecast": "Forecast",
		"py": "Prior year", "ly": "Prior year",
	}
	versionPairs = map[string]string{"prior_year": "Prior year", "last_year": "Prior year", "previous_year": "Prior year"}
	// comparisonWords: a metric that compares versions names them
	// (budget_variance, actual_vs_forecast) — that is the measure itself.
	comparisonWords = map[string]bool{
		"variance": true, "var": true, "vs": true, "versus": true, "delta": true, "diff": true, "difference": true, "gap": true,
		"attainment": true, "achievement": true, "accuracy": true, "bias": true, "deviation": true, "over": true, "under": true,
	}
	monthTokens = map[string]bool{
		"january": true, "february": true, "march": true, "april": true, "may": true, "june": true, "july": true, "august": true,
		"september": true, "october": true, "november": true, "december": true,
		"jan": true, "feb": true, "apr": true, "jun": true, "jul": true, "aug": true, "sep": true, "sept": true, "oct": true, "nov": true,
	}
	periodRE     = regexp.MustCompile(`^((19|20)\d{2}|fy\d{2}|fy(19|20)\d{2}|q[1-4]|h[12]|(19|20)\d{2}q[1-4]|q[1-4](19|20)\d{2}|y\d{1,2}|year\d{1,2}|(19|20)\d{2}m\d{1,2})$`)
	versionDimRE = regexp.MustCompile(`(?i)(version|scenario|plan_?type|data_?type|^views?$|^cases?$)`)
	snakeRE      = regexp.MustCompile(`[^a-z0-9]+`)
)

// snake is s as a metric name would hold it.
func snake(s string) string {
	return strings.Trim(snakeRE.ReplaceAllString(strings.ToLower(s), "_"), "_")
}

// FlatName is what a metric's name holds that belongs to a dimension.
type FlatName struct {
	Name     string
	Versions []string // tokens naming a version, e.g. "actuals"
	Periods  []string // tokens naming a period, e.g. "2027", "q1", "jan"
	Measure  string   // the name without them: the metric it should be
}

// controlWords: a flag or control value about versions or periods
// (is_actual, actual_through_month, last_closed_month) is a measure of its
// own, not a version's copy of one.
var controlWords = map[string]bool{
	"is": true, "has": true, "flag": true, "through": true, "thru": true, "until": true, "cutover": true, "cutoff": true, "closed": true,
	"close": true, "lock": true, "locked": true, "status": true, "check": true, "mode": true, "last": true, "first": true, "start": true,
	"end": true, "switch": true, "toggle": true, "enabled": true, "include": true, "use": true,
}

// FlatNameParts reads name for versions and periods: a period anywhere in
// it (2027, fy27, q1, jan), a version at its start or end (actuals_cost,
// revenue_budget). versionMembers are the members (snake) of the model's
// version-like dimensions; they count as versions too.
func FlatNameParts(name string, versionMembers map[string]bool) FlatName {
	f := FlatName{Name: name}
	toks := strings.Split(snake(name), "_")
	for _, t := range toks {
		if comparisonWords[t] || controlWords[t] {
			return f // a comparison or a control value names versions and periods on purpose
		}
	}
	var rest []string
	for _, t := range toks {
		if periodRE.MatchString(t) || monthTokens[t] {
			f.Periods = append(f.Periods, t)
			continue
		}
		rest = append(rest, t)
	}
	// A scenario member is an ordinary word as often (base_salary): it
	// counts at the end of the name only (revenue_upside).
	isVersion := func(t string) bool { return versionTokens[t] != "" }
	if n := len(rest); n >= 3 {
		if _, ok := versionPairs[rest[0]+"_"+rest[1]]; ok {
			f.Versions, rest = append(f.Versions, rest[0]+"_"+rest[1]), rest[2:]
		} else if _, ok := versionPairs[rest[n-2]+"_"+rest[n-1]]; ok {
			f.Versions, rest = append(f.Versions, rest[n-2]+"_"+rest[n-1]), rest[:n-2]
		}
	}
	if len(rest) >= 2 && isVersion(rest[0]) {
		f.Versions, rest = append(f.Versions, rest[0]), rest[1:]
	}
	if len(rest) >= 2 && (isVersion(rest[len(rest)-1]) || versionMembers[rest[len(rest)-1]]) {
		f.Versions, rest = append(f.Versions, rest[len(rest)-1]), rest[:len(rest)-1]
	}
	f.Measure = strings.Join(rest, "_")
	return f
}

// Flat reports whether the name holds a version or a period.
func (f FlatName) Flat() bool { return len(f.Versions)+len(f.Periods) > 0 && f.Measure != "" }

// Message sends a flat name back with the design to use.
func (f FlatName) Message() string {
	var held []string
	if len(f.Versions) > 0 {
		held = append(held, fmt.Sprintf("a version (%s)", strings.Join(f.Versions, ", ")))
	}
	if len(f.Periods) > 0 {
		held = append(held, fmt.Sprintf("a period (%s)", strings.Join(f.Periods, ", ")))
	}
	var dims []string
	if len(f.Versions) > 0 {
		dims = append(dims, "a Version dimension (Actual, Budget, Forecast — create it if the model has none)")
	}
	if len(f.Periods) > 0 {
		dims = append(dims, "the time dimension (years, quarters and months are its members)")
	}
	return fmt.Sprintf("metric %q holds %s in its name. Think multidimensionally: make it ONE metric %q on a grid with %s, and enter or load "+
		"each version's and period's values at those members — never a metric per version or period", f.Name, strings.Join(held, " and "), f.Measure, strings.Join(dims, " and "))
}

// NamedDimension is a dimension of the model and its members (snake).
type NamedDimension struct {
	Name    string
	Time    bool
	Members []string
}

// VersionMembers are the members of the dimensions that hold versions or
// scenarios.
func VersionMembers(dims []NamedDimension) map[string]bool {
	out := map[string]bool{}
	for _, d := range dims {
		if versionDimRE.MatchString(d.Name) {
			for _, m := range d.Members {
				if len(m) >= 3 {
					out[m] = true
				}
			}
		}
	}
	return out
}

// FlatMetricProblems reads the input metrics steps create (or switch to
// input): the data a version or a period is entered into. problems: names
// holding a version or a period (sent back); warnings: inputs that differ
// only by members of one dimension of the model (asked once). A calculated
// metric is left alone — forecast_base (a statistical forecast) or
// h1_revenue (a half-year total) are measures a formula makes. A name the
// developer's request spells out is theirs.
func FlatMetricProblems(steps []ProposalStep, request string, dims []NamedDimension, existing []string) (problems []string, warnings []string, failed []int) {
	req := strings.ToLower(request)
	vm := VersionMembers(dims)
	type made struct {
		name string
		step int
	}
	var created []made
	for i, s := range steps {
		if s.Tool != "create_metric" && s.Tool != "update_metric" {
			continue
		}
		var p struct {
			Name    string `json:"name"`
			IsInput *bool  `json:"is_input"`
			Formula string `json:"formula"`
		}
		if json.Unmarshal(s.Params, &p) != nil || strings.TrimSpace(p.Name) == "" {
			continue
		}
		input := p.IsInput != nil && *p.IsInput || (s.Tool == "create_metric" && p.IsInput == nil && strings.TrimSpace(p.Formula) == "")
		if !input {
			continue
		}
		if strings.Contains(req, strings.ToLower(strings.TrimSpace(p.Name))) {
			continue
		}
		created = append(created, made{p.Name, i})
	}
	for _, c := range created {
		if f := FlatNameParts(c.name, vm); f.Flat() {
			problems = append(problems, fmt.Sprintf("step %d: %s", c.step+1, f.Message()))
			failed = append(failed, c.step)
		}
	}
	// Families: names alike but for one token, each a member of the same
	// dimension (revenue_emea, revenue_apac with a region dimension).
	all := map[string]bool{}
	for _, e := range existing {
		all[snake(e)] = true
	}
	for _, c := range created {
		all[snake(c.name)] = true
	}
	memberOf := map[string][]string{} // member → dimensions
	for _, d := range dims {
		if d.Time {
			continue
		}
		for _, m := range d.Members {
			if len(m) >= 2 && !strings.Contains(m, "_") {
				memberOf[m] = append(memberOf[m], d.Name)
			}
		}
	}
	seen := map[string]bool{}
	for _, c := range created {
		toks := strings.Split(snake(c.name), "_")
		for i, t := range toks {
			for _, dim := range memberOf[t] {
				var sib []string
				for other := range all {
					ot := strings.Split(other, "_")
					if other == snake(c.name) || len(ot) != len(toks) || !slices.Equal(ot[:i], toks[:i]) || !slices.Equal(ot[i+1:], toks[i+1:]) {
						continue
					}
					if slices.Contains(memberOf[ot[i]], dim) {
						sib = append(sib, other)
					}
				}
				if len(sib) == 0 {
					continue
				}
				measure := strings.Trim(strings.Join(append(append([]string{}, toks[:i]...), toks[i+1:]...), "_"), "_")
				key := dim + "|" + measure
				if seen[key] || measure == "" {
					continue
				}
				seen[key] = true
				sort.Strings(sib)
				warnings = append(warnings, fmt.Sprintf("metrics %s and %s differ only by members of the dimension %s: think multidimensionally — "+
					"one metric %q on a grid with %s holds them all. Propose it that way, or propose it again unchanged if they really are different measures",
					snake(c.name), strings.Join(sib, ", "), dim, measure, dim))
			}
		}
	}
	return problems, warnings, failed
}

// targetFlatMetrics refuses a target model whose metric names hold a
// version or a period.
func targetFlatMetrics(t *TargetModel) error {
	var msgs []string
	for _, s := range t.Sheets {
		if s.Role != "input" && s.Role != "actuals" {
			continue // a calculation or report sheet's rows are formulas
		}
		for _, m := range s.Metrics {
			if f := FlatNameParts(m, nil); f.Flat() {
				msgs = append(msgs, fmt.Sprintf("%s (sheet %s) → one metric %q", m, s.Sheet, f.Measure))
			}
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	if len(msgs) > 12 {
		msgs = append(msgs[:12], fmt.Sprintf("and %d more", len(msgs)-12))
	}
	return fmt.Errorf("set_target_model: think multidimensionally — these input metric names hold a version (actual, budget, forecast) or a period "+
		"(a year, quarter or month), which are members of dimensions in an OLAP model: %s. List a Version dimension (Actual, Budget, Forecast) "+
		"and the time dimension under dimensions, put them on the sheets' grids, and name each measure once", strings.Join(msgs, "; "))
}
