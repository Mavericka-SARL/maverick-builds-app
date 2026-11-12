package gateway

// analyse_workbook and load_sheet on the gateway's side: the attached file,
// the working revision's metrics and dimensions, the converted file saved
// for the import, and a dry run of that import (aiassistant/load_sheet.go,
// aiassistant/workbook_analysis.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

func (h *handler) aiAnalyseWorkbook(ctx context.Context, sessionID, file string, sheets []string) (string, error) {
	doc, raw, err := aiassistant.NewDocumentStore(h.db.For(ctx)).GetRaw(ctx, sessionID, strings.TrimSpace(file))
	if err != nil {
		return "", fmt.Errorf("no file named %q is attached to this chat", file)
	}
	name := strings.ToLower(doc.Filename)
	if raw == nil || (!strings.HasSuffix(name, ".xlsx") && !strings.HasSuffix(name, ".xlsm")) {
		return "", fmt.Errorf("%s is not a workbook kept whole (.xlsx/.xlsm)", doc.Filename)
	}
	return aiassistant.AnalyseWorkbook(raw, sheets)
}

// loadInventory is the revision's metrics (with their grid) and dimensions
// (with their leaf members) for matching a sheet.
func (h *handler) loadInventory(ctx context.Context, revID string) (aiassistant.LoadInventory, error) {
	q := h.db.For(ctx)
	inv := aiassistant.LoadInventory{GridDims: map[string][]string{}}
	rows, err := q.Query(ctx, `
		SELECT m.name, COALESCE(m.label, ''), COALESCE(m.format, ''), COALESCE(g.name, ''), COALESCE(g.id::text, '')
		FROM model.metric_def m
		LEFT JOIN model.grid_metric gm ON gm.metric_id = m.id
		LEFT JOIN model.grid_def g ON g.id = gm.grid_id
		WHERE m.revision_id = $1::uuid`, revID)
	if err != nil {
		return inv, err
	}
	for rows.Next() {
		var m aiassistant.LoadMetric
		if rows.Scan(&m.Name, &m.Label, &m.Format, &m.Grid, &m.GridID) == nil {
			inv.Metrics = append(inv.Metrics, m)
		}
	}
	rows.Close()
	rows, err = q.Query(ctx, `
		SELECT g.id::text, d.name FROM model.grid_def g
		JOIN model.grid_dimension gd ON gd.grid_id = g.id
		JOIN model.dimension_def d ON d.id = gd.dimension_id
		WHERE g.revision_id = $1::uuid`, revID)
	if err != nil {
		return inv, err
	}
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil {
			inv.GridDims[id] = append(inv.GridDims[id], name)
		}
	}
	rows.Close()
	rows, err = q.Query(ctx, `
		SELECT d.name, COALESCE(d.dimension_type, '') = 'time', mb.code, COALESCE(mb.label, ''), COALESCE(mb.period_start::text, '')
		FROM model.dimension_def d
		JOIN model.dimension_member mb ON mb.dimension_id = d.id
		WHERE d.revision_id = $1::uuid
		  AND NOT EXISTS (SELECT 1 FROM model.dimension_member c WHERE c.parent_member_id = mb.id)
		ORDER BY d.name, mb.sort_order, mb.code`, revID)
	if err != nil {
		return inv, err
	}
	byDim := map[string]int{}
	for rows.Next() {
		var dim, code, label, start string
		var isTime bool
		if rows.Scan(&dim, &isTime, &code, &label, &start) != nil {
			continue
		}
		i, ok := byDim[dim]
		if !ok {
			i = len(inv.Dimensions)
			byDim[dim] = i
			inv.Dimensions = append(inv.Dimensions, aiassistant.LoadDimension{Name: dim, Time: isTime})
		}
		inv.Dimensions[i].Members = append(inv.Dimensions[i].Members, aiassistant.LoadMember{Code: code, Label: label, Start: start})
	}
	rows.Close()
	return inv, nil
}

