// Package modeltransfer serializes a model at one specific revision to a
// self-contained package, and recreates a model from such a package under a
// new application with every cross-entity reference remapped to freshly
// generated IDs. The entity set mirrors the revision deep-copy in
// developerRevisions (metrics, dependencies, dimensions with members and
// typed properties, grids, dashboards with widgets and folders, forms with
// records and metric mappings, integrations, workflow defs and automation
// rules, plus latest-wins fact values).
//
// This package holds no HTTP/auth concerns — those live in
// internal/gateway/model_transfer.go (the tenant_admin-gated HTTP
// handlers) and internal/deployment/builder.go (the tarball builder), both
// of which call CollectExport/Import directly rather than re-deriving
// these queries.
package modeltransfer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	PackageFormat  = "mavericks-model-export"
	PackageVersion = 1
	// FactsPolicy documents, as an explicit and inspectable field rather
	// than only an in-code comment, the policy CollectExport applies when
	// gathering runtime.fact_input rows: one latest-wins row per cell for
	// direct (manually entered, source_ref IS NULL) facts, plus every
	// form-posted (source_ref IS NOT NULL) row for that same cell — both
	// survive, they are summed by the grid() cell query, not alternatives.
	// See Fact.SourceMappingID.
	FactsPolicy = "latest_direct_value_per_cell_plus_all_form_posted_values"
)

// Queryer is satisfied by both *pgxpool.Pool and pgx.Tx — CollectExport and
// ResolveRevision only ever read, so they work identically against a live
// pool or an already-open transaction.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Member struct {
	ID             string          `json:"id"`
	Code           string          `json:"code"`
	Label          string          `json:"label"`
	ParentMemberID *string         `json:"parent_member_id,omitempty"`
	Properties     json.RawMessage `json:"properties,omitempty"`
	SortOrder      int             `json:"sort_order"`
}

type DimProperty struct {
	Name     string `json:"name"`
	DataType string `json:"data_type"`
}

type Dimension struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	AggRule           string          `json:"agg_rule"`
	Properties        json.RawMessage `json:"properties,omitempty"`
	ParentDimensionID *string         `json:"parent_dimension_id,omitempty"`
	SourceDimensionID *string         `json:"source_dimension_id,omitempty"`
	SourceProperty    *string         `json:"source_property,omitempty"`
	Members           []Member        `json:"members"`
	TypedProperties   []DimProperty   `json:"typed_properties,omitempty"`
}

type Metric struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Formula        *string `json:"formula,omitempty"`
	StorageType    string  `json:"storage_type"`
	IsInput        bool    `json:"is_input"`
	AggRule        string  `json:"agg_rule"`
	Format         string  `json:"format"`
	FormatDecimals int     `json:"format_decimals"`
	FormatCurrency string  `json:"format_currency"`
}

type Dependency struct {
	MetricID  string `json:"metric_id"`
	DependsOn string `json:"depends_on_metric_id"`
}

type GridMetric struct {
	MetricID  string `json:"metric_id"`
	SortOrder int    `json:"sort_order"`
}

type GridDimension struct {
	DimensionID  string `json:"dimension_id"`
	DisplayLevel *int   `json:"display_level,omitempty"`
}

type Grid struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	RollupSourceGridID *string         `json:"rollup_source_grid_id,omitempty"`
	Metrics            []GridMetric    `json:"metrics"`
	Dimensions         []GridDimension `json:"dimensions"`
}

type Folder struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	ParentID *string `json:"parent_id,omitempty"`
}

type Widget struct {
	WidgetType string          `json:"widget_type"`
	RefID      *string         `json:"ref_id,omitempty"`
	Content    *string         `json:"content,omitempty"`
	SortOrder  int             `json:"sort_order"`
	ColStart   int             `json:"col_start"`
	ColSpan    int             `json:"col_span"`
	PosX       *int            `json:"pos_x,omitempty"`
	PosY       *int            `json:"pos_y,omitempty"`
	SizeW      *int            `json:"size_w,omitempty"`
	SizeH      *int            `json:"size_h,omitempty"`
	Title      *string         `json:"title,omitempty"`
	ShowTitle  bool            `json:"show_title"`
	Props      json.RawMessage `json:"widget_props,omitempty"`
}

type Dashboard struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Tags     []string `json:"tags"`
	Category string   `json:"category"`
	FolderID *string  `json:"folder_id,omitempty"`
	Widgets  []Widget `json:"widgets"`
}

type Form struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Label  string          `json:"label"`
	Fields json.RawMessage `json:"fields"`
}

type FormRecord struct {
	FormID string          `json:"form_id"`
	Data   json.RawMessage `json:"data"`
	Status string          `json:"status"`
}

type FormMapping struct {
	// ID is the mapping's original ID — carried only so Fact rows can
	// reference "which mapping posted this" (SourceMappingID); never
	// reused as the new mapping's ID on import (that's always a fresh
	// INSERT ... RETURNING id, exactly like every other entity in this
	// package).
	ID                string          `json:"id"`
	FormID            string          `json:"form_id"`
	GridID            *string         `json:"grid_id,omitempty"`
	Name              string          `json:"name"`
	SourceField       string          `json:"source_field"`
	TargetMetricID    string          `json:"target_metric_id"`
	Aggregation       string          `json:"aggregation"`
	PostingStatuses   []string        `json:"posting_statuses"`
	DimensionMappings json.RawMessage `json:"dimension_mappings"`
	LivePosting       bool            `json:"live_posting"`
}

type Integration struct {
	// ID is the original ID, carried only so dashboard widgets of type
	// integration_button (ref_id → integration_def) can be remapped on
	// import; never reused as the new row's ID. Same rule as FormMapping.ID.
	ID         string          `json:"id,omitempty"`
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	TargetType string          `json:"target_type"`
	TargetID   *string         `json:"target_id,omitempty"`
	Config     json.RawMessage `json:"config"`
}

