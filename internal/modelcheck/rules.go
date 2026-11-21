package modelcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// revisionContext is what the rules read of a revision, loaded once.
type revisionContext struct {
	q                   Querier
	modelID, revisionID string

	metrics       []metric
	metricByName  map[string]*metric // lower-cased name
	dims          map[string]*dimension
	dimByName     map[string]*dimension // lower-cased name
	analyses      map[string]*formula.Analysis
	gridDimsOfMet map[string]map[string]bool // metric id → its grids' dimension ids
}

type metric struct {
	ID, Name, Lineage, Formula, Format, Agg string
	IsInput, Picklist                       bool
}

type dimension struct {
	ID, Name, Agg      string
	BusinessMaintained bool
}

func loadRevision(ctx context.Context, q Querier, modelID, revisionID string) (*revisionContext, error) {
	rc := &revisionContext{q: q, modelID: modelID, revisionID: revisionID,
		metricByName: map[string]*metric{}, dims: map[string]*dimension{}, dimByName: map[string]*dimension{},
		analyses: map[string]*formula.Analysis{}, gridDimsOfMet: map[string]map[string]bool{}}
	rows, err := q.Query(ctx, `
		SELECT id::text, name, lineage_id::text, COALESCE(formula,''), COALESCE(format,''), COALESCE(agg_rule,''),
		       is_input, picklist_dimension_id IS NOT NULL
		FROM model.metric_def WHERE model_id = $1::uuid AND revision_id = $2::uuid ORDER BY name`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m metric
		if err := rows.Scan(&m.ID, &m.Name, &m.Lineage, &m.Formula, &m.Format, &m.Agg, &m.IsInput, &m.Picklist); err != nil {
			rows.Close()
			return nil, err
		}
		m.Formula = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(m.Formula), "="))
		rc.metrics = append(rc.metrics, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range rc.metrics {
		m := &rc.metrics[i]
		rc.metricByName[strings.ToLower(m.Name)] = m
		if !m.IsInput && m.Formula != "" {
			if an, err := formula.Analyze(m.Formula); err == nil {
				rc.analyses[m.ID] = an
			}
		}
	}
	rows, err = q.Query(ctx, `
		SELECT id::text, name, COALESCE(agg_rule,'sum'), business_maintained
		FROM model.dimension_def WHERE model_id = $1::uuid AND revision_id = $2::uuid`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d dimension
		if err := rows.Scan(&d.ID, &d.Name, &d.Agg, &d.BusinessMaintained); err != nil {
			rows.Close()
			return nil, err
		}
		dd := d
		rc.dims[d.ID] = &dd
		rc.dimByName[strings.ToLower(d.Name)] = &dd
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `
		SELECT gm.metric_id::text, gd.dimension_id::text
		FROM model.grid_metric gm
		JOIN model.grid_def g ON g.id = gm.grid_id
		JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
		WHERE g.revision_id = $1::uuid`, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var mid, did string
		if err := rows.Scan(&mid, &did); err != nil {
			rows.Close()
			return nil, err
		}
		if rc.gridDimsOfMet[mid] == nil {
			rc.gridDimsOfMet[mid] = map[string]bool{}
		}
		rc.gridDimsOfMet[mid][did] = true
	}
	rows.Close()
	return rc, rows.Err()
}

// ── Schedules that cannot start their workflow ───────────────────────────