func (h *handler) aiLoadSheet(ctx context.Context, sessionID, userID, file, sheet, grid string, headerRow int) (string, error) {
	store := aiassistant.NewDocumentStore(h.db.For(ctx))
	doc, raw, err := store.GetRaw(ctx, sessionID, strings.TrimSpace(file))
	if err != nil || raw == nil {
		return "", fmt.Errorf("no workbook named %q is attached to this chat, kept whole", file)
	}
	sess, err := h.aiChatStore(ctx).GetSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	revID := sess.DraftRevisionID
	if revID == "" {
		return "", fmt.Errorf("load_sheet reads the AI draft: confirm a first plan so the session has one")
	}
	cells, err := importpkg.ReadGrid(doc.Filename, raw, sheet, ',')
	if err != nil {
		return "", err
	}
	inv, err := h.loadInventory(ctx, revID)
	if err != nil {
		return "", err
	}
	sl, err := aiassistant.PlanSheetLoad(cells, inv, headerRow, grid)
	if err != nil {
		return "", err
	}
	convName := sheet + " (loaded)"
	if len(sl.Unmatched()) == 0 {
		if _, err := aiassistant.NewConversionStore(h.db.For(ctx)).Save(ctx, aiassistant.Conversion{
			SessionID: sessionID, DocumentID: doc.ID, Sheet: sheet, Reshape: sl.Reshape, ColumnMap: sl.ColumnMap,
			Filename: convName, RowCount: len(sl.Rows) * len(sl.Columns), Columns: []string{"metric", sl.Dimension, "value"},
		}); err != nil {
			return "", fmt.Errorf("save the converted file: %w", err)
		}
	}
	out := sl.Describe(sheet, convName)
	if len(sl.Unmatched()) == 0 && sl.GridID != "" {
		req := aiassistant.FileImportRequest{RevisionID: revID, File: convName, TargetType: "grid", TargetID: sl.GridID, PercentFractions: sl.Percent}
		if dry, dErr := h.aiPreviewFileImport(ctx, sessionID, userID, req); dErr != nil {
			out += "Dry run of that import FAILS: " + dErr.Error() + "\n"
		} else {
			out += "Dry run of that import: " + strings.TrimSpace(dry) + "\n"
		}
	}
	return out, nil
}

// recordLoadedRows writes the rows a sheet import loaded into the workbook
// map as matching: the import wrote exactly those cells. So an input sheet's
// stage is proven by the map like a calculated one (closeProvenStages). Only
// a wide import whose labels map to metrics (load_sheet's) is recorded.
func (h *handler) recordLoadedRows(ctx context.Context, modelID, revID string, f *attachedFile, req aiassistant.FileImportRequest) {
	rs := req.Reshape
	if rs == nil || rs.Unpivot == nil || len(rs.Unpivot.Columns) == 0 || rs.HeaderRow < 1 || rs.HeaderRow > len(f.grid) {
		return
	}
	labelCol := ""
	for c, field := range req.ColumnMap {
		if field == "metric" {
			labelCol = c
		}
	}
	labels := map[string]string{}
	for k, v := range rs.ValueMap[labelCol] {
		labels[strings.ToLower(strings.TrimSpace(k))] = v
	}
	if labelCol == "" || len(labels) == 0 {
		return
	}
	header := f.grid[rs.HeaderRow-1]
	li, first := -1, -1
	var vcols []int
	for c, v := range header {
		v = strings.TrimSpace(v)
		if strings.EqualFold(v, labelCol) && li < 0 {
			li = c
		}
		for _, u := range rs.Unpivot.Columns {
			if strings.EqualFold(v, u) {
				vcols = append(vcols, c)
				if first < 0 {
					first = c
				}
			}
		}
	}
	if li < 0 || first < 0 {
		return
	}
	firstCol, _ := columnLetter(first + 1)
	for r := rs.HeaderRow + 1; r <= len(f.grid); r++ {
		row := f.grid[r-1]
		if li >= len(row) {
			continue
		}
		label := strings.TrimSpace(row[li])
		metric := labels[strings.ToLower(label)]
		if metric == "" {
			continue
		}
		n := 0
		for _, c := range vcols {
			if c < len(row) && strings.TrimSpace(row[c]) != "" {
				n++
			}
		}
		if n == 0 {
			continue
		}
		aiassistant.RecordWorkbookRow(ctx, h.db.For(ctx), modelID, revID, aiassistant.MapRow{
			File: f.filename, Sheet: f.sheet, Row: r, Label: label, Metric: metric,
			Members: map[string]string{loadedMember: "1"}, FirstColumn: firstCol, Matched: n, Compared: n,
		})
	}
}