type Workflow struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	TriggerEvent  string          `json:"trigger_event"`
	SubjectType   string          `json:"subject_type"`
	SubjectConfig json.RawMessage `json:"subject_config"`
	Steps         json.RawMessage `json:"steps"`
	ContextSchema json.RawMessage `json:"context_schema"`
	Status        string          `json:"status"`
}

type AutomationRule struct {
	// ID is the original ID, carried only so dashboard widgets of type
	// automation_button (ref_id → automation_rule) can be remapped on
	// import; never reused as the new row's ID. Same rule as FormMapping.ID.
	ID            string  `json:"id,omitempty"`
	Name          string  `json:"name"`
	Description   string  `json:"description"`
	TriggerType   string  `json:"trigger_type"`
	WorkflowName  string  `json:"workflow_name"`
	Enabled       bool    `json:"enabled"`
	WorkflowDefID *string `json:"workflow_def_id,omitempty"`
	SourceFormID  *string `json:"source_form_id,omitempty"`
	SourceGridID  *string `json:"source_grid_id,omitempty"`
}

type Fact struct {
	MetricID   string          `json:"metric_id"`
	DimMembers json.RawMessage `json:"dim_members"`
	Value      float64         `json:"value"`
	// SourceMappingID is the FormMapping.ID that posted this fact, or nil
	// for a direct (manually entered) fact. See FactsPolicy.
	SourceMappingID *string `json:"source_mapping_id,omitempty"`
}

type Package struct {
	Format       string    `json:"format"`
	Version      int       `json:"version"`
	ExportedAt   time.Time `json:"exported_at"`
	ModelName    string    `json:"model_name"`
	StorageType  string    `json:"storage_type"`
	RevisionName string    `json:"revision_name"`
	FactsPolicy  string    `json:"facts_policy"`
	// IncludeData says whether runtime data (fact values and form records)
	// travels with the definitions. A definitions-only package recreates
	// the model's structure — dimensions, metrics, grids, forms, mappings,
	// dashboards, workflows — with no entered values; see ExportOptions.
	IncludeData     bool             `json:"include_data"`
	Dimensions      []Dimension      `json:"dimensions"`
	Metrics         []Metric         `json:"metrics"`
	Dependencies    []Dependency     `json:"dependencies"`
	Grids           []Grid           `json:"grids"`
	Folders         []Folder         `json:"folders"`
	Dashboards      []Dashboard      `json:"dashboards"`
	Forms           []Form           `json:"forms"`
	FormRecords     []FormRecord     `json:"form_records"`
	FormMappings    []FormMapping    `json:"form_mappings"`
	Integrations    []Integration    `json:"integrations"`
	Workflows       []Workflow       `json:"workflows"`
	AutomationRules []AutomationRule `json:"automation_rules"`
	Facts           []Fact           `json:"facts"`
}

// ── Export ────────────────────────────────────────────────────────────────────

// ResolveRevision resolves a specific revision (revisionID non-empty,
// always verified to belong to modelID) or falls back to modelID's active
// revision, then its most recently created revision.
func ResolveRevision(ctx context.Context, q Queryer, modelID, revisionID string) (revID, name string, err error) {
	if revisionID != "" {
		err = q.QueryRow(ctx,
			`SELECT id::text, name FROM model.revision WHERE id=$1::uuid AND model_id=$2::uuid`,
			revisionID, modelID,
		).Scan(&revID, &name)
		return
	}
	err = q.QueryRow(ctx, `
		SELECT s.id::text, s.name
		FROM model.revision s
		JOIN core.model m ON m.active_revision_id = s.id
		WHERE m.id=$1::uuid`, modelID,
	).Scan(&revID, &name)
	if err != nil {
		err = q.QueryRow(ctx,
			`SELECT id::text, name FROM model.revision WHERE model_id=$1::uuid ORDER BY created_at DESC LIMIT 1`,
			modelID,
		).Scan(&revID, &name)
	}
	return
}

// ExportOptions selects what CollectExportWithOptions gathers beyond the
// model's definitions.
type ExportOptions struct {
	// IncludeData carries runtime.fact_input values (per FactsPolicy) and
	// runtime.form_record rows. False exports definitions only — the way
	// to hand a model's structure to another tenant without its numbers.
	IncludeData bool
}

// CollectExport is CollectExportWithOptions with data included — the full
// package every existing caller expects.
func CollectExport(ctx context.Context, q Queryer, modelID, revisionID, revisionName string) (*Package, error) {
	return CollectExportWithOptions(ctx, q, modelID, revisionID, revisionName, ExportOptions{IncludeData: true})
}

