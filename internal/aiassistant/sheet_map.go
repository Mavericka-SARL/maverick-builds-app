package aiassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// SavedSheetMap is a sheet map as kept (ai_assistant.sheet_map).
type SavedSheetMap struct {
	File, Sheet     string
	Map             SheetMap
	Compared        int
	Matched         int
	MappingProblems int
	CheckedAt       *time.Time
	Revision        string // the revision of the last check
}

// LoadBlockName names the block an import loaded: "loaded into <target>
// from row <header row>".
func LoadBlockName(target string, rs *importpkg.Reshape) string {
	name := "loaded into " + strings.TrimSpace(target)
	if rs != nil && rs.HeaderRow > 0 {
		name += fmt.Sprintf(" from row %d", rs.HeaderRow)
	}
	return name
}

// SaveSheetMap keeps a block's map. A load map never replaces a map the AI
// gave check_sheet for the same block.
func SaveSheetMap(ctx context.Context, q dbx.DB, modelID, file, sheet string, m SheetMap) error {
	if modelID == "" || strings.TrimSpace(file) == "" || len(m.ColumnMap) == 0 {
		return nil
	}
	m.Block = blockName(m.Block)
	if m.Source == "" {
		m.Source = "check"
	}
	_, err := q.Exec(ctx, `
		INSERT INTO ai_assistant.sheet_map (model_id, file_name, sheet, block, map, source, updated_at)
		VALUES ($1::uuid, $2, $3, $4, $5::jsonb, $6, now())
		ON CONFLICT (model_id, lower(file_name), lower(sheet), lower(block)) DO UPDATE
		  SET map = EXCLUDED.map, source = EXCLUDED.source, updated_at = now()
		  WHERE EXCLUDED.source = 'check' OR ai_assistant.sheet_map.source = 'load'`,
		modelID, strings.TrimSpace(file), strings.TrimSpace(sheet), m.Block, string(m.MapJSON()), m.Source)
	return err
}

