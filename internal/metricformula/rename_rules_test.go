package metricformula

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// renameFixture adds to setupDimFixture what names metrics, dimensions and
// members by text outside formulas, and a second revision whose rows of
// the same names must never change.
type renameFixture struct {
	*dimFixture
	t     *testing.T
	appID string
	rev2  string
}

func newRenameFixture(t *testing.T) *renameFixture {
	f := &renameFixture{dimFixture: setupDimFixture(t), t: t}
	f.appID = f.one(`SELECT application_id::text FROM core.model WHERE id=$1::uuid`, f.modelID)
	f.rev2 = f.one(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Other') RETURNING id::text`, f.modelID)
	return f
}

func (f *renameFixture) one(sql string, args ...any) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		f.t.Fatalf("query %q: %v", sql, err)
	}
	return s
}

func (f *renameFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *renameFixture) metric(rev, name, text string) string {
	return f.one(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula) VALUES ($1::uuid, $2::uuid, $3, $4, NULLIF($5,'')) RETURNING id::text`,
		f.modelID, rev, name, text == "", text)
}

func (f *renameFixture) export(rev, name, config string) string {
	return f.one(`INSERT INTO model.integration_def (model_id, revision_id, name, type, config) VALUES ($1::uuid, $2::uuid, $3, 'file_export', $4::jsonb) RETURNING id::text`,
		f.modelID, rev, name, config)
}

// jsonOf reads a jsonb column as a Go value, to compare with what a test
// expects.
func (f *renameFixture) jsonOf(sql string, args ...any) any {
	f.t.Helper()
	var v any
	if err := json.Unmarshal([]byte(f.one(sql, args...)), &v); err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *renameFixture) wantJSON(what string, got any, want string) {
	f.t.Helper()
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		f.t.Fatal(err)
	}
	if !reflect.DeepEqual(got, w) {
		b, _ := json.Marshal(got)
		f.t.Errorf("%s = %s, want %s", what, b, want)
	}
}

// TestRenameMetricInRules: a metric's rename reaches the highlight rules
// (metric and than), the closed rules and the data exports (metrics and a
// column name keyed by the metric's name) of its revision — in any case —
// and nothing of another revision. Numbers in a rule stay as written.
func TestRenameMetricInRules(t *testing.T) {
	f := newRenameFixture(t)
	ctx := context.Background()
	rules := `[{"op":">","than":"other","tone":"negative"},{"metric":"OTHER","op":"=","value":1,"tone":"info"},` +
		`{"op":">","value":5,"tone":"warning","members":["EMEA"],"dimension":"region"}]`
	f.exec(`UPDATE model.metric_def SET highlight_rules=$2::jsonb WHERE id=$1::uuid`, f.revenue, rules)
	f.exec(`UPDATE model.metric_def SET closed_when='[{"metric":"other","op":"=","value":1}]' WHERE id=$1::uuid`, f.flow)
	exp := f.export(f.revID, "Nightly", `{"metrics":["revenue","Other"],"column_names":{"other":"Other amount","revenue":"Revenue"},"decimals":2}`)
	labels := f.export(f.revID, "By label", `{"metrics":["other"],"metric_display":"label","column_names":{"other":"Kept"}}`)

	// The other revision has its own "other", and rules and an export naming it.
	f.metric(f.rev2, "other", "")
	x2 := f.metric(f.rev2, "x", "")
	f.exec(`UPDATE model.metric_def SET highlight_rules=$2::jsonb, closed_when='[{"metric":"other","op":"=","value":1}]' WHERE id=$1::uuid`, x2, rules)
	exp2 := f.export(f.rev2, "Nightly", `{"metrics":["other"],"column_names":{"other":"Other amount"}}`)

	f.exec(`UPDATE model.metric_def SET name='other_ind' WHERE id=$1::uuid`, f.other)
	n, err := RenameMetricInRules(ctx, f.pool, f.other, "other", "other_ind")
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("RenameMetricInRules rewrote %d holders, want 4 (revenue's rules, flow's closed rule, two exports)", n)
	}
	f.wantJSON("revenue's highlight rules", f.jsonOf(`SELECT highlight_rules::text FROM model.metric_def WHERE id=$1::uuid`, f.revenue),
		`[{"op":">","than":"other_ind","tone":"negative"},{"metric":"other_ind","op":"=","value":1,"tone":"info"},`+
			`{"op":">","value":5,"tone":"warning","members":["EMEA"],"dimension":"region"}]`)
	f.wantJSON("flow's closed rule", f.jsonOf(`SELECT closed_when::text FROM model.metric_def WHERE id=$1::uuid`, f.flow),
		`[{"metric":"other_ind","op":"=","value":1}]`)
	f.wantJSON("the export", f.jsonOf(`SELECT config::text FROM model.integration_def WHERE id=$1::uuid`, exp),
		`{"metrics":["revenue","other_ind"],"column_names":{"other_ind":"Other amount","revenue":"Revenue"},"decimals":2}`)
	f.wantJSON("an export headed by labels", f.jsonOf(`SELECT config::text FROM model.integration_def WHERE id=$1::uuid`, labels),
		`{"metrics":["other_ind"],"metric_display":"label","column_names":{"other":"Kept"}}`)
	if raw := f.one(`SELECT config::text FROM model.integration_def WHERE id=$1::uuid`, exp); !strings.Contains(raw, `"decimals": 2`) {
		t.Errorf("the export's decimals no longer read 2: %s", raw)
	}

	f.wantJSON("the other revision's rules", f.jsonOf(`SELECT highlight_rules::text FROM model.metric_def WHERE id=$1::uuid`, x2), rules)
	f.wantJSON("the other revision's closed rule", f.jsonOf(`SELECT closed_when::text FROM model.metric_def WHERE id=$1::uuid`, x2),
		`[{"metric":"other","op":"=","value":1}]`)
	f.wantJSON("the other revision's export", f.jsonOf(`SELECT config::text FROM model.integration_def WHERE id=$1::uuid`, exp2),
		`{"metrics":["other"],"column_names":{"other":"Other amount"}}`)

	// A change of case only is no rename: names match without case.
	if n, err := RenameMetricInRules(ctx, f.pool, f.other, "other_ind", "OTHER_IND"); err != nil || n != 0 {
		t.Errorf("a case-only rename: %d, %v; want 0, nil", n, err)
	}
}

// TestCheckMetricNotInUseCountsRulesAndConditions: a metric a highlight
// rule, a closed rule, a workflow condition or a data export of its
// revision names cannot be deleted either; the error names each holder. A
// holder in another revision does not count.
func TestCheckMetricNotInUseCountsRulesAndConditions(t *testing.T) {
	f := newRenameFixture(t)
	ctx := context.Background()
	if err := CheckMetricNotInUse(ctx, f.pool, f.other); err != nil {
		t.Fatalf("other, read by nothing yet: %v", err)
	}
	// In another revision: not this metric's holders.
	x2 := f.metric(f.rev2, "x", "")
	f.exec(`UPDATE model.metric_def SET highlight_rules='[{"op":">","than":"other","tone":"info"}]' WHERE id=$1::uuid`, x2)
	f.exec(`INSERT INTO workflow.workflow_def (application_id, revision_id, name, trigger_event, steps, status)
		VALUES ($1::uuid, $2::uuid, 'Gate', 'manual', '[{"id":"s1","name":"Check","type":"condition","condition":{"formula":"other > 0"}}]', 'published')`, f.appID, f.rev2)
	f.export(f.rev2, "Nightly", `{"metrics":["other"]}`)
	if err := CheckMetricNotInUse(ctx, f.pool, f.other); err != nil {
		t.Fatalf("other, named only in another revision: %v", err)
	}

	f.exec(`UPDATE model.metric_def SET highlight_rules='[{"op":">","than":"Other","tone":"negative"}]' WHERE id=$1::uuid`, f.revenue)
	f.exec(`UPDATE model.metric_def SET closed_when='[{"metric":"other","op":"=","value":1}]' WHERE id=$1::uuid`, f.flow)
	f.exec(`INSERT INTO workflow.workflow_def (application_id, revision_id, name, trigger_event, steps, status)
		VALUES ($1::uuid, $2::uuid, 'Gate', 'manual', '[{"id":"s0","name":"Review","type":"approval"},{"id":"s1","name":"Check","type":"condition","condition":{"formula":"= other > 0"}}]', 'published')`, f.appID, f.revID)
	f.export(f.revID, "Nightly", `{"metrics":["revenue","OTHER"]}`)

	err := CheckMetricNotInUse(ctx, f.pool, f.other)
	if codeOf(err) != CodeMetricInUse {
		t.Fatalf("want METRIC_IN_USE, got %v", err)
	}
	for _, holder := range []string{"revenue (its highlight rules)", "flow (its closed rule)", "workflow Gate (condition step Check)", "data export Nightly"} {
		if !strings.Contains(err.Error(), holder) {
			t.Errorf("the error %q does not name %s", err, holder)
		}
	}
}

// TestRenameDimensionInFormulas: a dimension's rename reaches every formula
// of its revision that names it, the highlight rules limited to its
// members and the data exports listing, pivoting, filtering or heading a
// column by it — and nothing of another revision.
func TestRenameDimensionInFormulas(t *testing.T) {
	f := newRenameFixture(t)
	ctx := context.Background()
	scaled := f.metric(f.revID, "scaled", `revenue * region.factor`)
	lk := f.metric(f.revID, "lk", `LOOKUP(revenue, Region, "EMEA")`)
	cmp := f.metric(f.revID, "cmp", `IF({region} = "US", 1, 0)`)
	plain := f.metric(f.revID, "plain", `revenue * 2`)
	other := f.metric(f.rev2, "scaled", `revenue * region.factor`)
	f.exec(`UPDATE model.metric_def SET highlight_rules='[{"op":">","value":5,"tone":"warning","members":["EMEA"],"dimension":"Region"}]' WHERE id=$1::uuid`, f.revenue)
	exp := f.export(f.revID, "Pivot", `{"layout":"pivot","pivot_dimension":"region","dimensions":["Region","month"],`+
		`"filters":{"Region":["EMEA"]},"column_names":{"region":"Area","region label":"Area name","month":"Month"}}`)
	exp2 := f.export(f.rev2, "Pivot", `{"dimensions":["region"]}`)

	f.exec(`UPDATE model.dimension_def SET name='territory' WHERE id=$1::uuid`, f.region)
	ids, err := RenameDimensionInFormulas(ctx, f.pool, f.region, "region", "territory")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Errorf("rewrote %d formulas, want 3", len(ids))
	}
	for id, want := range map[string]string{
		scaled: `revenue * territory.factor`,
		lk:     `LOOKUP(revenue, territory, "EMEA")`,
		cmp:    `IF(territory = "US", 1, 0)`,
		plain:  `revenue * 2`,
		other:  `revenue * region.factor`,
	} {
		if got := f.one(`SELECT formula FROM model.metric_def WHERE id=$1::uuid`, id); got != want {
			t.Errorf("formula %q, want %q", got, want)
		}
	}
	f.wantJSON("the highlight rule", f.jsonOf(`SELECT highlight_rules::text FROM model.metric_def WHERE id=$1::uuid`, f.revenue),
		`[{"op":">","value":5,"tone":"warning","members":["EMEA"],"dimension":"territory"}]`)
	f.wantJSON("the export", f.jsonOf(`SELECT config::text FROM model.integration_def WHERE id=$1::uuid`, exp),
		`{"layout":"pivot","pivot_dimension":"territory","dimensions":["territory","month"],`+
			`"filters":{"territory":["EMEA"]},"column_names":{"territory":"Area","territory label":"Area name","month":"Month"}}`)
	f.wantJSON("the other revision's export", f.jsonOf(`SELECT config::text FROM model.integration_def WHERE id=$1::uuid`, exp2), `{"dimensions":["region"]}`)

	// A bare name a metric of the revision also carries (a clash saved
	// before such names were refused) is the metric's: only channel.prop
	// references are the dimension's.
	both := f.metric(f.revID, "both", `channel * 2 + channel.tier`)
	if _, err := RenameDimensionInFormulas(ctx, f.pool, f.channel, "channel", "route"); err != nil {
		t.Fatal(err)
	}
	if got := f.one(`SELECT formula FROM model.metric_def WHERE id=$1::uuid`, both); got != `channel * 2 + route.tier` {
		t.Errorf("a clash: formula %q, want %q", got, `channel * 2 + route.tier`)
	}
}

// TestRenameMemberInFormulas: a member's new code reaches the literals of
// its revision's formulas that name it (a TIMESUM bound only on a grid
// carrying the time dimension) and the data exports filtering by it or
// heading a pivot column with it — and nothing else.
func TestRenameMemberInFormulas(t *testing.T) {
	f := newRenameFixture(t)
	ctx := context.Background()
	lk := f.metric(f.revID, "lk", `LOOKUP(revenue, region, "EMEA")`)
	isEMEA := f.metric(f.revID, "is_emea", `IF(region = "EMEA", 1, 0)`)
	notEMEA := f.metric(f.revID, "not_emea", `SUMIFS(revenue, region, "<>EMEA")`)
	product := f.metric(f.revID, "by_product", `LOOKUP(revenue, product, "P1")`)
	other := f.metric(f.rev2, "lk", `LOOKUP(revenue, region, "EMEA")`)
	tsOn := f.metric(f.revID, "ts_on", `TIMESUM(revenue, "2026-01", "2026-03")`)
	tsOff := f.metric(f.revID, "ts_off", `TIMESUM(revenue, "2026-01", "2026-03")`)
	f.exec(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, f.g1, tsOn)
	f.exec(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, f.g2, tsOff)
	exp := f.export(f.revID, "Pivot", `{"layout":"pivot","pivot_dimension":"Region","filters":{"region":["EMEA","US"],"product":["EMEA"]},"column_names":{"EMEA":"Europe"}}`)
	exp2 := f.export(f.rev2, "Pivot", `{"filters":{"region":["EMEA"]}}`)

	f.exec(`UPDATE model.dimension_member SET code='EUR' WHERE dimension_id=$1::uuid AND code='EMEA'`, f.region)
	ids, err := RenameMemberInFormulas(ctx, f.pool, f.region, "EMEA", "EUR")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Errorf("rewrote %d formulas, want 3", len(ids))
	}
	for id, want := range map[string]string{
		lk:      `LOOKUP(revenue, region, "EUR")`,
		isEMEA:  `IF(region = "EUR", 1, 0)`,
		notEMEA: `SUMIFS(revenue, region, "<>EUR")`,
		product: `LOOKUP(revenue, product, "P1")`,
		other:   `LOOKUP(revenue, region, "EMEA")`,
	} {
		if got := f.one(`SELECT formula FROM model.metric_def WHERE id=$1::uuid`, id); got != want {
			t.Errorf("formula %q, want %q", got, want)
		}
	}
	f.wantJSON("the export", f.jsonOf(`SELECT config::text FROM model.integration_def WHERE id=$1::uuid`, exp),
		`{"layout":"pivot","pivot_dimension":"Region","filters":{"region":["EUR","US"],"product":["EMEA"]},"column_names":{"EUR":"Europe"}}`)
	f.wantJSON("the other revision's export", f.jsonOf(`SELECT config::text FROM model.integration_def WHERE id=$1::uuid`, exp2), `{"filters":{"region":["EMEA"]}}`)

	// A period code: a TIMESUM bound follows only on a grid with the month.
	f.exec(`UPDATE model.dimension_member SET code='2026-M01' WHERE dimension_id=$1::uuid AND code='2026-01'`, f.month)
	if _, err := RenameMemberInFormulas(ctx, f.pool, f.month, "2026-01", "2026-M01"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		tsOn:  `TIMESUM(revenue, "2026-M01", "2026-03")`,
		tsOff: `TIMESUM(revenue, "2026-01", "2026-03")`,
	} {
		if got := f.one(`SELECT formula FROM model.metric_def WHERE id=$1::uuid`, id); got != want {
			t.Errorf("formula %q, want %q", got, want)
		}
	}
}

// TestRenameMetricInRulesReachesValues: a metric's rename also reaches what
// holds its name as a value in its revision — a grid loader's column map, a
// connector's mapping into a grid, a record's field of type metric, and the
// Metric field of a workflow run — and nothing of another revision. Left on
// the old name, a saved import failed, a record named no metric and a run
// lost its metric (2026-11-09).
func TestRenameMetricInRulesReachesValues(t *testing.T) {
	f := newRenameFixture(t)
	ctx := context.Background()
	user := f.one(`INSERT INTO identity.user (keycloak_sub, email) VALUES ('rn', 'rn@t.co') RETURNING id::text`)
	loader := func(rev string) string {
		return f.one(`INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, config)
			VALUES ($1::uuid, $2::uuid, 'Actuals', 'csv_import', 'grid', '{"column_map":{"Other amount":"Other","Region":"region","Note":"ignore"}}') RETURNING id::text`,
			f.modelID, rev)
	}
	connector := func(rev string) string {
		return f.one(`INSERT INTO model.integration_def (model_id, revision_id, name, type, config)
			VALUES ($1::uuid, $2::uuid, 'Pull', 'rest_api', '{"target_type":"grid","mapping":{"fields":[{"source":"$.amt","target":"other"},{"source":"$.r","target":"region"}]}}') RETURNING id::text`,
			f.modelID, rev)
	}
	record := func(rev string) string {
		form := f.one(`INSERT INTO model.form_def (model_id, revision_id, name, label, fields) VALUES ($1::uuid, $2::uuid, 'adj', 'Adjustments',
			'[{"name":"which","label":"Which","type":"metric"},{"name":"note","label":"Note","type":"text"}]') RETURNING id::text`, f.modelID, rev)
		return f.one(`INSERT INTO runtime.form_record (form_id, data, created_by) VALUES ($1::uuid, '{"which":"other","note":"other"}', $2::uuid) RETURNING id::text`, form, user)
	}
	run := func(rev string) string {
		def := f.one(`INSERT INTO workflow.workflow_def (application_id, revision_id, name, trigger_event, steps, status, context_schema)
			VALUES ($1::uuid, $2::uuid, 'Approve', 'manual', '[]', 'published', '[{"key":"m","data_type":"Metric"},{"key":"t","data_type":"Text"}]') RETURNING id::text`, f.appID, rev)
		return f.one(`INSERT INTO workflow.workflow_instance (workflow_def_id, context, started_by) VALUES ($1::uuid, '{"m":"OTHER","t":"other"}', $2::uuid) RETURNING id::text`, def, user)
	}
	l1, c1, r1, w1 := loader(f.revID), connector(f.revID), record(f.revID), run(f.revID)
	l2, c2, r2, w2 := loader(f.rev2), connector(f.rev2), record(f.rev2), run(f.rev2)

	f.exec(`UPDATE model.metric_def SET name='other_ind' WHERE id=$1::uuid`, f.other)
	n, err := RenameMetricInRules(ctx, f.pool, f.other, "other", "other_ind")
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("rewrote %d holders, want 4 (a loader, a connector, a record, a run)", n)
	}
	f.wantJSON("the loader", f.jsonOf(`SELECT config::text FROM model.integration_def WHERE id=$1::uuid`, l1),
		`{"column_map":{"Other amount":"other_ind","Region":"region","Note":"ignore"}}`)
	f.wantJSON("the connector", f.jsonOf(`SELECT config::text FROM model.integration_def WHERE id=$1::uuid`, c1),
		`{"target_type":"grid","mapping":{"fields":[{"source":"$.amt","target":"other_ind"},{"source":"$.r","target":"region"}]}}`)
	f.wantJSON("the record", f.jsonOf(`SELECT data::text FROM runtime.form_record WHERE id=$1::uuid`, r1), `{"which":"other_ind","note":"other"}`)
	f.wantJSON("the run", f.jsonOf(`SELECT context::text FROM workflow.workflow_instance WHERE id=$1::uuid`, w1), `{"m":"other_ind","t":"other"}`)

	f.wantJSON("the other revision's loader", f.jsonOf(`SELECT config::text FROM model.integration_def WHERE id=$1::uuid`, l2),
		`{"column_map":{"Other amount":"Other","Region":"region","Note":"ignore"}}`)
	f.wantJSON("the other revision's connector", f.jsonOf(`SELECT config::text FROM model.integration_def WHERE id=$1::uuid`, c2),
		`{"target_type":"grid","mapping":{"fields":[{"source":"$.amt","target":"other"},{"source":"$.r","target":"region"}]}}`)
	f.wantJSON("the other revision's record", f.jsonOf(`SELECT data::text FROM runtime.form_record WHERE id=$1::uuid`, r2), `{"which":"other","note":"other"}`)
	f.wantJSON("the other revision's run", f.jsonOf(`SELECT context::text FROM workflow.workflow_instance WHERE id=$1::uuid`, w2), `{"m":"OTHER","t":"other"}`)
}
