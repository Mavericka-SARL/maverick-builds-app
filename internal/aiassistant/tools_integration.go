package aiassistant

// File integrations and exports for the AI Developer: import a spreadsheet
// attached to the chat through the Import Wizard's own pipeline, save it as
// a re-runnable "csv_import" integration, and define "file_export"
// integrations that write a grid's values in a specified format.
//
// Every operation that touches data or a file runs in the gateway, reached
// through hooks (ReadHooks here, Hooks.ImportFile on the write side): the
// import and the export rendering are the developer endpoints' own code, so
// the assistant is held to exactly what they enforce — write guards, plan
// limits, access rules, recalculation and audit. This file only resolves
// the language model's references and stores definitions.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/dataexport"
	"github.com/mavericks-engine/mavericks/internal/modeledit"
)

// FileImportRequest is one import of an attached spreadsheet, its
// references already resolved into the working revision.
type FileImportRequest struct {
	RevisionID string
	// File is the attachment's file name (or document id) in this session.
	File  string
	Sheet string
	// TargetType is "grid" or "dimension"; TargetID the grid or dimension.
	// A grid import writes every metric its columns name, as the Import
	// Wizard does; the grid says which one the integration belongs to.
	TargetType string
	TargetID   string
	ColumnMap  map[string]string
	ImportMode string
	// IntegrationID, when set, records the run in that integration's
	// history.
	IntegrationID string
}

// ReadHooks are the gateway operations behind the read tools that look at a
// file or at data. An executor without one refuses that tool.
type ReadHooks struct {
	PreviewFileImport func(ctx context.Context, req FileImportRequest) (string, error)
	PreviewExport     func(ctx context.Context, revisionID, gridID, name string, spec dataexport.Spec) (string, error)
}

// WithReadHooks sets the read executor's gateway hooks and returns it.
func (e *ToolExecutor) WithReadHooks(h ReadHooks) *ToolExecutor {
	e.hooks = h
	return e
}

func integrationToolDefs() []toolDef {
	return []toolDef{
		{
			Name:        "list_integrations",
			Description: "Returns every data integration in the working revision: id, name, type (csv_import = Excel/CSV file import, google_sheets, rest_api, file_export = data export), status, target, its column_map/import_mode or export spec, and its last run. Use it before update_integration, delete_integration or import_file_data with an integration_id.",
			Parameters:  `{"type":"object","properties":{},"required":[]}`,
		},
		{
			Name:        "preview_file_import",
			Description: "Dry-runs importing a spreadsheet the developer attached to this chat (.xlsx, .xlsm or .csv) — nothing is written. Reports the file's sheets, columns and row count, how each column maps (metric, dimension, value, ignored, or unknown), the rows that would fail and why, and what would be imported. Call it before proposing import_file_data, and fix column_map until it reports no errors: an import is all-or-nothing.",
			Parameters: `{"type":"object","properties":{
				"file":{"type":"string","description":"The attached file's name, as listed under Attached documents"},
				"sheet":{"type":"string","description":"Worksheet name; omit for the first sheet"},
				"target_type":{"type":"string","enum":["grid","dimension"]},
				"target_id":{"type":"string","description":"The grid or dimension: id or exact name"},
				"column_map":{"type":"object","description":"File column -> model field (see the File import section of your instructions); omit to use the headers as they are"}
			},"required":["file","target_type","target_id"]}`,
		},
		{
			Name:        "preview_export",
			Description: "Renders an export spec against a grid's current values WITHOUT saving anything, and returns its columns, first rows and row count — or every problem with the spec. Call it before proposing create_export_integration or changing an export's spec.",
			Parameters: `{"type":"object","properties":{
				"grid_id":{"type":"string","description":"The grid: id or exact name"},
				"name":{"type":"string","description":"The export's name (used for the default file name)"},
				"spec":{"type":"object","description":"The export spec (see the Data export section of your instructions)"}
			},"required":["grid_id","spec"]}`,
		},
	}
}

// toolDef is a ToolDef with its parameters as a string literal.
type toolDef struct{ Name, Description, Parameters string }