// DeleteSheetMap drops one block's map.
func DeleteSheetMap(ctx context.Context, q dbx.DB, modelID, file, sheet, block string) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM ai_assistant.sheet_map WHERE model_id = $1::uuid AND lower(file_name) = lower($2)
		AND lower(sheet) = lower($3) AND lower(block) = lower($4)`, modelID, strings.TrimSpace(file), strings.TrimSpace(sheet), blockName(block))
	return tag.RowsAffected() > 0, err
}

// LoadSheetMaps is the model's maps of files (all files when none are
// named), of one sheet when sheet is set; ordered by file, sheet, block.
func LoadSheetMaps(ctx context.Context, q dbx.DB, modelID string, files []string, sheet string) ([]SavedSheetMap, error) {
	if files == nil {
		files = []string{}
	}
	rows, err := q.Query(ctx, `
		SELECT file_name, sheet, map, compared, matched, mapping_problems, checked_at, COALESCE(revision_id::text, '')
		FROM ai_assistant.sheet_map
		WHERE model_id = $1::uuid AND (cardinality($2::text[]) = 0 OR lower(file_name) = ANY($2))
		  AND ($3 = '' OR lower(sheet) = lower($3))
		ORDER BY lower(file_name), lower(sheet), source DESC, lower(block)`, modelID, lowerAll(files), strings.TrimSpace(sheet))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SavedSheetMap, error) {
		var s SavedSheetMap
		var raw []byte
		err := r.Scan(&s.File, &s.Sheet, &raw, &s.Compared, &s.Matched, &s.MappingProblems, &s.CheckedAt, &s.Revision)
		if err == nil {
			err = json.Unmarshal(raw, &s.Map)
		}
		return s, err
	})
}

// RecordSheetCheck keeps a block's last check (best-effort).
func RecordSheetCheck(ctx context.Context, q dbx.DB, modelID, revID string, c *SheetCheck) {
	if modelID == "" || c == nil {
		return
	}
	_, _ = q.Exec(ctx, `UPDATE ai_assistant.sheet_map SET revision_id = NULLIF($5,'')::uuid, compared = $6, matched = $7,
		mapping_problems = $8, checked_at = now()
		WHERE model_id = $1::uuid AND lower(file_name) = lower($2) AND lower(sheet) = lower($3) AND lower(block) = lower($4)`,
		modelID, c.File, c.Sheet, c.Block, revID, c.Compared, c.Matched, len(c.MappingProblems))
}

// LoadMapsOf are the sheet maps of executed import steps: an
// import_file_data step with its reshape and column_map, or one that ran a
// create_file_integration step of the same proposal (the step's
// integration_id names it, "<created in step 3>"). Only grid imports load
// values.
func LoadMapsOf(steps []ProposalStep) []struct {
	File, Sheet string
	Map         SheetMap
} {
	type imp struct {
		File       string             `json:"file"`
		Sheet      string             `json:"sheet"`
		Reshape    *importpkg.Reshape `json:"reshape"`
		ColumnMap  map[string]string  `json:"column_map"`
		TargetType string             `json:"target_type"`
		TargetID   string             `json:"target_id"`
		Name       string             `json:"name"`
		Integ      string             `json:"integration_id"`
	}
	var out []struct {
		File, Sheet string
		Map         SheetMap
	}
	parsed := make([]imp, len(steps))
	for i, s := range steps {
		_ = json.Unmarshal(s.Params, &parsed[i])
	}
	for i, s := range steps {
		if s.Tool != "import_file_data" || (s.Status != "" && s.Status != "success") {
			continue
		}
		p := parsed[i]
		src := p
		if len(src.ColumnMap) == 0 && p.Integ != "" {
			for j, t := range steps {
				if t.Tool == "create_file_integration" && strings.Contains(p.Integ, fmt.Sprintf("step %d", j+1)) {
					src = parsed[j]
				}
			}
			if len(src.ColumnMap) == 0 {
				for j, t := range steps {
					if t.Tool == "create_file_integration" && strings.EqualFold(parsed[j].Sheet, p.Sheet) {
						src = parsed[j]
					}
				}
			}
		}
		if len(src.ColumnMap) == 0 || p.File == "" || (src.TargetType != "" && src.TargetType != "grid") {
			continue
		}
		target := src.TargetID
		if target == "" {
			target = src.Name
		}
		out = append(out, struct {
			File, Sheet string
			Map         SheetMap
		}{p.File, firstNonBlank(p.Sheet, src.Sheet), SheetMap{Block: LoadBlockName(target, src.Reshape), Reshape: src.Reshape, ColumnMap: src.ColumnMap, Source: "load"}})
	}
	return out
}

func firstNonBlank(a ...string) string {
	for _, s := range a {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// checkSheetTool is check_sheet.
func (e *ToolExecutor) checkSheetTool(ctx context.Context, raw json.RawMessage) (string, error) {
	if e.hooks.AttachedFile == nil {
		return "", fmt.Errorf("checking an attached file is not available here")
	}
	var p struct {
		File   string    `json:"file"`
		Sheet  string    `json:"sheet"`
		Block  string    `json:"block"`
		Map    *SheetMap `json:"map"`
		Forget bool      `json:"forget"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", fmt.Errorf("invalid params: %w", err)
	}
	name, data, err := e.hooks.AttachedFile(ctx, p.File)
	if err != nil {
		return "", err
	}
	if p.Forget {
		gone, err := DeleteSheetMap(ctx, e.pool, e.modelID, name, p.Sheet, p.Block)
		if err != nil {
			return "", err
		}
		if !gone {
			return "", fmt.Errorf("no map %q is saved for %s %q", blockName(p.Block), name, p.Sheet)
		}
		return fmt.Sprintf("The map %q of %s %q is dropped.", blockName(p.Block), name, p.Sheet), nil
	}
	var maps []SheetMap
	if p.Map != nil {
		m := *p.Map
		m.Block, m.Source = blockName(firstNonBlank(p.Block, m.Block)), "check"
		maps = []SheetMap{m}
	} else {
		saved, err := LoadSheetMaps(ctx, e.pool, e.modelID, []string{name}, p.Sheet)
		if err != nil {
			return "", err
		}
		for _, s := range saved {
			if strings.TrimSpace(s.Sheet) == "" && strings.TrimSpace(p.Sheet) != "" {
				continue
			}
			if p.Block == "" || strings.EqualFold(blockName(p.Block), s.Map.Block) {
				maps = append(maps, s.Map)
			}
		}
		if len(maps) == 0 {
			return "", fmt.Errorf("no map is saved for %s %q (an executed import saves the map it loaded the sheet with): give map — "+
				"the reshape and column_map that read the sheet as the model's cells, as import_file_data takes them", name, p.Sheet)
		}
		// Saved ones may hold another sheet when sheet was omitted (CSV).
	}
	var checks []*SheetCheck
	var b strings.Builder
	for i, m := range maps {
		c, err := e.CheckSheet(ctx, name, data, p.Sheet, m)
		if err != nil {
			if p.Map != nil {
				return "", err
			}
			fmt.Fprintf(&b, "%sblock %q: not checked — %v\n", map[bool]string{true: "", false: "\n"}[i == 0], m.Block, err)
			continue
		}
		if p.Map != nil && c.Skipped() == "" {
			if err := SaveSheetMap(ctx, e.pool, e.modelID, name, p.Sheet, m); err != nil {
				return "", err
			}
		}
		checks = append(checks, c)
	}
	CombineCoverage(checks)
	for i, c := range checks {
		RecordSheetCheck(ctx, e.pool, e.modelID, e.revID, c)
		if i > 0 || b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(c.Render())
	}
	if p.Map != nil && len(checks) == 1 && checks[0].Skipped() == "" {
		fmt.Fprintf(&b, "\n(The map is saved as block %q: check_sheet with file and sheet replays it, and the server checks it again after every plan.)", checks[0].Block)
	}
	return b.String(), nil
}

