package gateway

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// The map an executed import loaded a sheet with is the map the server checks
// it by after the plan, cell by cell — months by their header dates, never by
// position — and a sheet so checked is not compared row by row as well
// (2026-11-14: an FY column read as next January made a correct Actuals sheet
// "differ" on all 103 rows, and the AI changed correct data).
func TestStageCheckReplaysTheLoadMapCellByCell(t *testing.T) {
	p := &recordingProvider{reply: "ok"}
	env := newAITurnEnv(t, p)
	ctx := context.Background()
	pool := env.chat.Pool()
	sess, err := env.chat.GetSession(ctx, env.session)
	if err != nil {
		t.Fatal(err)
	}
	var revID string
	if err := pool.QueryRow(ctx, `SELECT active_revision_id::text FROM core.model WHERE id = $1::uuid`, sess.ModelID).Scan(&revID); err != nil {
		t.Fatal(err)
	}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	period := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type, time_granularity, fiscal_year_start_month)
		VALUES ($1::uuid, $2::uuid, 'period', 'time', 'month', 1) RETURNING id::text`, sess.ModelID, revID)
	fy := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'FY2026', 'FY 2026') RETURNING id::text`, period)
	for i, code := range []string{"2026-01", "2026-02"} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, period_start, period_end, time_index)
			VALUES ($1::uuid, $2, $2, $3::uuid, ($2 || '-01')::date, (($2 || '-01')::date + interval '1 month' - interval '1 day')::date, $4) RETURNING id::text`,
			period, code, fy, i)
	}
	acct := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'account') RETURNING id::text`, sess.ModelID, revID)
	for _, c := range []string{"A1", "A2"} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, $2, $2) RETURNING id::text`, acct, c)
	}
	metric := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule, time_summary, format)
		VALUES ($1::uuid, $2::uuid, 'expense', true, 'sum', 'sum', 'number') RETURNING id::text`, sess.ModelID, revID)
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Expenses') RETURNING id::text`, sess.ModelID, revID)
	for _, d := range []string{period, acct} {
		q(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid) RETURNING grid_id::text`, grid, d)
	}
	q(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid) RETURNING grid_id::text`, grid, metric)
	for _, f := range []struct {
		acct, month string
		v           float64
	}{{"A1", "2026-01", 10}, {"A1", "2026-02", 20}, {"A2", "2026-01", 1}, {"A2", "2026-02", 2}} {
		q(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
			VALUES ($1::uuid, $2::uuid, $3::uuid, jsonb_build_object($4::text, $5::text, $6::text, $7::text), $8) RETURNING id::text`,
			sess.ModelID, revID, metric, period, f.month, acct, f.acct, f.v)
	}

	// The sheet: a title, months as dates across, then an FY total column.
	f := excelize.NewFile()
	_ = f.SetSheetName("Sheet1", "Actuals")
	set := func(cell string, v any) { _ = f.SetCellValue("Actuals", cell, v) }
	set("A1", "ACTUALS")
	set("A3", "Account")
	set("B3", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	set("C3", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	set("D3", "FY")
	for r, row := range [][]any{{"A1", 10, 20, 30}, {"A2", 1, 2, 3}} {
		for c, v := range row {
			cell, _ := excelize.CoordinatesToCellName(c+1, r+4)
			set(cell, v)
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	if _, err := env.docs.CreateDocument(ctx, env.session, "actuals.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "overview", false, buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if status, _ := env.send("load the actuals"); status != http.StatusOK {
		t.Fatalf("turn 1: %d", status)
	}
	prop, err := env.proposals.CreateProposal(ctx, env.session, []aiassistant.ProposalStep{{Tool: "import_file_data", Status: "success",
		Params: []byte(`{"file":"actuals.xlsx","sheet":"Actuals","target_type":"grid","target_id":"Expenses",
			"reshape":{"header_row":3,"unpivot":{"from":"2026-01-01","to":"2026-02-01","name_column":"period","value_column":"expense"}},
			"column_map":{"*":"ignore","Account":"account","period":"period","expense":"expense"}}`)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.proposals.SetStatus(ctx, prop.ID, "executed"); err != nil {
		t.Fatal(err)
	}
	if status, _ := env.send("go on"); status != http.StatusOK {
		t.Fatalf("turn 2: %d", status)
	}
	msgs, _ := env.chat.ListMessages(ctx, env.session)
	note := ""
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, stageCheckHead) {
			note = m.Content
		}
	}
	for _, want := range []string{"Checked cell by cell", "4 cell(s) — all match", `block "loaded into Expenses from row 3"`,
		`column D "FY" (2 numbers) — beside it are the periods of FY2026`} {
		if !strings.Contains(note, want) {
			t.Errorf("the server's check lacks %q:\n%s", want, note)
		}
	}
	// The note is the developer's: what matched and what happens next, no
	// instructions to the model (owner, 2026-11-16: "once its rows match,
	// mark it done with update_task_plan" under "all match" read as a task
	// for the developer). The model gets the note and the instructions.
	assertDeveloperNote(t, note)
	p.mu.Lock()
	given := p.lastUser
	p.mu.Unlock()
	if !strings.Contains(given, note) {
		t.Errorf("the model was not given the check:\n%s", given)
	}
	if strings.Contains(note, "periods match") {
		t.Errorf("a sheet with a map was also compared row by row:\n%s", note)
	}
	saved, err := aiassistant.LoadSheetMaps(ctx, pool, sess.ModelID, nil, "Actuals")
	if err != nil || len(saved) != 1 || saved[0].Compared != 4 || saved[0].Matched != 4 || saved[0].Revision != revID {
		t.Fatalf("saved maps = %+v, %v", saved, err)
	}
	if v := aiassistant.SheetMapVerdicts(ctx, pool, sess.ModelID, revID); v["actuals"] != "" {
		t.Errorf("the sheet's verdict = %q, want it done", v["actuals"])
	}

	notes := func() int {
		msgs, _ := env.chat.ListMessages(ctx, env.session)
		n := 0
		for _, m := range msgs {
			if strings.HasPrefix(m.Content, stageCheckHead) {
				n++
				note = m.Content
			}
		}
		return n
	}
	run := func(tools ...string) {
		t.Helper()
		var steps []aiassistant.ProposalStep
		for _, tool := range tools {
			steps = append(steps, aiassistant.ProposalStep{Tool: tool, Status: "success", Params: []byte(`{}`)})
		}
		p, err := env.proposals.CreateProposal(ctx, env.session, steps, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := env.proposals.SetStatus(ctx, p.ID, "executed"); err != nil {
			t.Fatal(err)
		}
		if status, _ := env.send("go on"); status != http.StatusOK {
			t.Fatalf("turn: %d", status)
		}
	}
	// A plan of dashboards and roles changes no value: no check after it.
	run("create_dashboard", "add_dashboard_widget", "arrange_dashboard", "set_role_dashboards", "create_business_role")
	if n := notes(); n != 1 {
		t.Fatalf("a dashboards-only plan was followed by a value check (%d notes):\n%s", n, note)
	}
	// A plan that can change values is checked; a sheet matching as before
	// is named, not shown again with its hints.
	run("update_metric")
	if n := notes(); n != 2 || !strings.Contains(note, "4 cell(s) — all match") || !strings.Contains(note, "Unchanged since the last check, still matching: Actuals.") ||
		strings.Contains(note, `column D "FY"`) {
		t.Errorf("after a value plan (%d notes):\n%s", n, note)
	}
}

// assertDeveloperNote: a server check in the chat speaks to the developer —
// it says what comes next and names no tool or instruction of the model's.
func assertDeveloperNote(t *testing.T, note string) {
	t.Helper()
	if !strings.Contains(note, "Next: the AI") && !strings.Contains(note, "Nothing to fix") {
		t.Errorf("the check says nothing of what comes next:\n%s", note)
	}
	for _, not := range []string{"update_task_plan", "check_sheet", "compare_with_sheet", "your task plan", "mark it done", "Fix what differs before going on"} {
		if strings.Contains(note, not) {
			t.Errorf("the developer's note carries the model's instruction %q:\n%s", not, note)
		}
	}
}

// The check's last line says what the AI does next — the developer's next
// step under "all match" was missing (owner, 2026-11-16) — and the stage
// instruction goes to the model only.
func TestCheckNextStep(t *testing.T) {
	plan := &aiassistant.TaskPlan{Stages: []aiassistant.TaskStage{
		{Title: "Accounts", Status: aiassistant.StageDone}, {Title: "Release controls", Status: "in_progress"}, {Title: "Reports", Status: "pending"}}}
	for _, c := range []struct {
		name                      string
		plan                      *aiassistant.TaskPlan
		handover, fix, uncompared bool
		want, wantIns             string
	}{
		{"all match, a stage current", plan, false, false, false, "Nothing to fix. Next: the AI goes on with stage 2 of 3, Release controls.", "mark it done with update_task_plan"},
		{"differences", plan, false, true, false, "Next: the AI fixes what differs, then goes on with stage 2 of 3, Release controls.", "stays current until"},
		{"not yet compared", plan, false, false, true, "Next: the AI compares what was not yet compared, then goes on with stage 2 of 3, Release controls.", "stays current until"},
		{"both", plan, false, true, true, "Next: the AI fixes what differs and compares what was not yet compared, then goes on with stage 2 of 3", "stays current until"},
		{"handover, all match", plan, true, false, false, "Nothing to fix: the AI goes on from where the earlier sessions stopped.", ""},
		{"handover, differences", plan, true, true, false, "Next: the AI fixes what differs, then goes on from where the earlier sessions stopped.", ""},
		{"no plan, all match", nil, false, false, false, "Nothing to fix.", ""},
		{"no plan, differences", nil, false, true, false, "Next: the AI fixes what differs.", ""},
	} {
		line, ins := checkNextStep(c.plan, c.handover, c.fix, c.uncompared)
		if !strings.HasPrefix(line, c.want) {
			t.Errorf("%s: %q, want %q", c.name, line, c.want)
		}
		assertDeveloperNote(t, line)
		if got := strings.Join(ins, " "); (c.wantIns == "") != (got == "") || !strings.Contains(got, c.wantIns) {
			t.Errorf("%s: instructions %q, want %q", c.name, got, c.wantIns)
		}
	}
}

func TestStepChangesValues(t *testing.T) {
	for tool, want := range map[string]bool{"create_dashboard": false, "add_dashboard_widget": false, "set_role_dashboards": false,
		"create_business_role": false, "publish_workflow_def": false, "reorder_grid_metrics": false, "create_automation_rule": false,
		"update_metric": true, "write_input_values": true, "import_file_data": true, "add_grid_metric": true, "a_tool_added_later": true} {
		if got := aiassistant.StepChangesValues(tool); got != want {
			t.Errorf("StepChangesValues(%s) = %v, want %v", tool, got, want)
		}
	}
}