// CollectExportWithOptions gathers a model+revision's full entity graph — everything
// needed to recreate it via Import. modelID/revisionID/revisionName must
// already be resolved (see ResolveRevision). Every query is scoped
// `WHERE model_id=$1 AND (revision_id=$2 OR revision_id IS NULL)` —
// revision-global (NULL revision_id) rows are included alongside the exact
// revision match, since some entities (e.g. a workflow def created via the
// older workflow.Store.CreateWorkflowDef) predate revision scoping.
func CollectExportWithOptions(ctx context.Context, q Queryer, modelID, revisionID, revisionName string, opts ExportOptions) (*Package, error) {
	pkg := &Package{
		Format:       PackageFormat,
		Version:      PackageVersion,
		ExportedAt:   time.Now().UTC(),
		FactsPolicy:  FactsPolicy,
		IncludeData:  opts.IncludeData,
		RevisionName: revisionName,
	}
	if !opts.IncludeData {
		pkg.FactsPolicy = "definitions_only"
	}
	if err := q.QueryRow(ctx,
		`SELECT name, storage_type::text FROM core.model WHERE id=$1::uuid`, modelID,
	).Scan(&pkg.ModelName, &pkg.StorageType); err != nil {
		return nil, fmt.Errorf("model not found: %w", err)
	}

	// Dimensions + members + typed properties.
	rows, err := q.Query(ctx, `
		SELECT id::text, name, agg_rule, properties, parent_dimension_id::text, source_dimension_id::text, source_property
		FROM model.dimension_def WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL) ORDER BY created_at`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d Dimension
		if err := rows.Scan(&d.ID, &d.Name, &d.AggRule, &d.Properties, &d.ParentDimensionID, &d.SourceDimensionID, &d.SourceProperty); err != nil {
			rows.Close()
			return nil, err
		}
		d.Members = []Member{}
		pkg.Dimensions = append(pkg.Dimensions, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range pkg.Dimensions {
		mrows, err := q.Query(ctx, `
			SELECT id::text, code, label, parent_member_id::text, properties, sort_order
			FROM model.dimension_member WHERE dimension_id=$1::uuid ORDER BY sort_order, code`,
			pkg.Dimensions[i].ID)
		if err != nil {
			return nil, err
		}
		for mrows.Next() {
			var m Member
			if err := mrows.Scan(&m.ID, &m.Code, &m.Label, &m.ParentMemberID, &m.Properties, &m.SortOrder); err != nil {
				mrows.Close()
				return nil, err
			}
			pkg.Dimensions[i].Members = append(pkg.Dimensions[i].Members, m)
		}
		mrows.Close()
		if err := mrows.Err(); err != nil {
			return nil, err
		}
		prows, err := q.Query(ctx,
			`SELECT name, data_type FROM model.dimension_property WHERE dimension_id=$1::uuid ORDER BY name`,
			pkg.Dimensions[i].ID)
		if err != nil {
			return nil, err
		}
		for prows.Next() {
			var p DimProperty
			if err := prows.Scan(&p.Name, &p.DataType); err != nil {
				prows.Close()
				return nil, err
			}
			pkg.Dimensions[i].TypedProperties = append(pkg.Dimensions[i].TypedProperties, p)
		}
		prows.Close()
		if err := prows.Err(); err != nil {
			return nil, err
		}
	}

	// Metrics + dependencies.
	rows, err = q.Query(ctx, `
		SELECT id::text, name, formula, storage_type::text, is_input, agg_rule,
		       COALESCE(format,''), COALESCE(format_decimals,0), COALESCE(format_currency,'')
		FROM model.metric_def WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL) ORDER BY created_at`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m Metric
		if err := rows.Scan(&m.ID, &m.Name, &m.Formula, &m.StorageType, &m.IsInput, &m.AggRule, &m.Format, &m.FormatDecimals, &m.FormatCurrency); err != nil {
			rows.Close()
			return nil, err
		}
		pkg.Metrics = append(pkg.Metrics, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `
		SELECT cd.metric_id::text, cd.depends_on_metric_id::text
		FROM model.calc_dependency cd
		JOIN model.metric_def m ON m.id = cd.metric_id
		WHERE m.model_id=$1::uuid AND (m.revision_id=$2::uuid OR m.revision_id IS NULL)`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d Dependency
		if err := rows.Scan(&d.MetricID, &d.DependsOn); err != nil {
			rows.Close()
			return nil, err
		}
		pkg.Dependencies = append(pkg.Dependencies, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Grids with their metric/dimension membership.
	rows, err = q.Query(ctx, `
		SELECT id::text, name, rollup_source_grid_id::text
		FROM model.grid_def WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL) ORDER BY created_at`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var g Grid
		if err := rows.Scan(&g.ID, &g.Name, &g.RollupSourceGridID); err != nil {
			rows.Close()
			return nil, err
		}
		g.Metrics, g.Dimensions = []GridMetric{}, []GridDimension{}
		pkg.Grids = append(pkg.Grids, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range pkg.Grids {
		gmRows, err := q.Query(ctx,
			`SELECT metric_id::text, sort_order FROM model.grid_metric WHERE grid_id=$1::uuid ORDER BY sort_order`,
			pkg.Grids[i].ID)
		if err != nil {
			return nil, err
		}
		for gmRows.Next() {
			var gm GridMetric
			if err := gmRows.Scan(&gm.MetricID, &gm.SortOrder); err != nil {
				gmRows.Close()
				return nil, err
			}
			pkg.Grids[i].Metrics = append(pkg.Grids[i].Metrics, gm)
		}
		gmRows.Close()
		if err := gmRows.Err(); err != nil {
			return nil, err
		}
		gdRows, err := q.Query(ctx,
			`SELECT dimension_id::text, display_level FROM model.grid_dimension WHERE grid_id=$1::uuid`,
			pkg.Grids[i].ID)
		if err != nil {
			return nil, err
		}
		for gdRows.Next() {
			var gd GridDimension
			if err := gdRows.Scan(&gd.DimensionID, &gd.DisplayLevel); err != nil {
				gdRows.Close()
				return nil, err
			}
			pkg.Grids[i].Dimensions = append(pkg.Grids[i].Dimensions, gd)
		}
		gdRows.Close()
		if err := gdRows.Err(); err != nil {
			return nil, err
		}
	}

	// Dashboard folders, dashboards, widgets.
	rows, err = q.Query(ctx, `
		SELECT id::text, name, parent_id::text
		FROM model.dashboard_folder WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL) ORDER BY created_at`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.Name, &f.ParentID); err != nil {
			rows.Close()
			return nil, err
		}
		pkg.Folders = append(pkg.Folders, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `
		SELECT id::text, name, tags, COALESCE(category,''), folder_id::text
		FROM model.dashboard_def WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL) ORDER BY created_at`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d Dashboard
		if err := rows.Scan(&d.ID, &d.Name, &d.Tags, &d.Category, &d.FolderID); err != nil {
			rows.Close()
			return nil, err
		}
		if d.Tags == nil {
			d.Tags = []string{}
		}
		d.Widgets = []Widget{}
		pkg.Dashboards = append(pkg.Dashboards, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range pkg.Dashboards {
		wRows, err := q.Query(ctx, `
			SELECT widget_type, ref_id, content, sort_order, col_start, col_span,
			       pos_x, pos_y, size_w, size_h, title, show_title, widget_props
			FROM model.dashboard_widget WHERE dashboard_id=$1::uuid ORDER BY sort_order`,
			pkg.Dashboards[i].ID)
		if err != nil {
			return nil, err
		}
		for wRows.Next() {
			var wd Widget
			if err := wRows.Scan(&wd.WidgetType, &wd.RefID, &wd.Content, &wd.SortOrder, &wd.ColStart, &wd.ColSpan,
				&wd.PosX, &wd.PosY, &wd.SizeW, &wd.SizeH, &wd.Title, &wd.ShowTitle, &wd.Props); err != nil {
				wRows.Close()
				return nil, err
			}
			pkg.Dashboards[i].Widgets = append(pkg.Dashboards[i].Widgets, wd)
		}
		wRows.Close()
		if err := wRows.Err(); err != nil {
			return nil, err
		}
	}

	// Forms, records, metric mappings.
	rows, err = q.Query(ctx, `
		SELECT id::text, name, label, COALESCE(fields,'[]'::jsonb)
		FROM model.form_def WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL) ORDER BY created_at`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f Form
		if err := rows.Scan(&f.ID, &f.Name, &f.Label, &f.Fields); err != nil {
			rows.Close()
			return nil, err
		}
		pkg.Forms = append(pkg.Forms, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if opts.IncludeData {
		rows, err = q.Query(ctx, `
			SELECT fr.form_id::text, fr.data, fr.status::text
			FROM runtime.form_record fr
			JOIN model.form_def fd ON fd.id = fr.form_id
			WHERE fd.model_id=$1::uuid AND (fd.revision_id=$2::uuid OR fd.revision_id IS NULL)
			ORDER BY fr.created_at`,
			modelID, revisionID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var rec FormRecord
			if err := rows.Scan(&rec.FormID, &rec.Data, &rec.Status); err != nil {
				rows.Close()
				return nil, err
			}
			pkg.FormRecords = append(pkg.FormRecords, rec)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	rows, err = q.Query(ctx, `
		SELECT fmm.id::text, fmm.form_id::text, fmm.grid_id::text, fmm.name, fmm.source_field, fmm.target_metric_id::text,
		       fmm.aggregation, fmm.posting_statuses, fmm.dimension_mappings, fmm.live_posting
		FROM model.form_metric_mapping fmm
		JOIN model.form_def fd ON fd.id = fmm.form_id
		WHERE fd.model_id=$1::uuid AND (fd.revision_id=$2::uuid OR fd.revision_id IS NULL)`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m FormMapping
		if err := rows.Scan(&m.ID, &m.FormID, &m.GridID, &m.Name, &m.SourceField, &m.TargetMetricID,
			&m.Aggregation, &m.PostingStatuses, &m.DimensionMappings, &m.LivePosting); err != nil {
			rows.Close()
			return nil, err
		}
		pkg.FormMappings = append(pkg.FormMappings, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Integrations.
	rows, err = q.Query(ctx, `
		SELECT id::text, name, type, target_type, target_id::text, COALESCE(config,'{}'::jsonb)
		FROM model.integration_def WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL) ORDER BY created_at`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var i Integration
		if err := rows.Scan(&i.ID, &i.Name, &i.Type, &i.TargetType, &i.TargetID, &i.Config); err != nil {
			rows.Close()
			return nil, err
		}
		pkg.Integrations = append(pkg.Integrations, i)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Workflows and automation rules (application-scoped, filtered to this revision).
	rows, err = q.Query(ctx, `
		SELECT wd.id::text, wd.name, COALESCE(wd.description,''), wd.trigger_event,
		       COALESCE(wd.subject_type,''), COALESCE(wd.subject_config,'{}'::jsonb),
		       wd.steps, wd.context_schema, wd.status
		FROM workflow.workflow_def wd
		WHERE wd.application_id = (SELECT application_id FROM core.model WHERE id=$1::uuid)
		  AND (wd.revision_id=$2::uuid OR wd.revision_id IS NULL)
		ORDER BY wd.created_at`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var wf Workflow
		if err := rows.Scan(&wf.ID, &wf.Name, &wf.Description, &wf.TriggerEvent,
			&wf.SubjectType, &wf.SubjectConfig, &wf.Steps, &wf.ContextSchema, &wf.Status); err != nil {
			rows.Close()
			return nil, err
		}
		pkg.Workflows = append(pkg.Workflows, wf)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `
		SELECT ar.id::text, ar.name, COALESCE(ar.description,''), ar.trigger_type::text, ar.workflow_name, ar.enabled,
		       ar.workflow_def_id::text, ar.source_form_id::text, ar.source_grid_id::text
		FROM workflow.automation_rule ar
		WHERE ar.application_id = (SELECT application_id FROM core.model WHERE id=$1::uuid)
		  AND (ar.revision_id=$2::uuid OR ar.revision_id IS NULL)
		ORDER BY ar.created_at`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ar AutomationRule
		if err := rows.Scan(&ar.ID, &ar.Name, &ar.Description, &ar.TriggerType, &ar.WorkflowName, &ar.Enabled,
			&ar.WorkflowDefID, &ar.SourceFormID, &ar.SourceGridID); err != nil {
			rows.Close()
			return nil, err
		}
		pkg.AutomationRules = append(pkg.AutomationRules, ar)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if opts.IncludeData {
		// Facts: fact_input is an append-only ledger where direct (manually
		// entered, source_ref IS NULL) and form-posted (source_ref IS NOT NULL)
		// rows for the same cell are fundamentally different — the grid() cell
		// query (internal/gateway/handler.go) sums the latest direct row with
		// EVERY form-posted row, not "whichever row is newest wins". Export
		// must preserve that same raw-row shape (one direct row per cell +
		// every form-posted row, each still tagged with which mapping posted
		// it) rather than collapsing to a single "latest wins" value per cell,
		// or any cell a form has ever posted into silently loses or corrupts
		// data on import. See FactsPolicy.
		rows, err = q.Query(ctx, `
			SELECT DISTINCT ON (metric_id, dim_members)
			       metric_id::text, dim_members, value
			FROM runtime.fact_input
			WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL) AND source_ref IS NULL
			ORDER BY metric_id, dim_members, entered_at DESC, id DESC`,
			modelID, revisionID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var f Fact
			if err := rows.Scan(&f.MetricID, &f.DimMembers, &f.Value); err != nil {
				rows.Close()
				return nil, err
			}
			pkg.Facts = append(pkg.Facts, f)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}

		// source_ref is resolved to THIS revision's mapping by (form name,
		// mapping name, source field) rather than exported verbatim: a revision
		// created by duplication used to carry its copied facts' source_ref
		// pointing at the ORIGINAL revision's mapping (fixed in duplicateRevision
		// the same day this was found), so a verbatim export tagged those facts
		// with a mapping ID that is not in the package and Import silently
		// dropped every one of them. The self-match (n.id = o.id) is preferred
		// so a correct source_ref is exported unchanged.
		rows, err = q.Query(ctx, `
			SELECT fi.metric_id::text, fi.dim_members, fi.value,
			       COALESCE((
			           SELECT n.id::text
			           FROM model.form_metric_mapping o
			           JOIN model.form_def od ON od.id = o.form_id
			           JOIN model.form_def nd ON nd.model_id = od.model_id AND nd.name = od.name
			                                 AND (nd.revision_id = $2::uuid OR nd.revision_id IS NULL)
			           JOIN model.form_metric_mapping n ON n.form_id = nd.id AND n.name = o.name AND n.source_field = o.source_field
			           WHERE o.id = fi.source_ref
			           ORDER BY (n.id = o.id) DESC, (nd.revision_id IS NOT NULL) DESC
			           LIMIT 1
			       ), fi.source_ref::text)
			FROM runtime.fact_input fi
			WHERE fi.model_id=$1::uuid AND (fi.revision_id=$2::uuid OR fi.revision_id IS NULL) AND fi.source_ref IS NOT NULL`,
			modelID, revisionID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var f Fact
			if err := rows.Scan(&f.MetricID, &f.DimMembers, &f.Value, &f.SourceMappingID); err != nil {
				rows.Close()
				return nil, err
			}
			pkg.Facts = append(pkg.Facts, f)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	return pkg, nil
}

// ── Import ────────────────────────────────────────────────────────────────────

type ImportRequest struct {
	ApplicationID string  `json:"application_id"`
	ModelName     string  `json:"model_name,omitempty"`    // override; defaults to package's
	RevisionName  string  `json:"revision_name,omitempty"` // override; defaults to package's
	Package       Package `json:"package"`
}

// remap returns the mapped ID for old (or old itself when unmapped) — used
// for optional references where a dangling source ref shouldn't abort the
// import.
func remap(m map[string]string, old *string) *string {
	if old == nil {
		return nil
	}
	if n, ok := m[*old]; ok {
		return &n
	}
	return old
}

// remapJSONKeys rewrites the top-level object keys of raw through m (used
// for {dimension_id: …} maps like fact dim_members and dimension_mappings).
func remapJSONKeys(raw json.RawMessage, m map[string]string) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return raw
	}
	out := make(map[string]json.RawMessage, len(obj))
	for k, v := range obj {
		if nk, ok := m[k]; ok {
			k = nk
		}
		out[k] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return b
}

// remapJSONArrayFields rewrites the named ID fields of each object in a JSON
// array (form fields' dimension_id/metric_id, context_schema's dimension_id).
func remapJSONArrayFields(raw json.RawMessage, field string, m map[string]string) json.RawMessage {
	var arr []map[string]json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return raw
	}
	for _, obj := range arr {
		v, ok := obj[field]
		if !ok {
			continue
		}
		var id string
		if json.Unmarshal(v, &id) != nil {
			continue
		}
		if nid, ok := m[id]; ok {
			b, _ := json.Marshal(nid)
			obj[field] = b
		}
	}
	b, err := json.Marshal(arr)
	if err != nil {
		return raw
	}
	return b
}

// remapJSONObjectFields rewrites the named string-valued top-level fields of
// a JSON object, each through its own map (a workflow's subject_config
// carries form_id / grid_id / metric_id side by side).
func remapJSONObjectFields(raw json.RawMessage, fields map[string]map[string]string) json.RawMessage {
	var obj map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil {
		return raw
	}
	changed := false
	for field, m := range fields {
		v, ok := obj[field]
		if !ok {
			continue
		}
		var id string
		if json.Unmarshal(v, &id) != nil {
			continue
		}
		if nid, ok := m[id]; ok {
			b, _ := json.Marshal(nid)
			obj[field] = b
			changed = true
		}
	}
	if !changed {
		return raw
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return b
}

// Import recreates a packaged model under req.ApplicationID as a new model
// with a single revision, remapping every cross-entity reference. Runs
// entirely inside tx — the caller commits or rolls back.
func Import(ctx context.Context, tx pgx.Tx, req ImportRequest, importerID string) (modelID, revisionID string, err error) {
	pkg := &req.Package
	modelName := req.ModelName
	if modelName == "" {
		modelName = pkg.ModelName
	}
	revisionName := req.RevisionName
	if revisionName == "" {
		revisionName = pkg.RevisionName
	}
	if revisionName == "" {
		revisionName = "Imported"
	}
	storageType := pkg.StorageType
	if storageType == "" {
		storageType = "oltp"
	}

	if err = tx.QueryRow(ctx,
		`INSERT INTO core.model (application_id, name, storage_type) VALUES ($1::uuid, $2, $3::core.storage_type) RETURNING id::text`,
		req.ApplicationID, modelName, storageType).Scan(&modelID); err != nil {
		return "", "", fmt.Errorf("create model: %w", err)
	}
	if err = tx.QueryRow(ctx,
		`INSERT INTO model.revision (model_id, name, description) VALUES ($1::uuid, $2, 'Imported from package') RETURNING id::text`,
		modelID, revisionName).Scan(&revisionID); err != nil {
		return "", "", fmt.Errorf("create revision: %w", err)
	}

	// Dimensions (parents/sources remapped after all rows exist), members,
	// typed properties.
	dimMap := make(map[string]string, len(pkg.Dimensions))
	memberMap := map[string]string{}
	for _, d := range pkg.Dimensions {
		var newID string
		if err = tx.QueryRow(ctx, `
			INSERT INTO model.dimension_def (model_id, revision_id, name, agg_rule, properties, source_property)
			VALUES ($1::uuid, $2::uuid, $3, $4, COALESCE($5::jsonb,'[]'::jsonb), $6)
			RETURNING id::text`,
			modelID, revisionID, d.Name, d.AggRule, []byte(d.Properties), d.SourceProperty).Scan(&newID); err != nil {
			return "", "", fmt.Errorf("dimension %q: %w", d.Name, err)
		}
		dimMap[d.ID] = newID
		for _, m := range d.Members {
			var newMemberID string
			if err = tx.QueryRow(ctx, `
				INSERT INTO model.dimension_member (dimension_id, code, label, properties, sort_order)
				VALUES ($1::uuid, $2, $3, COALESCE($4::jsonb,'{}'::jsonb), $5)
				RETURNING id::text`,
				newID, m.Code, m.Label, []byte(m.Properties), m.SortOrder).Scan(&newMemberID); err != nil {
				return "", "", fmt.Errorf("member %q of %q: %w", m.Code, d.Name, err)
			}
			memberMap[m.ID] = newMemberID
		}
		for _, p := range d.TypedProperties {
			if _, err = tx.Exec(ctx,
				`INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid, $2, $3)`,
				newID, p.Name, p.DataType); err != nil {
				return "", "", fmt.Errorf("dimension property %q: %w", p.Name, err)
			}
		}
	}
	for _, d := range pkg.Dimensions {
		if d.ParentDimensionID != nil {
			if _, err = tx.Exec(ctx, `UPDATE model.dimension_def SET parent_dimension_id=$2::uuid WHERE id=$1::uuid`,
				dimMap[d.ID], remap(dimMap, d.ParentDimensionID)); err != nil {
				return "", "", fmt.Errorf("dimension parent of %q: %w", d.Name, err)
			}
		}
		if d.SourceDimensionID != nil {
			if _, err = tx.Exec(ctx, `UPDATE model.dimension_def SET source_dimension_id=$2::uuid WHERE id=$1::uuid`,
				dimMap[d.ID], remap(dimMap, d.SourceDimensionID)); err != nil {
				return "", "", fmt.Errorf("dimension source of %q: %w", d.Name, err)
			}
		}
		for _, m := range d.Members {
			if m.ParentMemberID == nil {
				continue
			}
			if _, err = tx.Exec(ctx, `UPDATE model.dimension_member SET parent_member_id=$2::uuid WHERE id=$1::uuid`,
				memberMap[m.ID], remap(memberMap, m.ParentMemberID)); err != nil {
				return "", "", fmt.Errorf("member parent of %q: %w", m.Code, err)
			}
		}
	}

	// Metrics + dependencies.
	metricMap := make(map[string]string, len(pkg.Metrics))
	for _, m := range pkg.Metrics {
		var newID string
		if err = tx.QueryRow(ctx, `
			INSERT INTO model.metric_def (model_id, revision_id, name, formula, storage_type, is_input, agg_rule, format, format_decimals, format_currency)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5::core.storage_type, $6, $7, $8, $9, $10)
			RETURNING id::text`,
			modelID, revisionID, m.Name, m.Formula, m.StorageType, m.IsInput, m.AggRule, m.Format, m.FormatDecimals, m.FormatCurrency).Scan(&newID); err != nil {
			return "", "", fmt.Errorf("metric %q: %w", m.Name, err)
		}
		metricMap[m.ID] = newID
	}
	for _, dep := range pkg.Dependencies {
		from, okFrom := metricMap[dep.MetricID]
		to, okTo := metricMap[dep.DependsOn]
		if !okFrom || !okTo {
			continue // dangling dependency in the package; skip rather than abort
		}
		if _, err = tx.Exec(ctx,
			`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`,
			from, to); err != nil {
			return "", "", fmt.Errorf("dependency: %w", err)
		}
	}

	// Grids (rollup ref remapped after), memberships.
	gridMap := make(map[string]string, len(pkg.Grids))
	for _, g := range pkg.Grids {
		var newID string
		if err = tx.QueryRow(ctx,
			`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, $3) RETURNING id::text`,
			modelID, revisionID, g.Name).Scan(&newID); err != nil {
			return "", "", fmt.Errorf("grid %q: %w", g.Name, err)
		}
		gridMap[g.ID] = newID
	}
	for _, g := range pkg.Grids {
		if g.RollupSourceGridID != nil {
			if _, err = tx.Exec(ctx, `UPDATE model.grid_def SET rollup_source_grid_id=$2::uuid WHERE id=$1::uuid`,
				gridMap[g.ID], remap(gridMap, g.RollupSourceGridID)); err != nil {
				return "", "", fmt.Errorf("grid rollup source of %q: %w", g.Name, err)
			}
		}
		for _, gm := range g.Metrics {
			mid, ok := metricMap[gm.MetricID]
			if !ok {
				continue
			}
			if _, err = tx.Exec(ctx,
				`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, $3)`,
				gridMap[g.ID], mid, gm.SortOrder); err != nil {
				return "", "", fmt.Errorf("grid metric: %w", err)
			}
		}
		for _, gd := range g.Dimensions {
			did, ok := dimMap[gd.DimensionID]
			if !ok {
				continue
			}
			if _, err = tx.Exec(ctx,
				`INSERT INTO model.grid_dimension (grid_id, dimension_id, display_level) VALUES ($1::uuid, $2::uuid, $3)`,
				gridMap[g.ID], did, gd.DisplayLevel); err != nil {
				return "", "", fmt.Errorf("grid dimension: %w", err)
			}
		}
	}

	// Forms (field refs remapped inline), records, mappings.
	formMap := make(map[string]string, len(pkg.Forms))
	for _, f := range pkg.Forms {
		fields := remapJSONArrayFields(f.Fields, "dimension_id", dimMap)
		fields = remapJSONArrayFields(fields, "metric_id", metricMap)
		var newID string
		if err = tx.QueryRow(ctx, `
			INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
			VALUES ($1::uuid, $2::uuid, $3, $4, COALESCE($5::jsonb,'[]'::jsonb))
			RETURNING id::text`,
			modelID, revisionID, f.Name, f.Label, []byte(fields)).Scan(&newID); err != nil {
			return "", "", fmt.Errorf("form %q: %w", f.Name, err)
		}
		formMap[f.ID] = newID
	}
	for _, rec := range pkg.FormRecords {
		fid, ok := formMap[rec.FormID]
		if !ok {
			continue
		}
		// created_by must reference a local user; the original author does
		// not exist in this database, so records are attributed to the importer.
		if _, err = tx.Exec(ctx, `
			INSERT INTO runtime.form_record (form_id, data, status, created_by)
			VALUES ($1::uuid, COALESCE($2::jsonb,'{}'::jsonb), $3::runtime.record_status, $4::uuid)`,
			fid, []byte(rec.Data), rec.Status, importerID); err != nil {
			return "", "", fmt.Errorf("form record: %w", err)
		}
	}
	// mappingMap correlates each exported mapping's original ID to its
	// freshly created one, so the facts loop below can remap
	// Fact.SourceMappingID into a valid source_ref in this model.
	mappingMap := make(map[string]string, len(pkg.FormMappings))
	for _, m := range pkg.FormMappings {
		fid, ok := formMap[m.FormID]
		if !ok {
			continue
		}
		mid, ok := metricMap[m.TargetMetricID]
		if !ok {
			continue
		}
		var newMappingID string
		if err = tx.QueryRow(ctx, `
			INSERT INTO model.form_metric_mapping
			  (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id,
			   aggregation, posting_statuses, dimension_mappings, live_posting)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7::uuid, $8, $9, COALESCE($10::jsonb,'{}'::jsonb), $11)
			RETURNING id::text`,
			modelID, revisionID, fid, remap(gridMap, m.GridID), m.Name, m.SourceField, mid,
			m.Aggregation, m.PostingStatuses, []byte(remapJSONKeys(m.DimensionMappings, dimMap)), m.LivePosting,
		).Scan(&newMappingID); err != nil {
			return "", "", fmt.Errorf("form mapping %q: %w", m.Name, err)
		}
		if m.ID != "" {
			mappingMap[m.ID] = newMappingID
		}
	}

	// Folders (parents remapped after), dashboards, widgets.
	folderMap := make(map[string]string, len(pkg.Folders))
	for _, f := range pkg.Folders {
		var newID string
		if err = tx.QueryRow(ctx,
			`INSERT INTO model.dashboard_folder (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, $3) RETURNING id::text`,
			modelID, revisionID, f.Name).Scan(&newID); err != nil {
			return "", "", fmt.Errorf("folder %q: %w", f.Name, err)
		}
		folderMap[f.ID] = newID
	}
	for _, f := range pkg.Folders {
		if f.ParentID == nil {
			continue
		}
		if _, err = tx.Exec(ctx, `UPDATE model.dashboard_folder SET parent_id=$2::uuid WHERE id=$1::uuid`,
			folderMap[f.ID], remap(folderMap, f.ParentID)); err != nil {
			return "", "", fmt.Errorf("folder parent of %q: %w", f.Name, err)
		}
	}

	dashMap := make(map[string]string, len(pkg.Dashboards))
	for _, d := range pkg.Dashboards {
		var folderID *string
		if d.FolderID != nil {
			if fid, ok := folderMap[*d.FolderID]; ok {
				folderID = &fid
			}
		}
		var newID string
		if err = tx.QueryRow(ctx, `
			INSERT INTO model.dashboard_def (model_id, revision_id, name, tags, category, folder_id)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6::uuid)
			RETURNING id::text`,
			modelID, revisionID, d.Name, d.Tags, d.Category, folderID).Scan(&newID); err != nil {
			return "", "", fmt.Errorf("dashboard %q: %w", d.Name, err)
		}
		dashMap[d.ID] = newID
	}

	// Workflows before widgets/automation so widget/rule refs can resolve.
	wfMap := make(map[string]string, len(pkg.Workflows))
	var appID string
	if err = tx.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID); err != nil {
		return "", "", err
	}
	for _, wf := range pkg.Workflows {
		schema := remapJSONArrayFields(wf.ContextSchema, "dimension_id", dimMap)
		// subject_config binds the workflow to a form ({"form_id"}) or a
		// grid metric ({"grid_id","metric_id"}) — those are this package's
		// source-model IDs and must land on the freshly created objects, or
		// the imported workflow keeps pointing at another tenant's form.
		subject := remapJSONObjectFields(wf.SubjectConfig, map[string]map[string]string{
			"form_id": formMap, "grid_id": gridMap, "metric_id": metricMap,
		})
		status := wf.Status
		if status == "" {
			status = "draft"
		}
		var newID string
		if err = tx.QueryRow(ctx, `
			INSERT INTO workflow.workflow_def
			  (application_id, revision_id, name, description, trigger_event, subject_type, subject_config,
			   steps, context_schema, status, created_by, updated_by)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, COALESCE($7::jsonb,'{}'::jsonb),
			        COALESCE($8::jsonb,'[]'::jsonb), COALESCE($9::jsonb,'[]'::jsonb), $10, $11::uuid, $11::uuid)
			RETURNING id::text`,
			appID, revisionID, wf.Name, wf.Description, wf.TriggerEvent, wf.SubjectType, []byte(subject),
			[]byte(wf.Steps), []byte(schema), status, importerID).Scan(&newID); err != nil {
			return "", "", fmt.Errorf("workflow %q: %w", wf.Name, err)
		}
		wfMap[wf.ID] = newID
	}

	// ruleMap / integrationMap: dashboard widgets of type automation_button
	// and integration_button reference these by ref_id; without the maps an
	// imported button kept the SOURCE tenant's rule/integration ID (found
	// live, 2026-09-10 — the Regional Expense Planning dashboard's
	// "Auto-start approval" button).
	ruleMap := make(map[string]string, len(pkg.AutomationRules))
	for _, ar := range pkg.AutomationRules {
		var wfID *string
		if ar.WorkflowDefID != nil {
			if id, ok := wfMap[*ar.WorkflowDefID]; ok {
				wfID = &id
			}
		}
		var formID, gridID *string
		if ar.SourceFormID != nil {
			if id, ok := formMap[*ar.SourceFormID]; ok {
				formID = &id
			}
		}
		if ar.SourceGridID != nil {
			if id, ok := gridMap[*ar.SourceGridID]; ok {
				gridID = &id
			}
		}
		var newRuleID string
		if err = tx.QueryRow(ctx, `
			INSERT INTO workflow.automation_rule
			  (application_id, revision_id, name, description, trigger_type, workflow_name, enabled,
			   workflow_def_id, source_form_id, source_grid_id)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5::workflow.trigger_type, $6, $7, $8::uuid, $9::uuid, $10::uuid)
			RETURNING id::text`,
			appID, revisionID, ar.Name, ar.Description, ar.TriggerType, ar.WorkflowName, ar.Enabled,
			wfID, formID, gridID).Scan(&newRuleID); err != nil {
			return "", "", fmt.Errorf("automation rule %q: %w", ar.Name, err)
		}
		if ar.ID != "" {
			ruleMap[ar.ID] = newRuleID
		}
	}

	// Integrations (after grids/dashboards/forms so targets resolve, and
	// before widgets so integration_button refs can).
	integrationMap := make(map[string]string, len(pkg.Integrations))
	for _, ig := range pkg.Integrations {
		target := ig.TargetID
		switch ig.TargetType {
		case "grid":
			target = remap(gridMap, target)
		case "dashboard":
			target = remap(dashMap, target)
		case "form":
			target = remap(formMap, target)
		}
		var newIntegrationID string
		if err = tx.QueryRow(ctx, `
			INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, target_id, config)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6::uuid, COALESCE($7::jsonb,'{}'::jsonb))
			RETURNING id::text`,
			modelID, revisionID, ig.Name, ig.Type, ig.TargetType, target, []byte(ig.Config)).Scan(&newIntegrationID); err != nil {
			return "", "", fmt.Errorf("integration %q: %w", ig.Name, err)
		}
		if ig.ID != "" {
			integrationMap[ig.ID] = newIntegrationID
		}
	}

	// Widgets last: ref_id may point at a grid, form, dashboard, workflow,
	// automation rule or integration — remap opportunistically through
	// every map.
	refMaps := []map[string]string{gridMap, formMap, dashMap, wfMap, ruleMap, integrationMap, metricMap, dimMap}
	for _, d := range pkg.Dashboards {
		for _, wd := range d.Widgets {
			refID := wd.RefID
			if refID != nil {
				for _, m := range refMaps {
					if nid, ok := m[*refID]; ok {
						refID = &nid
						break
					}
				}
			}
			// widget_props carries its own metric/dimension IDs (chart series
			// and plotted dimension, kpi_scope) — an import always allocates
			// new IDs for those, so copying the blob verbatim guaranteed a
			// dead chart in every imported model. See RemapWidgetPropsIDs.
			props, _ := RemapWidgetPropsIDs([]byte(wd.Props), metricMap, dimMap)
			if _, err = tx.Exec(ctx, `
				INSERT INTO model.dashboard_widget
				  (dashboard_id, widget_type, ref_id, content, sort_order, col_start, col_span,
				   pos_x, pos_y, size_w, size_h, title, show_title, widget_props)
				VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
				dashMap[d.ID], wd.WidgetType, refID, wd.Content, wd.SortOrder, wd.ColStart, wd.ColSpan,
				wd.PosX, wd.PosY, wd.SizeW, wd.SizeH, wd.Title, wd.ShowTitle, props); err != nil {
				return "", "", fmt.Errorf("widget on %q: %w", d.Name, err)
			}
		}
	}

	// Facts: dim_members keys are the source dimension IDs. A fact exported
	// with a SourceMappingID must import with a resolved source_ref (not
	// NULL, and not the stale original-model mapping ID) — dropping it here
	// rather than importing a dangling reference, matching the "skip what
	// doesn't resolve" convention used throughout this function.
	for _, f := range pkg.Facts {
		mid, ok := metricMap[f.MetricID]
		if !ok {
			continue
		}
		var sourceRef *string
		if f.SourceMappingID != nil {
			newMappingID, ok := mappingMap[*f.SourceMappingID]
			if !ok {
				continue
			}
			sourceRef = &newMappingID
		}
		if _, err = tx.Exec(ctx, `
			INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by, source_ref)
			VALUES ($1::uuid, $2::uuid, $3, $4::uuid, COALESCE($5::jsonb,'{}'::jsonb), $6, $7::uuid, $8::uuid)`,
			modelID, revisionID, revisionName, mid,
			[]byte(remapJSONKeys(f.DimMembers, dimMap)), f.Value, importerID, sourceRef); err != nil {
			return "", "", fmt.Errorf("fact: %w", err)
		}
	}

	// A freshly imported model needs an active revision to be usable.
	if _, err = tx.Exec(ctx, `
		UPDATE core.model SET active_revision_id=$2::uuid, active_revision_name=$3
		WHERE id=$1::uuid`,
		modelID, revisionID, revisionName); err != nil {
		return "", "", err
	}

	return modelID, revisionID, nil
}
