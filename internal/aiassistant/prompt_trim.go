package aiassistant

// What a turn building one sheet of a target model is sent (owner,
// 2026-11-12, "send less with every call"): every call of the High-Tech
// rebuild re-sent ~110k tokens — 120k characters of instructions, most for
// workflows, connectors, exports and pages; the 80-stage plan whole (50k);
// the workbook's text (60k); every tool. A sheet turn keeps the instructions
// and tools that build metrics, grids and formulas and load inputs.

import (
	"strings"

	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// sheetDropSections are the system prompt's sections ("## <name>…") a sheet
// turn never uses.
var sheetDropSections = []string{
	"Workflow rule", "Automation rule", "Tables of records", "Table integration", "Data export",
	"REST API connectors", "Explaining the platform", "The pages people use",
}

// sheetDropExamples are the worked examples of the propose_actions section
// a sheet turn never uses (each starts `Example — developer says "<this>`).
var sheetDropExamples = []string{"build a dashboard", "add a Finance Review approval", "restrict user"}

// TrimPromptForSheet is the system prompt without the sections and worked
// examples a turn building one sheet never uses.
func TrimPromptForSheet(p string) string {
	parts := strings.Split(p, "\n## ")
	out := []string{parts[0]}
	for _, s := range parts[1:] {
		drop := false
		for _, d := range sheetDropSections {
			if strings.HasPrefix(s, d) {
				drop = true
				break
			}
		}
		if drop {
			continue
		}
		if strings.HasPrefix(s, "propose_actions format") {
			s = dropExamples(s)
		}
		out = append(out, s)
	}
	return strings.Join(out, "\n## ") +
		"\n\n(This turn builds one sheet: instructions for workflows, connectors, exports, tables of records and pages are left out; read_manual has them.)\n"
}

func dropExamples(s string) string {
	blocks := strings.Split(s, "\n\nExample — developer says \"")
	out := []string{blocks[0]}
	for _, b := range blocks[1:] {
		keep := true
		for _, d := range sheetDropExamples {
			if strings.HasPrefix(b, d) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, b)
			continue
		}
		// A dropped example runs to the next blank-line paragraph that is
		// not part of it: keep what follows its last line (the cross-step
		// ID rule after the last example).
		if i := strings.Index(b, "\n\nCross-step ID rule"); i >= 0 {
			out[len(out)-1] += b[i:]
		}
	}
	return strings.Join(out, "\n\nExample — developer says \"")
}

// sheetTools are the tools a sheet turn keeps.
var sheetTools = map[string]bool{
	"analyse_workbook": true, "check_grid_completeness": true, "compare_with_sheet": true, "get_model_summary": true,
	"list_dimensions": true, "list_grids": true, "list_metrics": true, "list_revisions": true, "load_sheet": true,
	"plan_sheet": true, "prepare_converted_file": true, "preview_file_import": true, "propose_actions": true,
	"read_attached_sheet": true, "read_manual": true, "validate_formulas": true, "workbook_map": true,
	PlanTaskTool: true, UpdateTaskPlanTool: true, SetTargetModelTool: true,
}

// SheetStageTools keeps, of tools, those a sheet turn uses.
func SheetStageTools(tools []providers.ToolDef) []providers.ToolDef {
	out := make([]providers.ToolDef, 0, len(tools))
	for _, t := range tools {
		if sheetTools[t.Name] {
			out = append(out, t)
		}
	}
	return out
}
