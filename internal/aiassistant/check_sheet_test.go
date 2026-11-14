package aiassistant_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// checkSheetModel is a small OLAP model: expense by period (FY2026 over
// three months), cost_center (DIV over CC1, CC2), account and version, with
// actuals for every leaf — CC2/A2 February deliberately 6 where the sheet
// says 5.
func checkSheetModel(t *testing.T) (*aiassistant.ToolExecutor, func(name string, data []byte), context.Context) {
	t.Helper()
	pool := setupWriteExecutorDB(t)
	ctx := context.Background()
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Working")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	period := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type, time_granularity, fiscal_year_start_month)
		VALUES ($1::uuid, $2::uuid, 'period', 'time', 'month', 1) RETURNING id::text`, modelID, revID)
	fy := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'FY2026', 'FY 2026') RETURNING id::text`, period)
	for i, code := range []string{"2026-01", "2026-02", "2026-03"} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, period_start, period_end, time_index)
			VALUES ($1::uuid, $2, $2, $3::uuid, ($2 || '-01')::date, (($2 || '-01')::date + interval '1 month' - interval '1 day')::date, $4) RETURNING id::text`,
			period, code, fy, i)
	}
	cc := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'cost_center') RETURNING id::text`, modelID, revID)
	div := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'DIV', 'Division') RETURNING id::text`, cc)
	for _, c := range [][2]string{{"CC1", "Cost centre one"}, {"CC2", "Cost centre two"}} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id) VALUES ($1::uuid, $2, $3, $4::uuid) RETURNING id::text`, cc, c[0], c[1], div)
	}
	acct := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'account') RETURNING id::text`, modelID, revID)
	for _, c := range []string{"A1", "A2"} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, $2, $2) RETURNING id::text`, acct, c)
	}
	ver := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'version') RETURNING id::text`, modelID, revID)
	for _, c := range []string{"ACTUAL", "BUDGET"} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, $2, $2) RETURNING id::text`, ver, c)
	}
	metric := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule, time_summary, format)
		VALUES ($1::uuid, $2::uuid, 'expense', true, 'sum', 'sum', 'number') RETURNING id::text`, modelID, revID)
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Expenses') RETURNING id::text`, modelID, revID)
	for _, d := range []string{period, cc, acct, ver} {
		q(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid) RETURNING grid_id::text`, grid, d)
	}
	q(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid) RETURNING grid_id::text`, grid, metric)
	facts := map[[2]string][3]float64{
		{"CC1", "A1"}: {10, 20, 30}, {"CC1", "A2"}: {1, 2, 3},
		{"CC2", "A1"}: {100, 200, 300}, {"CC2", "A2"}: {5, 6, 5}, // the sheet says 5 in February
	}
	for k, vals := range facts {
		for i, code := range []string{"2026-01", "2026-02", "2026-03"} {
			q(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
				VALUES ($1::uuid, $2::uuid, $3::uuid, jsonb_build_object($4::text, $5::text, $6::text, $7::text, $8::text, $9::text, $10::text, 'ACTUAL'), $11)
				RETURNING id::text`, modelID, revID, metric, period, code, cc, k[0], acct, k[1], ver, vals[i])
		}
	}
	files := map[string][]byte{}
	e := aiassistant.NewToolExecutor(pool, modelID, revID).WithReadHooks(aiassistant.ReadHooks{
		AttachedFile: func(_ context.Context, name string) (string, []byte, error) {
			for n, d := range files {
				if strings.EqualFold(n, name) {
					return n, d, nil
				}
			}
			return "", nil, context.Canceled
		},
	})
	return e, func(name string, data []byte) { files[name] = data }, ctx
}

// actualsBook is a sheet laid out for people: a title, dates across, an FY
// total column, a Δ column, a division subtotal and a TOTAL row.
func actualsBook(t *testing.T) []byte {
	t.Helper()
	f := excelize.NewFile()
	sh := "Actuals"
	_ = f.SetSheetName("Sheet1", sh)
	set := func(cell string, v any) {
		if err := f.SetCellValue(sh, cell, v); err != nil {
			t.Fatal(err)
		}
	}
	set("A1", "ACTUALS 2026 — input template")
	for i, h := range []any{"Line", "Cost center", "Account"} {
		cell, _ := excelize.CoordinatesToCellName(i+1, 3)
		set(cell, h)
	}
	for i := 0; i < 3; i++ {
		cell, _ := excelize.CoordinatesToCellName(4+i, 3)
		set(cell, time.Date(2026, time.Month(i+1), 1, 0, 0, 0, 0, time.UTC))
	}
	set("G3", "FY total")
	set("H3", "Δ %")
	rows := [][]any{
		{"Salaries", "CC1", "A1", 10, 20, 30, 60, 0.1},
		{"Travel", "Cost centre one", "A2", 1, 2, 3, 6, 0.2}, // a cost centre by its label
		{"Salaries", "CC2", "A1", 100, 200, 300, 600, 0.3},
		{"Travel", "CC2", "A2", 5, 5, 5, 15, 0.4},
		{"Division subtotal", "DIV", "", 116, 227, 338, 681, 0.5}, // a parent member, every account
		{},
		{"TOTAL", "", "", 116, 227, 338, 681, 0.6}, // every cost centre and account
	}
	for r, vals := range rows {
		for c, v := range vals {
			cell, _ := excelize.CoordinatesToCellName(c+1, r+4)
			set(cell, v)
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func actualsMap(version string, valueMap importpkg.ValueMaps, to string) aiassistant.SheetMap {
	return aiassistant.SheetMap{
		Reshape: &importpkg.Reshape{HeaderRow: 3,
			Unpivot:   &importpkg.Unpivot{From: "2026-01-01", To: to, NameColumn: "period", ValueColumn: "expense"},
			Constants: map[string]string{"version": version}, ValueMap: valueMap},
		ColumnMap: map[string]string{"*": "ignore", "Cost center": "cost_center", "Account": "account", "period": "period", "expense": "expense", "version": "version"},
	}
}

// A sheet compared cell by cell as model coordinates (2026-11-14): months by
// their header dates, the FY column at the model's FY total, a parent member
// and blank keys at the model's totals; one value problem, nothing else.
func TestCheckSheetReadsCellsAsModelCoordinates(t *testing.T) {
	e, attach, ctx := checkSheetModel(t)
	book := actualsBook(t)
	attach("actuals.xlsx", book)

	res, err := e.CheckSheet(ctx, "actuals.xlsx", book, "Actuals", actualsMap("ACTUAL", importpkg.ValueMaps{"period": {"FY total": "FY2026"}}, "FY total"))
	if err != nil {
		t.Fatal(err)
	}
	// 4 leaf rows, the subtotal and the total row × 3 months + FY = 24.
	if res.Compared != 24 {
		t.Fatalf("compared %d cells, want 24:\n%s", res.Compared, res.Render())
	}
	// CC2/A2 February differs, and every total it is in: its FY, the
	// division's February and FY, the grand total's February and FY.
	if res.Matched != 18 || len(res.MappingProblems) != 0 {
		t.Fatalf("matched %d of 24, mapping problems %v, want 18 and none:\n%s", res.Matched, res.MappingProblems, res.Render())
	}
	out := res.Render()
	for _, want := range []string{`row 7 "Travel"`, "1 of 3 cells differ", "2026-02-01: sheet 5, model 6", "5 total cell(s)", "Not in this block", `column H "Δ %"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the check lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `row 8 `) || strings.Contains(out, `row 10 `) {
		t.Errorf("the subtotal and TOTAL rows differ only through row 7 and should not be listed:\n%s", out)
	}
	if !strings.Contains(out, "input cells in the sheet") {
		t.Errorf("a value problem in input cells should point at the load:\n%s", out)
	}

	// Without the FY column the map leaves it out, and says it is the total
	// of the months beside it.
	months, err := e.CheckSheet(ctx, "actuals.xlsx", book, "Actuals", actualsMap("ACTUAL", nil, "2026-03-01"))
	if err != nil {
		t.Fatal(err)
	}
	if months.Compared != 18 || !strings.Contains(months.Render(), `column G "FY total" (6 numbers) — beside it are the periods of FY2026`) {
		t.Errorf("months only: compared %d, want 18, and the FY column named as FY2026:\n%s", months.Compared, months.Render())
	}
}

// A wrong constant moves every cell: nothing matches, and the check says to
// look at the map before the data.
func TestCheckSheetWrongVersionIsAMapProblem(t *testing.T) {
	e, _, ctx := checkSheetModel(t)
	book := actualsBook(t)
	res, err := e.CheckSheet(ctx, "actuals.xlsx", book, "Actuals", actualsMap("BUDGET", nil, "2026-03-01"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 0 || !strings.Contains(res.Overall, "Nothing matches") || !strings.Contains(res.Overall, "no value") {
		t.Errorf("a map onto BUDGET: matched %d, overall %q", res.Matched, res.Overall)
	}
}

// A long file (one value per row, the metric named in a column) with a
// member the model does not have: a mapping problem, the rest compared.
func TestCheckSheetLongFileAndUnknownMember(t *testing.T) {
	e, _, ctx := checkSheetModel(t)
	csv := "metric,cost centre,account,month,amount\n" +
		"expense,CC1,A1,2026-01,10\n" +
		"expense,CC1,A9,2026-01,7\n" +
		"expense,DIV,,FY2026,681\n"
	res, err := e.CheckSheet(ctx, "long.csv", []byte(csv), "", aiassistant.SheetMap{
		Reshape:   &importpkg.Reshape{Constants: map[string]string{"version": "ACTUAL"}},
		ColumnMap: map[string]string{"metric": "metric", "cost centre": "cost_center", "account": "account", "month": "period", "amount": "value", "version": "version"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compared != 2 || res.Matched != 1 {
		t.Errorf("compared %d matched %d, want 2 and 1 (the division's FY total is 681 in the sheet, 682 in the model):\n%s", res.Compared, res.Matched, res.Render())
	}
	if len(res.MappingProblems) != 1 || !strings.Contains(res.MappingProblems[0], `"A9" (1 cells) is not a member of account`) {
		t.Errorf("mapping problems = %v", res.MappingProblems)
	}
}

// check_sheet saves the map it was given, replays it with file and sheet
// only, and two blocks of one sheet list together what neither reads.
func TestCheckSheetToolSavesAndReplaysBlocks(t *testing.T) {
	e, attach, ctx := checkSheetModel(t)
	attach("actuals.xlsx", actualsBook(t))
	months := actualsMap("ACTUAL", nil, "2026-03-01")
	fy := actualsMap("ACTUAL", importpkg.ValueMaps{"period": {"FY total": "FY2026"}}, "")
	fy.Reshape.Unpivot = &importpkg.Unpivot{Columns: []string{"FY total"}, NameColumn: "period", ValueColumn: "expense"}
	for block, m := range map[string]aiassistant.SheetMap{"months": months, "FY": fy} {
		out, err := e.Execute(ctx, "check_sheet", mustJSON(t, map[string]any{"file": "actuals.xlsx", "sheet": "Actuals", "block": block,
			"map": map[string]any{"reshape": m.Reshape, "column_map": m.ColumnMap}}))
		if err != nil || !strings.Contains(out, "is saved as block") {
			t.Fatalf("check_sheet %s: %v\n%s", block, err, out)
		}
	}
	out, err := e.Execute(ctx, "check_sheet", mustJSON(t, map[string]any{"file": "actuals.xlsx", "sheet": "Actuals"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `block "FY": 6 cell(s) compared`) || !strings.Contains(out, `block "months": 18 cell(s) compared`) {
		t.Errorf("replay of both blocks:\n%s", out)
	}
	if strings.Contains(out, "FY total") && strings.Contains(out, `column G "FY total"`) {
		t.Errorf("the FY column is read by block FY and still listed as not read:\n%s", out)
	}
	if !strings.Contains(out, `column H "Δ %"`) {
		t.Errorf("the Δ column, read by neither block, is not listed:\n%s", out)
	}
	if _, err := e.Execute(ctx, "check_sheet", mustJSON(t, map[string]any{"file": "actuals.xlsx", "sheet": "Actuals", "block": "FY", "forget": true})); err != nil {
		t.Fatal(err)
	}
	if out, _ := e.Execute(ctx, "check_sheet", mustJSON(t, map[string]any{"file": "actuals.xlsx", "sheet": "Actuals"})); strings.Contains(out, `block "FY"`) {
		t.Errorf("a forgotten block is still checked:\n%s", out)
	}
}

// compare_with_sheet reads each column's period from the header above the
// row: a sheet starting in February is read from February, and the row ends
// at the first column whose header is no period (2026-11-14).
func TestCompareWithSheetReadsPeriodsFromTheHeader(t *testing.T) {
	e, attach, ctx := checkSheetModel(t)
	attach("feb.csv", []byte("Feb to Mar\nLine,Cost center,2026-02,2026-03,FY\nSalaries,CC1,20,30,60\n"))
	out, err := e.Execute(ctx, "compare_with_sheet", mustJSON(t, map[string]any{"file": "feb.csv", "row": 3, "first_column": "C", "metric": "expense",
		"members": map[string]string{"cost_center": "CC1", "account": "A1", "version": "ACTUAL"}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 of 2 periods match") {
		t.Errorf("a row under a February header: %s", out)
	}
}

// The map an executed import loaded a sheet with becomes the sheet's map,
// from the import's own params or the loader created in the same plan.
func TestLoadMapsOfImports(t *testing.T) {
	steps := []aiassistant.ProposalStep{
		{Tool: "create_file_integration", Params: []byte(`{"name":"Load actuals","sheet":"Actuals","target_type":"grid","target_id":"Expenses",
			"reshape":{"header_row":3},"column_map":{"Cost center":"cost_center"}}`), Status: "success"},
		{Tool: "import_file_data", Params: []byte(`{"file":"actuals.xlsx","sheet":"Actuals","integration_id":"<created in step 1>"}`), Status: "success"},
		{Tool: "import_file_data", Params: []byte(`{"file":"actuals.xlsx","sheet":"Rates","target_type":"grid","target_id":"Rates","column_map":{"Rate":"rate"}}`), Status: "success"},
		{Tool: "import_file_data", Params: []byte(`{"file":"actuals.xlsx","sheet":"Staff","target_type":"form","target_id":"Staff","column_map":{"Name":"name"}}`), Status: "success"},
		{Tool: "import_file_data", Params: []byte(`{"file":"actuals.xlsx","sheet":"Failed","column_map":{"x":"y"}}`), Status: "failed"},
	}
	got := aiassistant.LoadMapsOf(steps)
	if len(got) != 2 || got[0].Sheet != "Actuals" || got[0].Map.Block != "loaded into Expenses from row 3" || got[0].Map.ColumnMap["Cost center"] != "cost_center" ||
		got[1].Sheet != "Rates" || got[1].Map.Source != "load" {
		t.Errorf("load maps = %+v", got)
	}
}

// A block whose value columns hold text (an owner, a status) has nothing to
// compare by value: said once, never "0 cells compared".
func TestCheckSheetTextOnlyBlock(t *testing.T) {
	e, _, ctx := checkSheetModel(t)
	res, err := e.CheckSheet(ctx, "owners.csv", []byte("account,expense\nA1,Anna\nA2,Ben\n"), "", aiassistant.SheetMap{
		ColumnMap: map[string]string{"account": "account", "expense": "expense"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TextOnly || !strings.Contains(res.Render(), "hold text, not numbers") {
		t.Errorf("text block: %+v\n%s", res.TextOnly, res.Render())
	}
}