// resolveRef resolves a read tool's id-or-name reference to a grid or
// dimension into the working revision, as requireInModel does for write
// tools — refusing a name two of them share.
func (e *ToolExecutor) resolveRef(ctx context.Context, kind, ref string) (string, error) {
	table := map[string]string{"grid": "model.grid_def", "dimension": "model.dimension_def"}[kind]
	ref = strings.TrimSpace(ref)
	if table == "" || ref == "" {
		return "", fmt.Errorf("%s is required — its id or exact name", kind)
	}
	if !uuidShaped(ref) {
		var n int
		var id string
		if err := e.pool.QueryRow(ctx, `SELECT count(*)::int, COALESCE(min(id::text),'') FROM `+table+`
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND lower(name)=lower($3)`, e.modelID, e.revID, ref).Scan(&n, &id); err != nil {
			return "", err
		}
		switch {
		case n == 1:
			return id, nil
		case n > 1:
			return "", fmt.Errorf("%d %ss are named %q — pass the id", n, kind, ref)
		}
		return "", fmt.Errorf("%s %q not found in the working revision — pass its exact name or id", kind, ref)
	}
	q := modelScopedResourceSQL[kind]
	var owner, revision, identity string
	if err := e.pool.QueryRow(ctx, q.lookup, ref).Scan(&owner, &revision, &identity); err != nil || owner != e.modelID {
		return "", fmt.Errorf("%s %s not found in this model", kind, ref)
	}
	if revision == "" || revision == e.revID {
		return ref, nil
	}
	var mapped string
	if err := e.pool.QueryRow(ctx, q.counterpart, e.modelID, e.revID, identity).Scan(&mapped); err != nil {
		return "", fmt.Errorf("%s %s has no counterpart in the working revision", kind, ref)
	}
	return mapped, nil
}

func (e *ToolExecutor) listIntegrations(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT i.id::text, i.name, i.type, i.status, i.target_type, COALESCE(i.target_id::text,''),
		       COALESCE(g.name, d.name, f.name, ''), COALESCE(i.config, '{}'::jsonb)::text,
		       COALESCE((SELECT r.status || ' ' || to_char(r.started_at, 'YYYY-MM-DD HH24:MI') || ', ' || r.rows_imported || ' rows'
		                 FROM model.integration_run r WHERE r.integration_id = i.id ORDER BY r.started_at DESC LIMIT 1), 'never run')
		FROM model.integration_def i
		LEFT JOIN model.grid_def g ON i.target_type = 'grid' AND g.id = i.target_id
		LEFT JOIN model.dimension_def d ON i.target_type = 'dimension' AND d.id = i.target_id
		LEFT JOIN model.form_def f ON i.target_type = 'form' AND f.id = i.target_id
		WHERE i.model_id = $1::uuid AND (i.revision_id IS NULL OR i.revision_id = $2::uuid)
		ORDER BY i.type, i.name`, e.modelID, e.revID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var sb strings.Builder
	n := 0
	for rows.Next() {
		var id, name, typ, status, targetType, targetID, targetName, cfg, lastRun string
		if err := rows.Scan(&id, &name, &typ, &status, &targetType, &targetID, &targetName, &cfg, &lastRun); err != nil {
			return "", err
		}
		n++
		fmt.Fprintf(&sb, "- %s (id:%s) type=%s status=%s target=%s %q · last run: %s\n", name, id, typ, status, targetType, targetName, lastRun)
		switch typ {
		case "file_export":
			var spec dataexport.Spec
			_ = json.Unmarshal([]byte(cfg), &spec)
			b, _ := json.Marshal(spec)
			fmt.Fprintf(&sb, "    export: %s\n    spec: %s\n", dataexport.Describe(spec), b)
		case "csv_import", "google_sheets":
			var c struct {
				ColumnMap  map[string]string `json:"column_map"`
				ImportMode string            `json:"import_mode"`
				SheetURL   string            `json:"sheet_url"`
			}
			_ = json.Unmarshal([]byte(cfg), &c)
			b, _ := json.Marshal(c.ColumnMap)
			fmt.Fprintf(&sb, "    column_map: %s · import_mode: %s", b, orDefault(c.ImportMode, "default"))
			if c.SheetURL != "" {
				fmt.Fprintf(&sb, " · sheet: %s", c.SheetURL)
			}
			sb.WriteString("\n")
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if n == 0 {
		return "No integrations in the working revision.", nil
	}
	return fmt.Sprintf("%d integration(s):\n%s", n, sb.String()), nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (e *ToolExecutor) previewFileImport(ctx context.Context, raw json.RawMessage) (string, error) {
	if e.hooks.PreviewFileImport == nil {
		return "", fmt.Errorf("file import is not available here")
	}
	var p struct {
		File       string            `json:"file"`
		Sheet      string            `json:"sheet"`
		TargetType string            `json:"target_type"`
		TargetID   string            `json:"target_id"`
		ColumnMap  map[string]string `json:"column_map"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", fmt.Errorf("invalid params: %w", err)
	}
	if p.File == "" {
		return "", fmt.Errorf("file is required — the attached file's name")
	}
	kind, err := importTargetKind(p.TargetType)
	if err != nil {
		return "", err
	}
	targetID, err := e.resolveRef(ctx, kind, p.TargetID)
	if err != nil {
		return "", err
	}
	return e.hooks.PreviewFileImport(ctx, FileImportRequest{
		RevisionID: e.revID, File: p.File, Sheet: p.Sheet,
		TargetType: p.TargetType, TargetID: targetID, ColumnMap: p.ColumnMap,
	})
}

