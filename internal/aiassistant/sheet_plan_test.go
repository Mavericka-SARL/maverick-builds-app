package aiassistant

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// A small workbook: a driver sheet and a monthly sheet whose formulas read
// the same period, the period before, a running total, the month number and
// another sheet; the server translates every row.
func TestPlanSheetTranslates(t *testing.T) {
	f := excelize.NewFile()
	_ = f.SetSheetName("Sheet1", "Drivers")
	_ = f.SetCellValue("Drivers", "A2", "Price per unit")
	_ = f.SetCellValue("Drivers", "B2", 10)
	_ = f.SetCellValue("Drivers", "A3", "Growth %")
	_ = f.SetCellValue("Drivers", "B3", 0.05)
	_, _ = f.NewSheet("Plan")
	_ = f.SetCellValue("Plan", "A1", "Period")
	for i, m := range []string{"Jan-27", "Feb-27", "Mar-27"} {
		col, _ := excelize.ColumnNumberToName(i + 2)
		_ = f.SetCellValue("Plan", col+"1", m)
		_ = f.SetCellValue("Plan", col+"2", i+1)
		_ = f.SetCellValue("Plan", col+"4", 100+i)
		prev, _ := excelize.ColumnNumberToName(i + 1)
		if i == 0 {
			_ = f.SetCellFormula("Plan", col+"5", "B4")
		} else {
			_ = f.SetCellFormula("Plan", col+"5", prev+"5*(1+Drivers!$B$3)")
		}
		_ = f.SetCellFormula("Plan", col+"6", col+"5*Drivers!$B$2")
		_ = f.SetCellFormula("Plan", col+"7", "SUM($B6:"+col+"6)")
		_ = f.SetCellFormula("Plan", col+"8", "IF("+col+"$2>=2,"+col+"6,0)")
	}
	_ = f.SetCellValue("Plan", "A2", "Month #")
	_ = f.SetCellValue("Plan", "A3", "VOLUME")
	_ = f.SetCellValue("Plan", "A4", "Base units")
	_ = f.SetCellValue("Plan", "A5", "Units")
	_ = f.SetCellValue("Plan", "A6", "Revenue")
	_ = f.SetCellValue("Plan", "A7", "Cumulative revenue")
	_ = f.SetCellValue("Plan", "A8", "Revenue after launch")
	var buf bytes.Buffer
	_ = f.Write(&buf)
	target := &TargetModel{Summary: "x", Sheets: []TargetSheet{
		{Sheet: "Drivers", Role: "input", Grid: "Drivers", Metrics: []string{"price_per_unit", "growth_pct"}},
		{Sheet: "Plan", Role: "calculation", Grid: "Plan", Dimensions: []string{"month"}, Metrics: []string{"base_units", "units", "revenue", "cumulative_revenue", "revenue_after_launch"}},
	}}
	rows, err := PlanSheet(buf.Bytes(), SheetPlanInput{Sheet: "Plan", Target: target, Percent: map[string]bool{"growth_pct": true}, FirstYear: 2027, FirstMon: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		switch {
		case r.Input:
			got[r.Metric] = "input"
		case r.Problem != "":
			got[r.Metric] = "problem: " + r.Problem
		default:
			got[r.Metric] = r.Formula
		}
	}
	want := map[string]string{
		"base_units":           "input",
		"units":                "IF(((YEAR(START()) - 2027) * 12 + MONTH(START()) - 1 + 1) = 1, base_units, LAG(units, 1, 0)*(1+(growth_pct / 100)))",
		"revenue":              "units*price_per_unit",
		"cumulative_revenue":   "SUM(CUMULATE(revenue))",
		"revenue_after_launch": "IF(((YEAR(START()) - 2027) * 12 + MONTH(START()) - 1 + 1)>=2,revenue,0)",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// A sheet turn's prompt leaves out the sections and examples it never uses,
// keeps the cross-step rule, and offers only the model-building tools.
func TestTrimPromptForSheet(t *testing.T) {
	full := BuildSystemPrompt(ModelContext{})
	trimmed := TrimPromptForSheet(full)
	for _, gone := range []string{"\n## Workflow rule", "\n## REST API connectors", "\n## The pages people use", `Example — developer says "build a dashboard`} {
		if !strings.Contains(full, gone) || strings.Contains(trimmed, gone) {
			t.Errorf("%q not dropped", gone)
		}
	}
	for _, kept := range []string{"\n## Formula language", "\n## Time dimensions", "Cross-step ID rule", "\n## propose_actions format"} {
		if !strings.Contains(trimmed, kept) {
			t.Errorf("%q dropped", kept)
		}
	}
	if len(trimmed)*10 > len(full)*7 {
		t.Errorf("trimmed %d of %d characters only", len(full)-len(trimmed), len(full))
	}
	tools := SheetStageTools(AllTools())
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
	}
	if !names["propose_actions"] || !names["plan_sheet"] || !names["read_attached_sheet"] || names["list_workflows"] || names["create_csv_file"] {
		t.Errorf("sheet tools: %v", names)
	}
}

// A long plan renders its counts, the stages around the current one and the
// current one whole; the target shows the current sheet whole.
func TestCompactPlanRender(t *testing.T) {
	var stages []TaskStage
	for i := 0; i < 30; i++ {
		stages = append(stages, TaskStage{Title: "Stage " + string(rune('A'+i%26)) + string(rune('a'+i/26)), Items: []string{"item of a long description number " + string(rune('A'+i%26))}})
	}
	p, _ := NewTaskPlan("Build", stages)
	for i := 0; i < 20; i++ {
		p.Stages[i].Status = StageDone
	}
	p.Stages[20].Sheet = "P&L"
	p.Target = &TargetModel{Summary: "s", Sheets: []TargetSheet{{Sheet: "P&L", Role: "calculation", Grid: "PL", Metrics: []string{"revenue", "cogs"}}, {Sheet: "Cash", Role: "calculation", Grid: "Cash", Metrics: []string{"cash"}}}}
	r := p.Render()
	if !strings.Contains(r, "30 stages: 20 done") || strings.Contains(r, "1. [done]") || !strings.Contains(r, "21. [to do, CURRENT]") || !strings.Contains(r, "item of a long description number U") {
		t.Fatalf("render:\n%s", r)
	}
	if !strings.Contains(r, "revenue, cogs") || strings.Contains(r, "1 metrics: cash") {
		t.Errorf("target render:\n%s", r)
	}
}