// CheckSavedSheets checks every saved map of files (attached ones) in this
// revision: the server's check after a confirmed plan. A file that cannot be
// read is left out.
func (e *ToolExecutor) CheckSavedSheets(ctx context.Context, files []string) []*SheetCheck {
	if e.hooks.AttachedFile == nil || len(files) == 0 {
		return nil
	}
	saved, err := LoadSheetMaps(ctx, e.pool, e.modelID, files, "")
	if err != nil {
		return nil
	}
	type file struct {
		name string
		data []byte
	}
	read := map[string]*file{}
	var checks []*SheetCheck
	for _, s := range saved {
		f, ok := read[strings.ToLower(s.File)]
		if !ok {
			if name, data, err := e.hooks.AttachedFile(ctx, s.File); err == nil {
				f = &file{name, data}
			}
			read[strings.ToLower(s.File)] = f
		}
		if f == nil {
			continue
		}
		c, err := e.CheckSheet(ctx, f.name, f.data, s.Sheet, s.Map)
		if err != nil {
			c = &SheetCheck{File: f.name, Sheet: s.Sheet, Block: s.Map.Block, skipped: err.Error()}
		}
		prev := s
		c.prev = &prev
		checks = append(checks, c)
	}
	CombineCoverage(checks)
	for _, c := range checks {
		if c.skipped == "" {
			RecordSheetCheck(ctx, e.pool, e.modelID, e.revID, c)
		}
	}
	return checks
}

// SheetMapVerdicts judges each sheet with a saved map (lower-cased name) by
// its blocks' last checks: "" when every block was checked in revID and all
// its cells match with nothing mis-mapped, else what is open. A sheet with
// no map is absent: it is judged by its rows (the workbook map).
func SheetMapVerdicts(ctx context.Context, q dbx.DB, modelID, revID string) map[string]string {
	saved, err := LoadSheetMaps(ctx, q, modelID, nil, "")
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, s := range saved {
		k := strings.ToLower(strings.TrimSpace(s.Sheet))
		var open string
		switch {
		case s.CheckedAt == nil || (revID != "" && s.Revision != revID):
			open = fmt.Sprintf("block %q not checked in this draft yet (check_sheet)", s.Map.Block)
		case s.MappingProblems > 0:
			open = fmt.Sprintf("block %q has %d mapping problem(s) at its last check", s.Map.Block, s.MappingProblems)
		case s.Matched < s.Compared:
			open = fmt.Sprintf("block %q: %d of %d cells differ at its last check", s.Map.Block, s.Compared-s.Matched, s.Compared)
		}
		prev, seen := out[k]
		switch {
		case !seen:
			out[k] = open
		case open != "" && prev != "":
			out[k] = prev + "; " + open
		case open != "":
			out[k] = open
		}
	}
	return out
}

// StepChangesValues reports whether a plan step can change a value a sheet
// check reads. A step that only shapes how values are shown or who sees
// them — dashboards, widgets, roles, workflows, tags, order — cannot: the
// server's value check after a plan of only such steps said "2,955 cells —
// all match" again after every dashboard plan (2026-11-14). Any other step,
// a new tool included, can.
func StepChangesValues(tool string) bool {
	t := strings.ToLower(tool)
	for _, w := range []string{"dashboard", "widget", "role", "workflow", "automation", "tag", "reorder_", "arrange_"} {
		if strings.Contains(t, w) {
			return false
		}
	}
	return true
}

// ChangesValues reports whether an executed plan ran a step that can change
// values.
func (p Proposal) ChangesValues() bool {
	for _, s := range p.Steps {
		if (s.Status == "" || s.Status == "success") && StepChangesValues(s.Tool) {
			return true
		}
	}
	return false
}