// loadedMember marks map rows a sheet load recorded: the after-confirm
// compare (along months) skips them.
const loadedMember = "@loaded"

func columnLetter(n int) (string, error) {
	s := ""
	for n > 0 {
		n--
		s = string(rune('A'+n%26)) + s
		n /= 26
	}
	return s, nil
}

// labelImportSources says on the plan card where each imported file comes
// from (owner, 2026-11-12: the card showed an import of numbers the AI had
// typed as if they were the workbook's): the developer's own file, a sheet
// of it read by the server, or a file the assistant wrote.
func (h *handler) labelImportSources(ctx context.Context, sessionID string, steps []aiassistant.ProposalStep) {
	var typed map[string]bool
	for i := range steps {
		if steps[i].Tool != "import_file_data" {
			continue
		}
		var p struct {
			File  string `json:"file"`
			Sheet string `json:"sheet"`
		}
		if json.Unmarshal(steps[i].Params, &p) != nil || strings.TrimSpace(p.File) == "" {
			continue
		}
		if typed == nil {
			typed = h.aiTypedFiles(ctx, sessionID)
			if typed == nil {
				typed = map[string]bool{}
			}
		}
		name := strings.TrimSpace(p.File)
		src := ""
		switch {
		case typed[strings.ToLower(name)]:
			src = "WRITTEN BY THE ASSISTANT — its values were typed, not read from your file"
		default:
			if c, ok := h.convertedFileNamed(ctx, sessionID, name); ok {
				var orig string
				_ = h.db.QueryRow(ctx, `SELECT filename FROM ai_assistant.document WHERE id = $1::uuid`, c.DocumentID).Scan(&orig)
				src = fmt.Sprintf("your file %s, sheet %s, read by the server", orig, c.Sheet)
			} else if p.Sheet != "" {
				src = fmt.Sprintf("your file %s, sheet %s", name, p.Sheet)
			} else {
				src = "your file " + name
			}
		}
		if !strings.Contains(steps[i].Description, "Source: ") {
			steps[i].Description = strings.TrimSpace(steps[i].Description) + " — Source: " + src
		}
	}
}

// buildDocumentIndex lists the session's attached files for a turn once the
// target model holds what the workbook says: each workbook by its sheets and
// their sizes, with the tools that read one — not the workbook's text, 60k
// characters re-sent with every call on 2026-11-12. Other documents keep
// their text, clipped.
func buildDocumentIndex(docs []aiassistant.Document, workbooks map[string][]byte) string {
	if len(docs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n## Attached documents\n")
	sb.WriteString("Read a sheet when you need it: analyse_workbook (every sheet\x27s role and rows), read_attached_sheet (rows and whole formulas), plan_sheet (a sheet\x27s plan, formulas translated), load_sheet (inputs).\n")
	for _, d := range docs {
		raw, ok := workbooks[d.Filename]
		if !ok {
			content := d.Content
			if len(content) > 8000 {
				content = content[:8000] + "\n(… clipped)"
			}
			fmt.Fprintf(&sb, "\n### %s\n%s\n", d.Filename, content)
			continue
		}
		f, err := excelize.OpenReader(bytes.NewReader(raw))
		if err != nil {
			fmt.Fprintf(&sb, "\n### %s (workbook)\n", d.Filename)
			continue
		}
		var sheets []string
		for _, s := range f.GetSheetList() {
			dim, _ := f.GetSheetDimension(s)
			sheets = append(sheets, fmt.Sprintf("%s (%s)", s, dim))
		}
		_ = f.Close()
		fmt.Fprintf(&sb, "\n### %s (workbook kept whole)\nSheets: %s\n", d.Filename, strings.Join(sheets, ", "))
	}
	return sb.String()
}