func (e *ToolExecutor) previewExport(ctx context.Context, raw json.RawMessage) (string, error) {
	if e.hooks.PreviewExport == nil {
		return "", fmt.Errorf("export preview is not available here")
	}
	var p struct {
		GridID string          `json:"grid_id"`
		Name   string          `json:"name"`
		Spec   dataexport.Spec `json:"spec"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", fmt.Errorf("invalid params (spec must be an object with the export fields): %w", err)
	}
	gridID, err := e.resolveRef(ctx, "grid", p.GridID)
	if err != nil {
		return "", err
	}
	return e.hooks.PreviewExport(ctx, e.revID, gridID, p.Name, p.Spec)
}

func importTargetKind(targetType string) (string, error) {
	switch targetType {
	case "grid":
		return "grid", nil
	case "dimension":
		return "dimension", nil
	}
	return "", fmt.Errorf("target_type must be \"grid\" (metric values) or \"dimension\" (members), not %q", targetType)
}

var importModes = map[string]bool{"incremental": true, "replace": true, "full_reload": true}

// ── write tools ──────────────────────────────────────────────────────────────

// integrationNameFree refuses a second integration of the same name in the
// working revision: the assistant references integrations by name, and a
// revision copy matches them by name, so a duplicate is ambiguous to both.
func (e *WriteExecutor) integrationNameFree(ctx context.Context, name, exceptID string) error {
	var n int
	if err := e.pool.QueryRow(ctx, `
		SELECT count(*)::int FROM model.integration_def
		WHERE model_id=$1::uuid AND revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid
		  AND lower(name)=lower($3) AND id::text <> $4`, e.modelID, e.revID, name, exceptID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("an integration named %q already exists in this revision — choose another name, or update that one", name)
	}
	return nil
}

type integrationRow struct {
	ID, Name, Type, TargetType, TargetID string
	Config                               json.RawMessage
}

func (e *WriteExecutor) loadIntegrationDef(ctx context.Context, ref string) (integrationRow, error) {
	if strings.TrimSpace(ref) == "" {
		return integrationRow{}, fmt.Errorf("integration_id is required")
	}
	if !uuidShaped(ref) {
		var n int
		_ = e.pool.QueryRow(ctx, `SELECT count(*)::int FROM model.integration_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3`,
			e.modelID, e.revID, ref).Scan(&n)
		if n > 1 {
			return integrationRow{}, fmt.Errorf("%d integrations are named %q — pass the id from list_integrations", n, ref)
		}
	}
	id, err := e.requireInModel(ctx, "integration", ref)
	if err != nil {
		return integrationRow{}, err
	}
	var row integrationRow
	var cfg []byte
	if err := e.pool.QueryRow(ctx, `
		SELECT id::text, name, type, target_type, COALESCE(target_id::text,''), COALESCE(config,'{}'::jsonb)
		FROM model.integration_def WHERE id=$1::uuid`, id).Scan(&row.ID, &row.Name, &row.Type, &row.TargetType, &row.TargetID, &cfg); err != nil {
		return integrationRow{}, fmt.Errorf("integration %s not found", ref)
	}
	row.Config = cfg
	return row, nil
}

type fileIntegrationConfig struct {
	ColumnMap  map[string]string `json:"column_map,omitempty"`
	ImportMode string            `json:"import_mode,omitempty"`
}

// create_file_integration saves a re-runnable Excel/CSV import: the Import
// Wizard's "save as integration" — a target, a column map and a commit mode.
func (e *WriteExecutor) createFileIntegration(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Name       string            `json:"name"`
		TargetType string            `json:"target_type"`
		TargetID   string            `json:"target_id"`
		ColumnMap  map[string]string `json:"column_map"`
		ImportMode string            `json:"import_mode"`
		Tags       []string          `json:"tags"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	kind, err := importTargetKind(p.TargetType)
	if err != nil {
		return "", "", err
	}
	targetID, err := e.requireInModel(ctx, kind, p.TargetID)
	if err != nil {
		return "", "", err
	}
	cfg, err := fileImportConfig(p.TargetType, p.ColumnMap, p.ImportMode)
	if err != nil {
		return "", "", err
	}
	if err := e.integrationNameFree(ctx, p.Name, ""); err != nil {
		return "", "", err
	}
	tags := p.Tags
	if tags == nil {
		tags = []string{}
	}
	cfgJSON, _ := json.Marshal(cfg)
	var id string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO model.integration_def (model_id, name, type, target_type, target_id, revision_id, status, tags, config)
		VALUES ($1::uuid, $2, 'csv_import', $3, $4::uuid, NULLIF($5,'')::uuid, 'active', $6, $7::jsonb)
		RETURNING id::text`, e.modelID, p.Name, p.TargetType, targetID, e.revID, tags, string(cfgJSON)).Scan(&id); err != nil {
		return "", "", fmt.Errorf("create integration: %w", err)
	}
	return fmt.Sprintf("Excel/CSV integration '%s' created (id: %s) into %s, %d mapped column(s), mode %s",
		p.Name, id, p.TargetType, len(cfg.ColumnMap), orDefault(cfg.ImportMode, "default")), id, nil
}

func fileImportConfig(targetType string, columnMap map[string]string, mode string) (fileIntegrationConfig, error) {
	cfg := fileIntegrationConfig{ColumnMap: columnMap}
	if targetType == "grid" {
		if mode == "" {
			mode = "replace"
		}
		if !importModes[mode] {
			return cfg, fmt.Errorf("import_mode must be replace, incremental or full_reload, not %q", mode)
		}
		cfg.ImportMode = mode
	} else if mode != "" {
		return cfg, fmt.Errorf("import_mode applies to grid imports only — a dimension import adds and updates members")
	}
	return cfg, nil
}

// create_export_integration saves a file_export: a grid and a spec, checked
// against the grid the way the console's export editor checks it.
func (e *WriteExecutor) createExportIntegration(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Name   string          `json:"name"`
		GridID string          `json:"grid_id"`
		Spec   dataexport.Spec `json:"spec"`
		Tags   []string        `json:"tags"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params (spec must be an object with the export fields): %w", err)
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	gridID, err := e.requireInModel(ctx, "grid", p.GridID)
	if err != nil {
		return "", "", err
	}
	g, _, _, err := dataexport.LoadGrid(ctx, e.pool, gridID)
	if err != nil {
		return "", "", err
	}
	if err := dataexport.ValidationError(p.Spec, g); err != nil {
		return "", "", err
	}
	if err := e.integrationNameFree(ctx, p.Name, ""); err != nil {
		return "", "", err
	}
	tags := p.Tags
	if tags == nil {
		tags = []string{}
	}
	cfg, _ := json.Marshal(p.Spec)
	var id string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO model.integration_def (model_id, name, type, target_type, target_id, revision_id, status, tags, config)
		VALUES ($1::uuid, $2, 'file_export', 'grid', $3::uuid, NULLIF($4,'')::uuid, 'active', $5, $6::jsonb)
		RETURNING id::text`, e.modelID, p.Name, gridID, e.revID, tags, string(cfg)).Scan(&id); err != nil {
		return "", "", fmt.Errorf("create export: %w", err)
	}
	return fmt.Sprintf("Export '%s' created (id: %s) from grid '%s': %s", p.Name, id, g.Name, dataexport.Describe(p.Spec)), id, nil
}

// update_integration changes a file import's or an export's name, tags,
// target and config. REST API connectors are edited in their own wizard.
func (e *WriteExecutor) updateIntegration(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		IntegrationID string            `json:"integration_id"`
		Name          *string           `json:"name"`
		Tags          *[]string         `json:"tags"`
		TargetID      string            `json:"target_id"`
		ColumnMap     map[string]string `json:"column_map"`
		ImportMode    *string           `json:"import_mode"`
		Spec          json.RawMessage   `json:"spec"`
		Status        *string           `json:"status"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	row, err := e.loadIntegrationDef(ctx, p.IntegrationID)
	if err != nil {
		return "", "", err
	}
	if row.Type == "rest_api" {
		return "", "", fmt.Errorf("REST API integrations are edited in the Integrations tab's REST API wizard, not here")
	}
	name := row.Name
	if p.Name != nil {
		name = strings.TrimSpace(*p.Name)
		if name == "" {
			return "", "", fmt.Errorf("name cannot be empty")
		}
		if err := e.integrationNameFree(ctx, name, row.ID); err != nil {
			return "", "", err
		}
	}
	if p.Status != nil && *p.Status != "active" && *p.Status != "draft" {
		return "", "", fmt.Errorf("status must be active or draft")
	}
	targetID := row.TargetID
	if p.TargetID != "" {
		kind := row.TargetType
		if kind != "grid" && kind != "dimension" {
			return "", "", fmt.Errorf("a %s integration's target is changed in the console", row.TargetType)
		}
		if targetID, err = e.requireInModel(ctx, kind, p.TargetID); err != nil {
			return "", "", err
		}
	}

	var cfg []byte
	var changed []string
	switch row.Type {
	case "file_export":
		if p.ColumnMap != nil || p.ImportMode != nil {
			return "", "", fmt.Errorf("an export has a spec, not a column_map or import_mode")
		}
		var spec dataexport.Spec
		if len(p.Spec) > 0 {
			if err := json.Unmarshal(p.Spec, &spec); err != nil {
				return "", "", fmt.Errorf("spec must be an object with the export fields: %w", err)
			}
			changed = append(changed, "spec")
		} else {
			_ = json.Unmarshal(row.Config, &spec)
		}
		g, _, _, err := dataexport.LoadGrid(ctx, e.pool, targetID)
		if err != nil {
			return "", "", err
		}
		if err := dataexport.ValidationError(spec, g); err != nil {
			return "", "", err
		}
		cfg, _ = json.Marshal(spec)
	default: // csv_import, google_sheets
		if len(p.Spec) > 0 {
			return "", "", fmt.Errorf("only an export has a spec")
		}
		var c map[string]any
		_ = json.Unmarshal(row.Config, &c)
		if c == nil {
			c = map[string]any{}
		}
		if p.ColumnMap != nil {
			c["column_map"] = p.ColumnMap
			changed = append(changed, "column_map")
		}
		if p.ImportMode != nil {
			if row.TargetType != "grid" {
				return "", "", fmt.Errorf("import_mode applies to grid imports only")
			}
			if !importModes[*p.ImportMode] {
				return "", "", fmt.Errorf("import_mode must be replace, incremental or full_reload, not %q", *p.ImportMode)
			}
			c["import_mode"] = *p.ImportMode
			changed = append(changed, "import_mode")
		}
		cfg, _ = json.Marshal(c)
	}
	if p.Name != nil {
		changed = append(changed, "name")
	}
	if p.Tags != nil {
		changed = append(changed, "tags")
	}
	if p.TargetID != "" {
		changed = append(changed, "target")
	}
	if p.Status != nil {
		changed = append(changed, "status")
	}
	if len(changed) == 0 {
		return "", "", fmt.Errorf("nothing to change — pass name, tags, target_id, status, column_map, import_mode or spec")
	}
	if _, err := e.pool.Exec(ctx, `
		UPDATE model.integration_def
		SET name=$2, target_id=NULLIF($3,'')::uuid, config=$4::jsonb,
		    tags=COALESCE($5, tags), status=COALESCE($6, status)
		WHERE id=$1::uuid`, row.ID, name, targetID, string(cfg), p.Tags, p.Status); err != nil {
		return "", "", fmt.Errorf("update integration: %w", err)
	}
	return fmt.Sprintf("Integration '%s' updated (%s)", name, strings.Join(changed, ", ")), "", nil
}

// delete_integration removes any integration, its run history and the
// dashboard buttons that ran it — what the console's delete does.
func (e *WriteExecutor) deleteIntegration(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		IntegrationID string `json:"integration_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	row, err := e.loadIntegrationDef(ctx, p.IntegrationID)
	if err != nil {
		return "", "", err
	}
	if err := modeledit.DropWidgetsReferencing(ctx, e.pool, row.ID); err != nil {
		return "", "", fmt.Errorf("remove widgets running it: %w", err)
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM model.integration_def WHERE id=$1::uuid`, row.ID); err != nil {
		return "", "", fmt.Errorf("delete integration: %w", err)
	}
	return fmt.Sprintf("Integration '%s' (%s) deleted", row.Name, row.Type), "", nil
}