var ruleScheduleGap = Rule{ID: "schedule_without_values", Title: "Schedule cannot start its workflow", Severity: SeverityError,
	Run: func(ctx context.Context, rc *revisionContext) ([]Finding, error) {
		rows, err := rc.q.Query(ctx, `
			SELECT ar.id::text, ar.name, wd.name, COALESCE(wd.context_schema, '[]'::jsonb)
			FROM workflow.automation_rule ar JOIN workflow.workflow_def wd ON wd.id = ar.workflow_def_id
			WHERE ar.revision_id = $1::uuid AND ar.trigger_type = 'schedule' AND ar.enabled
			ORDER BY ar.name`, rc.revisionID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []Finding
		for rows.Next() {
			var id, name, wf string
			var schema json.RawMessage
			if err := rows.Scan(&id, &name, &wf, &schema); err != nil {
				return nil, err
			}
			gaps := workflow.ScheduleGaps(schema)
			if len(gaps) == 0 {
				continue
			}
			out = append(out, Finding{Object: Object{Kind: "automation_rule", ID: id, Name: name}, Key: key("schedule_without_values", name),
				Message: fmt.Sprintf("Trigger %s starts workflow %s on a schedule, and the workflow needs %s when it starts: a schedule brings no values, so every run fails.", name, wf, list(gaps)),
				Fix:     fmt.Sprintf("Give %s a default in the workflow's Context (a formula such as a date setting or TODAY()), or make it optional.", list(gaps))})
		}
		return out, rows.Err()
	}}

// ── A record field that posts nothing ─────────────────────────────────────

var ruleTextPosting = Rule{ID: "field_posts_nothing", Title: "A record field posts nothing", Severity: SeverityWarning,
	Run: func(ctx context.Context, rc *revisionContext) ([]Finding, error) {
		rows, err := rc.q.Query(ctx, `
			SELECT m.id::text, m.name, f.name, f.fields, m.source_field, m.aggregation, md.name, COALESCE(md.format,'')
			FROM model.form_metric_mapping m
			JOIN model.form_def f ON f.id = m.form_id
			JOIN model.metric_def md ON md.id = m.target_metric_id
			WHERE md.revision_id = $1::uuid
			ORDER BY f.name, m.name`, rc.revisionID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []Finding
		for rows.Next() {
			var id, name, table, field, agg, target, format string
			var raw json.RawMessage
			if err := rows.Scan(&id, &name, &table, &raw, &field, &agg, &target, &format); err != nil {
				return nil, err
			}
			if agg == "count" || format == "text" || field == "" {
				continue
			}
			var fields []crudapp.FormField
			_ = json.Unmarshal(raw, &fields)
			for _, f := range fields {
				if f.Name != field || !crudapp.FieldHoldsNoNumber(f) {
					continue
				}
				out = append(out, Finding{Object: Object{Kind: "mapping", ID: id, Name: name}, Key: key("field_posts_nothing", table, field, target),
					Message: fmt.Sprintf("Table %s maps its field %s, a %s field, into %s, a number metric: the field holds no number, so nothing ever arrives — the cells stay blank.", table, field, f.Type, target),
					Fix:     "Map it into a text metric (each cell takes the latest record's text), count the records (aggregation count), or show the field in a Records widget."})
			}
		}
		return out, rows.Err()
	}}

// ── A calculated pick-list or text whose total is worked out from blanks ──

var rulePicklistTotal = Rule{ID: "total_from_blanks", Title: "A total worked out from blanks", Severity: SeverityWarning,
	Run: func(ctx context.Context, rc *revisionContext) ([]Finding, error) {
		var out []Finding
		for _, m := range rc.metrics {
			if m.IsInput || m.Formula == "" || m.Agg != "formula" || (!m.Picklist && m.Format != "text") {
				continue
			}
			if metricformula.PicklistTotalDefault(ctx, rc.q, rc.modelID, rc.revisionID, m.Formula) != "none" {
				continue
			}
			kind := "pick-list"
			if !m.Picklist {
				kind = "text"
			}
			out = append(out, Finding{Object: Object{Kind: "metric", ID: m.ID, Name: m.Name}, Key: key("total_from_blanks", m.Name),
				Message: fmt.Sprintf("%s is a calculated %s whose total rows are worked out with its formula, from metrics that have no total there or from the row's own member, which a total row leaves blank: a total can show a class or an action no member has.", m.Name, kind),
				Fix:     "Set its Total to none (Developer › Metrics), unless every metric it reads has a total that means something."})
		}
		return out, nil
	}}

// ── A SUMIFS that adds members that are never added up ────────────────────

// conditionalFuncs are the aggregations over matching members.
var conditionalFuncs = map[string]bool{"SUMIFS": true, "AVERAGEIFS": true, "MINIFS": true, "MAXIFS": true, "COUNTIFS": true,
	"SUMIF": true, "AVERAGEIF": true, "COUNTIF": true, "STDEVIFS": true, "VARIFS": true}

var ruleUnsummedSpan = Rule{ID: "adds_unsummed_members", Title: "SUMIFS adds members that are never added up", Severity: SeverityWarning,
	Run: func(ctx context.Context, rc *revisionContext) ([]Finding, error) {
		var out []Finding
		for _, m := range rc.metrics {
			an := rc.analyses[m.ID]
			if an == nil {
				continue
			}
			own := rc.gridDimsOfMet[m.ID]
			reported := map[string]bool{}
			for _, call := range an.DimensionalCalls {
				if !conditionalFuncs[call.Func] {
					continue
				}
				pinned := map[string]bool{} // dimensions a criterion of the call tests
				var metricRanges []*metric
				for _, c := range call.Criteria {
					if d := rc.dimByName[strings.ToLower(c.Dim)]; d != nil {
						pinned[d.ID] = true
					} else if rm := rc.metricByName[strings.ToLower(c.Dim)]; rm != nil {
						metricRanges = append(metricRanges, rm)
					}
				}
				for _, rm := range metricRanges {
					for did := range rc.gridDimsOfMet[rm.ID] {
						d := rc.dims[did]
						if d == nil || pinned[did] || !own[did] || d.Agg == "sum" || reported[did] {
							continue
						}
						reported[did] = true
						src := call.Source
						if src == "" {
							src = "the matches"
						}
						out = append(out, Finding{Object: Object{Kind: "metric", ID: m.ID, Name: m.Name}, Key: key("adds_unsummed_members", m.Name, d.Name),
							Message: fmt.Sprintf("%s: %s tests %s at every member of %s — %s is dimensioned by %s — so it combines %s across all %s members, while the members of %s are never added up (its total is %q).",
								m.Name, call.Func, rm.Name, d.Name, rm.Name, d.Name, src, d.Name, d.Name, d.Agg),
							Fix: fmt.Sprintf("Add the criterion %s, %s to keep the cell's own %s member: %s(…, %s, %s).", formula.QuoteName(d.Name), formula.QuoteName(d.Name), d.Name, call.Func, formula.QuoteName(d.Name), formula.QuoteName(d.Name))})
					}
				}
			}
		}
		return out, nil
	}}

// ── Member lists pinned on a dimension business users maintain ────────────

var rulePinnedMembers = Rule{ID: "pinned_members", Title: "Fixed member list on a business-maintained dimension", Severity: SeverityWarning,
	Run: func(ctx context.Context, rc *revisionContext) ([]Finding, error) {
		var out []Finding
		add := func(kind, id, name, dashboard, dimID string, n int) {
			d := rc.dims[dimID]
			if d == nil || !d.BusinessMaintained || n == 0 {
				return
			}
			out = append(out, Finding{Object: Object{Kind: kind, ID: id, Name: name, Dashboard: dashboard}, Key: key("pinned_members", dashboard, name, d.Name),
				Message: fmt.Sprintf("%s on dashboard %s shows a fixed list of %d %s members, and business users maintain %s: members they add later will not appear there.", name, dashboard, n, d.Name, d.Name),
				Fix:     "Clear the list to show every member, and let the dashboard's selector bar narrow it."})
		}
		rows, err := rc.q.Query(ctx, `
			SELECT w.id::text, COALESCE(NULLIF(w.content, ''), w.widget_type || ' ' || (w.sort_order + 1)::text), d.name, COALESCE(w.widget_props, '{}'::jsonb)
			FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id = w.dashboard_id
			WHERE d.revision_id = $1::uuid ORDER BY d.name, w.sort_order`, rc.revisionID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, name, dash string
			var raw json.RawMessage
			if err := rows.Scan(&id, &name, &dash, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			var props struct {
				ShowMembers map[string][]string `json:"show_members"`
				Chart       struct {
					DimensionID    string   `json:"dimension_id"`
					PlottedMembers []string `json:"plotted_members"`
				} `json:"chart"`
			}
			_ = json.Unmarshal(raw, &props)
			ids := make([]string, 0, len(props.ShowMembers))
			for did := range props.ShowMembers {
				ids = append(ids, did)
			}
			sort.Strings(ids)
			for _, did := range ids {
				add("widget", id, name, dash, did, len(props.ShowMembers[did]))
			}
			add("widget", id, name, dash, props.Chart.DimensionID, len(props.Chart.PlottedMembers))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows, err = rc.q.Query(ctx, `
			SELECT d.id::text, d.name, COALESCE(d.selector_bar, '[]'::jsonb)
			FROM model.dashboard_def d WHERE d.revision_id = $1::uuid AND jsonb_typeof(d.selector_bar) = 'array' ORDER BY d.name`, rc.revisionID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id, dash string
			var raw json.RawMessage
			if err := rows.Scan(&id, &dash, &raw); err != nil {
				return nil, err
			}
			var bar []struct {
				DimensionID string   `json:"dimension_id"`
				Members     []string `json:"members"`
			}
			_ = json.Unmarshal(raw, &bar)
			for _, s := range bar {
				add("dashboard", id, "The selector bar", dash, s.DimensionID, len(s.Members))
			}
		}
		return out, rows.Err()
	}}

// ── Hidden members under figures read across them ─────────────────────────

var ruleHiddenReads = Rule{ID: "hidden_members_blank_figures", Title: "Hidden members blank figures built across them", Severity: SeverityWarning,
	Run: func(ctx context.Context, rc *revisionContext) ([]Finding, error) {
		rows, err := rc.q.Query(ctx, `
			SELECT br.id::text, br.name, dm.dimension_id::text, count(DISTINCT dm.id)
			FROM identity.business_role_access_rule r
			JOIN identity.business_role br ON br.id = r.role_id
			JOIN model.dimension_member dm ON dm.id::text = r.ref_id OR dm.lineage_id = r.ref_lineage_id
			JOIN model.dimension_def dd ON dd.id = dm.dimension_id
			WHERE r.rule_type = 'dimension_member' AND r.access = 'hidden' AND dd.revision_id = $1::uuid
			GROUP BY br.id, br.name, dm.dimension_id ORDER BY br.name`, rc.revisionID)
		if err != nil {
			return nil, err
		}
		type hidden struct {
			roleID, role, dimID string
			n                   int
		}
		var hiddenSets []hidden
		for rows.Next() {
			var h hidden
			if err := rows.Scan(&h.roleID, &h.role, &h.dimID, &h.n); err != nil {
				rows.Close()
				return nil, err
			}
			hiddenSets = append(hiddenSets, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		var out []Finding
		for _, h := range hiddenSets {
			d := rc.dims[h.dimID]
			if d == nil {
				continue
			}
			hiddenMetric, err := rc.metricsHiddenFrom(ctx, h.roleID)
			if err != nil {
				return nil, err
			}
			var readers []string
			for _, m := range rc.metrics {
				if hiddenMetric[m.ID] || !rc.readsAcross(m.ID, d) {
					continue
				}
				readers = append(readers, m.Name)
			}
			if len(readers) == 0 {
				continue
			}
			out = append(out, Finding{Object: Object{Kind: "role", ID: h.roleID, Name: h.role}, Key: key("hidden_members_blank_figures", h.role, d.Name),
				Message: fmt.Sprintf("Role %s cannot see %d %s member(s), and %s read(s) across every %s member: for %s they are withheld wherever they read a hidden member — shown blank, with what is built on them.",
					h.role, h.n, d.Name, list(readers), d.Name, h.role),
				Fix: fmt.Sprintf("Make those members read-only for %s instead of hidden, if %s may see figures built from them (rankings, shares, network totals); or keep those metrics off %s's dashboards.", h.role, h.role, h.role)})
		}
		return out, nil
	}}

// metricsHiddenFrom is the set of metric ids a role may not see.
func (rc *revisionContext) metricsHiddenFrom(ctx context.Context, roleID string) (map[string]bool, error) {
	rows, err := rc.q.Query(ctx, `
		SELECT md.id::text FROM identity.business_role_access_rule r
		JOIN model.metric_def md ON md.id::text = r.ref_id OR md.lineage_id = r.ref_lineage_id
		WHERE r.role_id = $1::uuid AND r.rule_type = 'metric' AND r.access = 'hidden' AND md.revision_id = $2::uuid`, roleID, rc.revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// readsAcross reports whether a metric's formula reads other members of d:
// a LOOKUP moving it, or a conditional aggregation testing d, a property
// of it, or a metric dimensioned by it.
func (rc *revisionContext) readsAcross(metricID string, d *dimension) bool {
	an := rc.analyses[metricID]
	if an == nil {
		return false
	}
	for _, call := range an.DimensionalCalls {
		for _, name := range call.Dims {
			if strings.EqualFold(name, d.Name) {
				return true
			}
			if rm := rc.metricByName[strings.ToLower(name)]; rm != nil && rc.gridDimsOfMet[rm.ID][d.ID] {
				return true
			}
		}
		for _, c := range append(append([]formula.DimensionalArg(nil), call.Criteria...), call.Members...) {
			if strings.EqualFold(c.Dim, d.Name) {
				return true
			}
			if rm := rc.metricByName[strings.ToLower(c.Dim)]; rm != nil && rc.gridDimsOfMet[rm.ID][d.ID] {
				return true
			}
		}
	}
	return false
}

// ── An input open to every role while others are restricted ───────────────

var ruleOpenInput = Rule{ID: "input_open_to_every_role", Title: "Input open to every business role", Severity: SeverityInfo,
	Run: func(ctx context.Context, rc *revisionContext) ([]Finding, error) {
		rows, err := rc.q.Query(ctx, `
			SELECT DISTINCT md.id::text FROM identity.business_role_access_rule r
			JOIN model.metric_def md ON md.id::text = r.ref_id OR md.lineage_id = r.ref_lineage_id
			WHERE r.rule_type = 'metric' AND md.revision_id = $1::uuid`, rc.revisionID)
		if err != nil {
			return nil, err
		}
		ruled := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			ruled[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		var restrictedInputs int
		for _, m := range rc.metrics {
			if m.IsInput && ruled[m.ID] {
				restrictedInputs++
			}
		}
		if restrictedInputs == 0 {
			return nil, nil // access is not set per metric in this model
		}
		rows, err = rc.q.Query(ctx, `
			SELECT DISTINCT target_metric_id::text FROM model.form_metric_mapping m
			JOIN model.metric_def md ON md.id = m.target_metric_id WHERE md.revision_id = $1::uuid`, rc.revisionID)
		if err != nil {
			return nil, err
		}
		fed := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			fed[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		var out []Finding
		for _, m := range rc.metrics {
			if !m.IsInput || ruled[m.ID] || fed[m.ID] || (len(rc.gridDimsOfMet[m.ID]) == 0 && !rc.onAGrid(ctx, m.ID)) {
				continue
			}
			out = append(out, Finding{Object: Object{Kind: "metric", ID: m.ID, Name: m.Name}, Key: key("input_open_to_every_role", m.Name),
				Message: fmt.Sprintf("Every business role may write %s: no role makes it read-only or hidden, while %d other input(s) of this model are restricted by role.", m.Name, restrictedInputs),
				Fix:     "Set who may write it in the business roles' access (Roles › access), or ignore this if everyone should."})
		}
		return out, nil
	}}

// onAGrid reports whether a metric is on a grid with no dimensions (which
// gridDimsOfMet, built from grid dimensions, does not list).
func (rc *revisionContext) onAGrid(ctx context.Context, metricID string) bool {
	var on bool
	_ = rc.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM model.grid_metric WHERE metric_id = $1::uuid)`, metricID).Scan(&on)
	return on
}