// import_file_data imports an attached spreadsheet into the working
// revision, optionally as a run of a saved file integration whose target,
// column map and mode it then uses (any given here override them).
func (e *WriteExecutor) importFileData(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		File          string            `json:"file"`
		Sheet         string            `json:"sheet"`
		IntegrationID string            `json:"integration_id"`
		TargetType    string            `json:"target_type"`
		TargetID      string            `json:"target_id"`
		ColumnMap     map[string]string `json:"column_map"`
		ImportMode    string            `json:"import_mode"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if e.hooks.ImportFile == nil {
		return "", "", fmt.Errorf("file import is not available here")
	}
	if strings.TrimSpace(p.File) == "" {
		return "", "", fmt.Errorf("file is required — the attached file's name")
	}
	req := FileImportRequest{RevisionID: e.revID, File: p.File, Sheet: p.Sheet}
	if p.IntegrationID != "" {
		row, err := e.loadIntegrationDef(ctx, p.IntegrationID)
		if err != nil {
			return "", "", err
		}
		if row.Type != "csv_import" {
			return "", "", fmt.Errorf("integration '%s' is a %s integration; an attached file runs a csv_import (Excel/CSV) one", row.Name, row.Type)
		}
		var c fileIntegrationConfig
		_ = json.Unmarshal(row.Config, &c)
		req.IntegrationID, req.TargetType, req.TargetID = row.ID, row.TargetType, row.TargetID
		req.ColumnMap, req.ImportMode = c.ColumnMap, c.ImportMode
		if p.TargetType != "" && p.TargetType != row.TargetType {
			return "", "", fmt.Errorf("integration '%s' imports into a %s; leave target_type out or match it", row.Name, row.TargetType)
		}
	}
	if p.TargetType != "" {
		req.TargetType = p.TargetType
	}
	kind, err := importTargetKind(req.TargetType)
	if err != nil {
		return "", "", err
	}
	if p.TargetID != "" {
		req.TargetID = p.TargetID
	}
	if req.TargetID, err = e.requireInModel(ctx, kind, req.TargetID); err != nil {
		return "", "", err
	}
	if p.ColumnMap != nil {
		req.ColumnMap = p.ColumnMap
	}
	if p.ImportMode != "" {
		req.ImportMode = p.ImportMode
	}
	cfg, err := fileImportConfig(req.TargetType, req.ColumnMap, req.ImportMode)
	if err != nil {
		return "", "", err
	}
	req.ImportMode = cfg.ImportMode
	result, err := e.hooks.ImportFile(ctx, req)
	if err != nil {
		return "", "", err
	}
	return result, "", nil
}
