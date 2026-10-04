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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/timedim"
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
	ID string `json:"id"`
	// LineageID is the identity this row shares with its copies in every
	// revision (migration 099) — what access rules resolve by. Kept on
	// import so an imported revision lines up with the source model's
	// other revisions; absent in packages exported before it existed.
	LineageID      string          `json:"lineage_id,omitempty"`
	Code           string          `json:"code"`
	Label          string          `json:"label"`
	ParentMemberID *string         `json:"parent_member_id,omitempty"`
	Properties     json.RawMessage `json:"properties,omitempty"`
	SortOrder      int             `json:"sort_order"`
	// Time members: the period (YYYY-MM-DD) and chronological ordinal.
	PeriodStart *string `json:"period_start,omitempty"`
	PeriodEnd   *string `json:"period_end,omitempty"`
	TimeIndex   *int    `json:"time_index,omitempty"`
	// Formula marks a calculated member ({RF} - {LY}); it names member
	// codes, which copy as they are.
	Formula string `json:"formula,omitempty"`
}

type DimProperty struct {
	Name     string `json:"name"`
	DataType string `json:"data_type"`
}

type Dimension struct {
	ID string `json:"id"`
	// LineageID is the identity this row shares with its copies in every
	// revision (migration 099) — what access rules resolve by. Kept on
	// import so an imported revision lines up with the source model's
	// other revisions; absent in packages exported before it existed.
	LineageID         string          `json:"lineage_id,omitempty"`
	Name              string          `json:"name"`
	AggRule           string          `json:"agg_rule"`
	Properties        json.RawMessage `json:"properties,omitempty"`
	ParentDimensionID *string         `json:"parent_dimension_id,omitempty"`
	SourceDimensionID *string         `json:"source_dimension_id,omitempty"`
	SourceProperty    *string         `json:"source_property,omitempty"`
	// Time marker (spec §3.1). Empty DimensionType reads as "standard" so
	// packages exported before time dimensions existed import unchanged.
	DimensionType   string        `json:"dimension_type,omitempty"`
	TimeGranularity *string       `json:"time_granularity,omitempty"`
	FiscalYearStart *int          `json:"fiscal_year_start_month,omitempty"`
	Tags            []string      `json:"tags,omitempty"`
	Members         []Member      `json:"members"`
	TypedProperties []DimProperty `json:"typed_properties,omitempty"`
}

type Metric struct {
	ID string `json:"id"`
	// LineageID is the identity this row shares with its copies in every
	// revision (migration 099) — what access rules resolve by. Kept on
	// import so an imported revision lines up with the source model's
	// other revisions; absent in packages exported before it existed.
	LineageID      string   `json:"lineage_id,omitempty"`
	Name           string   `json:"name"`
	Label          string   `json:"label,omitempty"` // display label (migration 108); absent = derived from the name
	Formula        *string  `json:"formula,omitempty"`
	StorageType    string   `json:"storage_type"`
	IsInput        bool     `json:"is_input"`
	AggRule        string   `json:"agg_rule"`
	Format         string   `json:"format"`
	FormatDecimals int      `json:"format_decimals"`
	FormatCurrency string   `json:"format_currency"`
	TimeSummary    string   `json:"time_summary,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	// AggNumeratorMetricID and AggDenominatorMetricID are the operands of an
	// agg_rule "rate" (the total is numerator ÷ denominator), as package
	// metric IDs remapped on import. Absent in packages exported before
	// they were carried.
	AggNumeratorMetricID   *string `json:"agg_numerator_metric_id,omitempty"`
	AggDenominatorMetricID *string `json:"agg_denominator_metric_id,omitempty"`
}

type Dependency struct {
	MetricID  string `json:"metric_id"`
	DependsOn string `json:"depends_on_metric_id"`
	// Time offsets the dependency is read at (spec §3.4).
	MinTimeOffset   int  `json:"min_time_offset,omitempty"`
	MaxTimeOffset   int  `json:"max_time_offset,omitempty"`
	UnboundedPast   bool `json:"unbounded_past,omitempty"`
	UnboundedFuture bool `json:"unbounded_future,omitempty"`
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
	// LivePosting is a pointer so a package that leaves it out gets the
	// column default (true, as the console creates a mapping) rather than
	// Go's false. Export always sets it.
	LivePosting *bool `json:"live_posting,omitempty"`
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
	ID           string `json:"id,omitempty"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	TriggerType  string `json:"trigger_type"`
	WorkflowName string `json:"workflow_name"`
	// Enabled is a pointer so a package that leaves it out gets the column
	// default (true, as the console creates a rule) rather than Go's false.
	// Export always sets it.
	Enabled       *bool   `json:"enabled,omitempty"`
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
// older workflow.Store.CreateWorkflowDef) predate revision scoping. What
// those rows name in the model's other revisions is pointed at this
// revision's copies before the package is returned (resolveSiblingRefs).
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
		SELECT id::text, name, agg_rule, properties, parent_dimension_id::text, source_dimension_id::text, source_property,
		       dimension_type, time_granularity, fiscal_year_start_month, tags, lineage_id::text
		FROM model.dimension_def WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL) ORDER BY created_at`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d Dimension
		if err := rows.Scan(&d.ID, &d.Name, &d.AggRule, &d.Properties, &d.ParentDimensionID, &d.SourceDimensionID, &d.SourceProperty,
			&d.DimensionType, &d.TimeGranularity, &d.FiscalYearStart, &d.Tags, &d.LineageID); err != nil {
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
			SELECT id::text, code, label, parent_member_id::text, properties, sort_order,
			       period_start::text, period_end::text, time_index, lineage_id::text, COALESCE(btrim(formula),'')
			FROM model.dimension_member WHERE dimension_id=$1::uuid ORDER BY time_index NULLS LAST, sort_order, code`,
			pkg.Dimensions[i].ID)
		if err != nil {
			return nil, err
		}
		for mrows.Next() {
			var m Member
			if err := mrows.Scan(&m.ID, &m.Code, &m.Label, &m.ParentMemberID, &m.Properties, &m.SortOrder, &m.PeriodStart, &m.PeriodEnd, &m.TimeIndex, &m.LineageID, &m.Formula); err != nil {
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
		       COALESCE(format,''), COALESCE(format_decimals,0), COALESCE(format_currency,''), time_summary, tags, lineage_id::text,
		       agg_numerator_metric_id::text, agg_denominator_metric_id::text, COALESCE(label,'')
		FROM model.metric_def WHERE model_id=$1::uuid AND (revision_id=$2::uuid OR revision_id IS NULL) ORDER BY created_at`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m Metric
		if err := rows.Scan(&m.ID, &m.Name, &m.Formula, &m.StorageType, &m.IsInput, &m.AggRule, &m.Format, &m.FormatDecimals, &m.FormatCurrency, &m.TimeSummary, &m.Tags, &m.LineageID,
			&m.AggNumeratorMetricID, &m.AggDenominatorMetricID, &m.Label); err != nil {
			rows.Close()
			return nil, err
		}
		pkg.Metrics = append(pkg.Metrics, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Ordered by name, here and for grid dimensions and form mappings below,
	// so two exports of one revision are the same file: a shared package then
	// diffs cleanly, and examples/ can be checked against a fresh export.
	rows, err = q.Query(ctx, `
		SELECT cd.metric_id::text, cd.depends_on_metric_id::text,
		       cd.min_time_offset, cd.max_time_offset, cd.unbounded_past, cd.unbounded_future
		FROM model.calc_dependency cd
		JOIN model.metric_def m ON m.id = cd.metric_id
		JOIN model.metric_def dm ON dm.id = cd.depends_on_metric_id
		WHERE m.model_id=$1::uuid AND (m.revision_id=$2::uuid OR m.revision_id IS NULL)
		ORDER BY m.name, dm.name, cd.min_time_offset NULLS FIRST, cd.max_time_offset NULLS FIRST`,
		modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d Dependency
		if err := rows.Scan(&d.MetricID, &d.DependsOn, &d.MinTimeOffset, &d.MaxTimeOffset, &d.UnboundedPast, &d.UnboundedFuture); err != nil {
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
			`SELECT gd.dimension_id::text, gd.display_level FROM model.grid_dimension gd
			 JOIN model.dimension_def d ON d.id = gd.dimension_id
			 WHERE gd.grid_id=$1::uuid ORDER BY d.name`,
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
		WHERE fd.model_id=$1::uuid AND (fd.revision_id=$2::uuid OR fd.revision_id IS NULL)
		ORDER BY fd.name, fmm.name`,
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

	if err := resolveSiblingRefs(ctx, q, pkg, modelID, revisionID); err != nil {
		return nil, err
	}
	return pkg, nil
}

// ── Import ────────────────────────────────────────────────────────────────────

// importLineages decides the lineage_id each imported dimension, member and
// metric gets, as a function from the package's lineage to the one to
// store (nil = generate a fresh one). A package lineage is kept, so an
// imported revision still lines up with its source model's revisions — the
// identity access rules resolve by (migration 099). Two exceptions get a
// fresh lineage instead:
//
//   - a missing or malformed lineage (a package from before migration 099,
//     or a hand-edited one);
//   - a lineage some row in this database already carries. Import always
//     creates a NEW model, so such a row belongs to another model — most
//     often the very model the package was exported from, re-imported as a
//     copy in the same tenant. A lineage is one model's identity: shared
//     across models, a rule on one model's member would also restrict its
//     twin in the other.
//
// A re-minted lineage is re-minted consistently: every row of the package
// that carried it gets the same new value.
func importLineages(ctx context.Context, tx pgx.Tx, pkg *Package) (func(string) *string, error) {
	var ids []string
	for _, d := range pkg.Dimensions {
		ids = append(ids, d.LineageID)
		for _, m := range d.Members {
			ids = append(ids, m.LineageID)
		}
	}
	for _, m := range pkg.Metrics {
		ids = append(ids, m.LineageID)
	}
	keep := map[string]string{}
	var valid []string
	for _, id := range ids {
		u, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		if _, seen := keep[id]; !seen {
			keep[id] = u.String()
			valid = append(valid, u.String())
		}
	}
	if len(valid) > 0 {
		rows, err := tx.Query(ctx, `
			SELECT lineage_id::text FROM model.dimension_def WHERE lineage_id = ANY($1::uuid[])
			UNION SELECT lineage_id::text FROM model.dimension_member WHERE lineage_id = ANY($1::uuid[])
			UNION SELECT lineage_id::text FROM model.metric_def WHERE lineage_id = ANY($1::uuid[])`, valid)
		if err != nil {
			return nil, fmt.Errorf("check package lineages: %w", err)
		}
		taken := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("check package lineages: %w", err)
			}
			taken[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("check package lineages: %w", err)
		}
		for raw, canon := range keep {
			if taken[canon] {
				keep[raw] = uuid.NewString()
			}
		}
	}
	return func(pkgLineage string) *string {
		if v, ok := keep[pkgLineage]; ok {
			return &v
		}
		return nil
	}, nil
}

type ImportRequest struct {
	ApplicationID string  `json:"application_id"`
	ModelName     string  `json:"model_name,omitempty"`    // override; defaults to package's
	RevisionName  string  `json:"revision_name,omitempty"` // override; defaults to package's
	Package       Package `json:"package"`
}

// ── References ────────────────────────────────────────────────────────────────
//
// A package's rows name each other by the IDs they had where it was
// exported, and Import gives every row a new one, so every reference is
// resolved through the package — to the row created for the ID it names.
// What a reference must never do is keep the ID it came with. That ID is a
// row of the model the package was exported from (imported into the same
// database) or of anybody's model (a crafted package), and the engine reads
// through several of these references without asking which model the row
// is in — a chart widget's grid, an integration's target — so an ID passed
// through reads or writes rows of another model, or another tenant.
//
// Two rules, by where the reference is stored:
//
//   - A foreign-key column (a dimension's parent or source dimension, a
//     member's parent, a grid's rollup source, a mapping's grid, a folder's
//     parent, a rate metric's operands) must name a row of the package, or
//     the import is refused naming the field and the ID (packageRef). Every
//     copy path keeps such a column inside its revision, so an export of a
//     revision always satisfies it.
//   - A reference held without a foreign key — a widget's ref_id, an
//     integration's target, and the reference positions of the JSON
//     documents (jsonRefs) — is resolved through the package, and one the
//     package does not have is dropped (importRefs): the column is left
//     NULL, the JSON field or entry removed. It is never refused, because
//     the engine leaves such references behind itself — deleting a row
//     rewrites none of the forms, widgets or facts that used it, and a
//     workflow shared by every revision names the rows of the one it was
//     made in — so ordinary exports carry them. Nor is it asked whether the
//     ID names a row elsewhere in the database: the answer would tell an
//     importer that a row of another tenant exists. A dropped reference
//     reads and writes nothing, which is the most the ID it replaces could
//     rightly have done. The exception is a dimension key of a fact or a
//     mapping, part of the cell a value belongs to: it becomes a fresh ID
//     naming no row (one per unresolved ID, shared by every fact and mapping
//     of the import), so values in different cells stay in different cells.
//
// Before a revision is exported, references its rows make to the same
// model's other revisions are pointed at this revision's copy of the row
// they name (resolveSiblingRefs), so what a revision-wide workflow or an
// older copy of a revision holds travels instead of being dropped.
//
// References that already fall away when unresolved — a dashboard's folder,
// an automation rule's workflow/form/grid, a grid's metrics and dimensions,
// dependencies, a mapping's form and metric, a fact's metric and posting
// mapping — keep doing so: they never pass an ID through.

// packageRef resolves a foreign-key reference, which must name a row of the
// same package: absent is "", and an ID the package does not contain is an
// error rather than passed through, since the original ID would point at a
// row of the model the package was exported from (or at nothing). Whether
// such a row exists is not looked up, so the error says nothing about it.
func packageRef(m map[string]string, old *string) (string, error) {
	if old == nil || *old == "" {
		return "", nil
	}
	if n, ok := m[*old]; ok {
		return n, nil
	}
	return "", fmt.Errorf("%q is not in the package", *old)
}

// optionalPackageRef is packageRef for a nullable column: absent stays NULL.
func optionalPackageRef(m map[string]string, old *string) (*string, error) {
	n, err := packageRef(m, old)
	if err != nil || n == "" {
		return nil, err
	}
	return &n, nil
}

// rowIDOf returns s in canonical UUID form if it spells a UUID in any form
// Postgres or Go reads as one (upper case, braces, hyphens anywhere or none,
// a urn:uuid: prefix). A reference is checked in that form because it is
// read wherever it ends up by a ::uuid cast, which accepts all of them.
func rowIDOf(s string) (string, bool) {
	t := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "urn:uuid:")
	hex := make([]byte, 0, 32)
	for i := 0; i < len(t); i++ {
		switch c := t[i]; {
		case c == '{' || c == '}' || c == '-':
		case '0' <= c && c <= '9' || 'a' <= c && c <= 'f':
			hex = append(hex, c)
		default:
			return "", false
		}
	}
	if len(hex) != 32 {
		return "", false
	}
	h := string(hex)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], true
}

// jsonRefs says which positions of a JSON document hold references. Only
// those are resolved; every other string is data and is stored as written,
// even one that spells a UUID or a package ID — a member code, an allowed
// member, a connector's header, query or body, a lookup table. The
// positions, at any depth unless topOnly:
//
//   - a string field whose name ends in _id (dimension_id, x_metric_id,
//     form_id, target_id, target_revision_id …);
//   - the strings of an array field whose name ends in _ids (metric_ids);
//   - the keys of an object field named in dimensionKeyedObjects;
//   - the strings of a saved grid layout's axes (layoutAxes);
//   - with keys, the document's own top-level keys.
//
// A field holding a reference under any other name is not recognised, so a
// new one is named by these conventions or added here.
type jsonRefs struct {
	// keys: the top-level keys are dimension IDs and the values data (a
	// fact's dim_members, a mapping's dimension_mappings).
	keys bool
	// topOnly: only the top level's fields are the model's; everything
	// nested is the external system's (an integration's config: request,
	// auth, response, mapping).
	topOnly bool
}

// dimensionKeyedObjects are object fields keyed by dimension ID whose values
// are member codes: a chart's context_defaults, a grid layout's filter_sel.
var dimensionKeyedObjects = map[string]bool{"context_defaults": true, "filter_sel": true}

// layoutAxes are the axis arrays of a grid widget's default_view: dimension
// IDs in order, and the "__metrics__" sentinel.
var layoutAxes = map[string]bool{"rows": true, "cols": true, "context": true}

// refFunc decides one reference: what it becomes, or keep=false to drop it.
// kind is the kind of row the position names (refKind).
type refFunc func(kind, id string) (to string, keep bool)

// refKind is the kind of row a field named key refers to: the word before
// _id or _ids — dimension_id → dimension, x_metric_id → metric,
// target_revision_id → revision, target_id → target.
func refKind(key string) string {
	k := strings.TrimSuffix(strings.TrimSuffix(key, "_ids"), "_id")
	if i := strings.LastIndexByte(k, '_'); i >= 0 {
		k = k[i+1:]
	}
	return k
}

// rewriteRefs passes every reference in raw (see jsonRefs) through fn. It
// reports whether anything changed; unchanged, the document comes back as
// raw itself, formatting included. A document that is not JSON is returned
// as given.
func rewriteRefs(raw json.RawMessage, spec jsonRefs, fn refFunc) (json.RawMessage, bool) {
	if len(jsonArg(raw)) == 0 {
		return raw, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if dec.Decode(&doc) != nil {
		return raw, false
	}
	changed := false
	ref := func(kind, id string) (string, bool) {
		to, keep := fn(kind, id)
		if !keep || to != id {
			changed = true
		}
		return to, keep
	}
	// keysOf rebuilds a dimension-keyed object, so a rewritten key cannot
	// collide with one not yet visited; a dropped key takes its entry along.
	keysOf := func(obj map[string]any) map[string]any {
		out := make(map[string]any, len(obj))
		for k, v := range obj {
			if to, keep := ref("dimension", k); keep {
				out[to] = v
			}
		}
		return out
	}
	// strs rewrites an array's strings; a dropped one is removed and the
	// others keep their order (an axis order is the layout).
	strs := func(kind string, arr []any) []any {
		out := make([]any, 0, len(arr))
		for _, e := range arr {
			if s, ok := e.(string); ok {
				to, keep := ref(kind, s)
				if !keep {
					continue
				}
				e = to
			}
			out = append(out, e)
		}
		return out
	}
	var walkObj func(obj map[string]any, name string)
	var walkArr func(arr []any, name string)
	walkObj = func(obj map[string]any, name string) {
		for k, v := range obj {
			switch t := v.(type) {
			case string:
				if strings.HasSuffix(k, "_id") {
					if to, keep := ref(refKind(k), t); keep {
						obj[k] = to
					} else {
						delete(obj, k)
					}
				}
			case []any:
				switch {
				case strings.HasSuffix(k, "_ids"):
					obj[k] = strs(refKind(k), t)
				case name == "default_view" && layoutAxes[k]:
					obj[k] = strs("dimension", t)
				case !spec.topOnly:
					walkArr(t, k)
				}
			case map[string]any:
				if spec.topOnly {
					continue
				}
				if dimensionKeyedObjects[k] {
					t = keysOf(t)
					obj[k] = t
				}
				walkObj(t, k)
			}
		}
	}
	walkArr = func(arr []any, name string) {
		for _, e := range arr {
			switch t := e.(type) {
			case map[string]any:
				walkObj(t, name)
			case []any:
				walkArr(t, name)
			}
		}
	}
	switch t := doc.(type) {
	case map[string]any:
		if spec.keys {
			t = keysOf(t)
			doc = t
		}
		walkObj(t, "")
	case []any:
		walkArr(t, "")
	}
	if !changed {
		return raw, false
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return raw, false
	}
	return b, true
}

// looseRefs visits every reference of the package held without a foreign
// key (see References), plus an automation rule's references — which fall
// away when unresolved — through fn, and stores what it returns: a dropped
// column reference becomes nil. Import resolves the same positions as it
// creates the rows; this is the one list of them for everything else.
func (pkg *Package) looseRefs(fn refFunc) {
	col := func(kind string, p **string) {
		if *p == nil || **p == "" {
			return
		}
		if to, keep := fn(kind, **p); !keep {
			*p = nil
		} else if to != **p {
			*p = &to
		}
	}
	doc := func(p *json.RawMessage, spec jsonRefs) { *p, _ = rewriteRefs(*p, spec, fn) }
	for i := range pkg.Forms {
		doc(&pkg.Forms[i].Fields, jsonRefs{})
	}
	for i := range pkg.FormMappings {
		doc(&pkg.FormMappings[i].DimensionMappings, jsonRefs{keys: true})
	}
	for i := range pkg.Workflows {
		doc(&pkg.Workflows[i].SubjectConfig, jsonRefs{})
		doc(&pkg.Workflows[i].ContextSchema, jsonRefs{})
	}
	for i := range pkg.AutomationRules {
		ar := &pkg.AutomationRules[i]
		col("workflow", &ar.WorkflowDefID)
		col("form", &ar.SourceFormID)
		col("grid", &ar.SourceGridID)
	}
	for i := range pkg.Integrations {
		ig := &pkg.Integrations[i]
		column, config := ig.targetKinds()
		col(column, &ig.TargetID)
		ig.Config, _ = rewriteRefs(ig.Config, jsonRefs{topOnly: true}, targetRefs(fn, config))
	}
	for i := range pkg.Dashboards {
		for j := range pkg.Dashboards[i].Widgets {
			wd := &pkg.Dashboards[i].Widgets[j]
			col(widgetRefKinds[wd.WidgetType], &wd.RefID)
			doc(&wd.Props, jsonRefs{})
		}
	}
	for i := range pkg.Facts {
		doc(&pkg.Facts[i].DimMembers, jsonRefs{keys: true})
	}
}

// widgetRefKinds is the kind of row each widget type's ref_id names (the
// kinds the dashboard writer checks it against); a type not listed resolves
// through every kind.
var widgetRefKinds = map[string]string{
	"grid": "grid", "chart": "grid", "import": "grid", "form": "form",
	"metric_kpi": "metric", "integration_button": "integration", "automation_button": "rule",
}

// targetKinds is the kind of row an integration's target column names — its
// target_type, whose column default is grid — and the kind its config's
// target_id names: the config's own target_type when it has one.
func (ig *Integration) targetKinds() (column, config string) {
	column = ig.TargetType
	if column == "" {
		column = "grid"
	}
	var head struct {
		TargetType string `json:"target_type"`
	}
	if json.Unmarshal(ig.Config, &head) == nil && head.TargetType != "" {
		return column, head.TargetType
	}
	return column, column
}

// targetRefs is fn for a document whose target_id names a row of kind.
func targetRefs(fn refFunc, kind string) refFunc {
	return func(k, id string) (string, bool) {
		if k == "target" {
			k = kind
		}
		return fn(k, id)
	}
}

// importRefs resolves an import's references held without a foreign key
// (see References): through the package, or dropped. It asks the database
// nothing.
type importRefs struct {
	// byKind is each kind's package-ID → new-ID map (by refKind's words);
	// all is every map of the import. A reference is resolved through its
	// own kind's map first and then through all of them, so what it lands
	// on is always a row this import created. The maps fill as the rows are
	// created, so a reference resolves once its row exists.
	byKind map[string]map[string]string
	all    []map[string]string
	// cells holds the fresh ID each unresolved dimension key of a cell
	// becomes (see cellDoc).
	cells map[string]string
}

func newImportRefs() *importRefs {
	return &importRefs{byKind: map[string]map[string]string{}, cells: map[string]string{}}
}

// add registers the map of one kind of row.
func (r *importRefs) add(kind string, m map[string]string) {
	r.byKind[kind] = m
	r.all = append(r.all, m)
}

// ref is the refFunc of an import. A string that spells no UUID and is not
// a package ID can name no row — every reader casts a reference to ::uuid —
// and is kept as written: the layout sentinel "__metrics__", or a
// hand-written package's dangling ID.
func (r *importRefs) ref(kind, id string) (string, bool) {
	if id == "" {
		return id, true
	}
	if n, ok := r.byKind[kind][id]; ok {
		return n, true
	}
	for _, m := range r.all {
		if n, ok := m[id]; ok {
			return n, true
		}
	}
	if _, ok := rowIDOf(id); !ok {
		return id, true
	}
	return "", false
}

// column resolves a nullable reference column: a dropped reference is NULL.
func (r *importRefs) column(kind string, id *string) *string {
	if id == nil {
		return nil
	}
	n, keep := r.ref(kind, *id)
	if !keep {
		return nil
	}
	return &n
}

// doc resolves the references of a JSON document.
func (r *importRefs) doc(raw json.RawMessage, spec jsonRefs) json.RawMessage {
	out, _ := rewriteRefs(raw, spec, r.ref)
	return out
}

// cellDoc resolves a document keyed by the dimensions of a cell (a fact's
// dim_members, a mapping's dimension_mappings). A dimension key the package
// does not have becomes a fresh ID naming no row instead of being dropped —
// the same one wherever the import meets that key — so two values that
// differed only in it stay two values, and what a mapping posts lands in
// the cells of the facts imported beside it.
func (r *importRefs) cellDoc(raw json.RawMessage) json.RawMessage {
	out, _ := rewriteRefs(raw, jsonRefs{keys: true}, func(kind, id string) (string, bool) {
		if n, keep := r.ref(kind, id); keep || kind != "dimension" {
			return n, keep
		}
		n, ok := r.cells[id]
		if !ok {
			n = uuid.NewString()
			r.cells[id] = n
		}
		return n, true
	})
	return out
}

// ── Stale references on export ────────────────────────────────────────────────

// siblingRefSQL finds, for each ID in $1 naming a row of model $2 in a
// revision other than $3, the row of revision $3 that is its copy: the same
// lineage for a dimension, member or metric, the same name for anything
// else (what duplicateRevision matches by). Only model $2 is searched.
const siblingRefSQL = `
	WITH ids AS (SELECT unnest($1::uuid[]) AS id),
	     revs AS (SELECT id FROM model.revision WHERE model_id = $2::uuid AND id <> $3::uuid)
	          SELECT s.id::text, p.id::text FROM model.dimension_def s
	            JOIN model.dimension_def p ON p.model_id = s.model_id AND p.lineage_id = s.lineage_id AND p.revision_id = $3::uuid
	           WHERE s.id IN (SELECT id FROM ids) AND s.model_id = $2::uuid AND s.revision_id IN (SELECT id FROM revs)
	UNION ALL SELECT s.id::text, p.id::text FROM model.dimension_member s
	            JOIN model.dimension_def sd ON sd.id = s.dimension_id
	            JOIN model.dimension_member p ON p.lineage_id = s.lineage_id
	            JOIN model.dimension_def pd ON pd.id = p.dimension_id AND pd.model_id = sd.model_id AND pd.revision_id = $3::uuid
	           WHERE s.id IN (SELECT id FROM ids) AND sd.model_id = $2::uuid AND sd.revision_id IN (SELECT id FROM revs)
	UNION ALL SELECT s.id::text, p.id::text FROM model.metric_def s
	            JOIN model.metric_def p ON p.model_id = s.model_id AND p.lineage_id = s.lineage_id AND p.revision_id = $3::uuid
	           WHERE s.id IN (SELECT id FROM ids) AND s.model_id = $2::uuid AND s.revision_id IN (SELECT id FROM revs)
	UNION ALL SELECT s.id::text, p.id::text FROM model.grid_def s
	            JOIN model.grid_def p ON p.model_id = s.model_id AND p.name = s.name AND p.revision_id = $3::uuid
	           WHERE s.id IN (SELECT id FROM ids) AND s.model_id = $2::uuid AND s.revision_id IN (SELECT id FROM revs)
	UNION ALL SELECT s.id::text, p.id::text FROM model.form_def s
	            JOIN model.form_def p ON p.model_id = s.model_id AND p.name = s.name AND p.revision_id = $3::uuid
	           WHERE s.id IN (SELECT id FROM ids) AND s.model_id = $2::uuid AND s.revision_id IN (SELECT id FROM revs)
	UNION ALL SELECT s.id::text, p.id::text FROM model.dashboard_def s
	            JOIN model.dashboard_def p ON p.model_id = s.model_id AND p.name = s.name AND p.revision_id = $3::uuid
	           WHERE s.id IN (SELECT id FROM ids) AND s.model_id = $2::uuid AND s.revision_id IN (SELECT id FROM revs)
	UNION ALL SELECT s.id::text, p.id::text FROM model.integration_def s
	            JOIN model.integration_def p ON p.model_id = s.model_id AND p.name = s.name AND p.revision_id = $3::uuid
	           WHERE s.id IN (SELECT id FROM ids) AND s.model_id = $2::uuid AND s.revision_id IN (SELECT id FROM revs)
	UNION ALL SELECT s.id::text, p.id::text FROM workflow.workflow_def s
	            JOIN workflow.workflow_def p ON p.application_id = s.application_id AND p.name = s.name AND p.revision_id = $3::uuid
	           WHERE s.id IN (SELECT id FROM ids) AND s.revision_id IN (SELECT id FROM revs)
	UNION ALL SELECT s.id::text, p.id::text FROM workflow.automation_rule s
	            JOIN workflow.automation_rule p ON p.application_id = s.application_id AND p.name = s.name AND p.revision_id = $3::uuid
	           WHERE s.id IN (SELECT id FROM ids) AND s.revision_id IN (SELECT id FROM revs)`

// resolveSiblingRefs points the references pkg makes to rows of the same
// model's other revisions at this revision's copy of each row (siblingRefSQL).
// They are what a workflow or automation rule shared by every revision
// holds — it names the rows of the revision it was made in — and what a
// copy the engine made before it remapped a field still holds (a
// connector's config target, a grid widget's saved layout). Import would
// drop them; resolved, they travel. A reference without exactly one copy in
// this revision is left for Import to drop.
func resolveSiblingRefs(ctx context.Context, q Queryer, pkg *Package, modelID, revisionID string) error {
	own := pkg.rowIDs()
	seen := map[string]bool{}
	var outside []string
	pkg.looseRefs(func(_, id string) (string, bool) {
		if c, ok := rowIDOf(id); ok && !own[c] && !seen[c] {
			seen[c] = true
			outside = append(outside, c)
		}
		return id, true
	})
	if len(outside) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, siblingRefSQL, outside, modelID, revisionID)
	if err != nil {
		return fmt.Errorf("resolve references to other revisions: %w", err)
	}
	copies := map[string][]string{}
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			rows.Close()
			return fmt.Errorf("resolve references to other revisions: %w", err)
		}
		if own[to] {
			copies[from] = append(copies[from], to)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("resolve references to other revisions: %w", err)
	}
	if len(copies) == 0 {
		return nil
	}
	pkg.looseRefs(func(_, id string) (string, bool) {
		if c, ok := rowIDOf(id); ok && len(copies[c]) == 1 {
			return copies[c][0], true
		}
		return id, true
	})
	return nil
}

// rowIDs is the set of the package's own row IDs, in canonical form.
func (pkg *Package) rowIDs() map[string]bool {
	own := map[string]bool{}
	add := func(id string) {
		if c, ok := rowIDOf(id); ok {
			own[c] = true
		}
	}
	for _, d := range pkg.Dimensions {
		add(d.ID)
		for _, m := range d.Members {
			add(m.ID)
		}
	}
	for _, m := range pkg.Metrics {
		add(m.ID)
	}
	for _, g := range pkg.Grids {
		add(g.ID)
	}
	for _, f := range pkg.Forms {
		add(f.ID)
	}
	for _, m := range pkg.FormMappings {
		add(m.ID)
	}
	for _, f := range pkg.Folders {
		add(f.ID)
	}
	for _, d := range pkg.Dashboards {
		add(d.ID)
	}
	for _, ig := range pkg.Integrations {
		add(ig.ID)
	}
	for _, wf := range pkg.Workflows {
		add(wf.ID)
	}
	for _, ar := range pkg.AutomationRules {
		add(ar.ID)
	}
	return own
}

// A package is not only what CollectExport writes: a hand-made package, or
// one exported before a column existed, leaves fields out. Import treats an
// absent field the way the database treats an omitted column — it takes the
// column's default — because passing the Go zero value through does not:
// a nil slice or pointer is an explicit NULL (which a NOT NULL column
// rejects even when it has a default), and an empty string fails an enum
// cast or lands as a value no reader expects. The SQL below therefore wraps
// each such argument in COALESCE/NULLIF with the column's default, and
// TestImportMinimalPackage checks those literals against the live column
// defaults so the two cannot drift apart.

// jsonArg is a package JSON value as a query argument: absent, or an
// explicit JSON null, becomes SQL NULL so the COALESCE beside it supplies the
// column default rather than storing a JSON null where readers expect an
// object or an array.
func jsonArg(raw []byte) []byte {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil
	}
	return raw
}

// Canvas geometry for widgets whose package leaves it out, taken from
// migration 029 — the one that added the pixel columns and converted every
// existing widget from the old 12-column grid: about 100px a column, 200px
// tall, rows 220px apart.
const (
	legacyColumnPx = 100 // one column of the old 12-column grid
	legacyColumns  = 12  // col_span's column default: the full width
	widgetHeightPx = 200 // size_h's column default
	widgetRowGapPx = 20  // 220px row pitch less the 200px height
)

type widgetBox struct{ x, y, w, h int }

// legacyColumns is the widget's old 12-column placement with what the
// package left out filled in by the column defaults (col_start 1, col_span
// the full width) — the values stored and the ones its derived canvas box
// assumes, so the two agree.
func (w Widget) legacyColumns() (start, span int) {
	start, span = max(w.ColStart, 1), w.ColSpan
	if span <= 0 {
		span = legacyColumns
	}
	return start, span
}

// widgetGeometry returns every widget's canvas box, filling in what the
// package left out (pos_x, pos_y, size_w and size_h are NOT NULL, so an
// omitted one cannot simply be passed through). x and width come from the
// widget's legacy col_start/col_span the way migration 029 derived them, a
// missing col_span reading as its column default (the full width), and
// height is the column default. A widget without pos_y is stacked below
// everything else on the dashboard, in package order, instead of taking
// the column default of 0 — that would pile every such widget at the top,
// over each other and over the widgets the package did place.
func widgetGeometry(ws []Widget) []widgetBox {
	boxes := make([]widgetBox, len(ws))
	bottom, placed := 0, false
	for i, w := range ws {
		colStart, colSpan := w.legacyColumns()
		b := widgetBox{
			x: (colStart - 1) * legacyColumnPx,
			w: colSpan * legacyColumnPx,
			h: widgetHeightPx,
		}
		if w.PosX != nil {
			b.x = *w.PosX
		}
		if w.SizeW != nil {
			b.w = *w.SizeW
		}
		if w.SizeH != nil {
			b.h = *w.SizeH
		}
		if w.PosY != nil {
			b.y = *w.PosY
			bottom, placed = max(bottom, b.y+b.h), true
		}
		boxes[i] = b
	}
	for i, w := range ws {
		if w.PosY != nil {
			continue
		}
		if placed {
			boxes[i].y = bottom + widgetRowGapPx
		}
		bottom, placed = boxes[i].y+boxes[i].h, true
	}
	return boxes
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

	lineage, err := importLineages(ctx, tx, pkg)
	if err != nil {
		return "", "", err
	}

	// Dimensions (parents/sources remapped after all rows exist), members,
	// typed properties.
	dimMap := make(map[string]string, len(pkg.Dimensions))
	memberMap := map[string]string{}
	// Every map below joins refs as it is made (see importRefs).
	refs := newImportRefs()
	refs.add("dimension", dimMap)
	refs.add("member", memberMap)
	for _, d := range pkg.Dimensions {
		var newID string
		dimType := d.DimensionType
		if dimType == "" {
			dimType = timedim.TypeStandard
		}
		// The same rule every dimension writer applies, so a package that
		// marks a dimension as time but leaves out its granularity or
		// fiscal year start is told which, not handed a constraint name.
		timeCfg := timedim.Config{Type: dimType}
		if d.TimeGranularity != nil {
			timeCfg.Granularity = *d.TimeGranularity
		}
		if d.FiscalYearStart != nil {
			timeCfg.FiscalYearStartMonth = *d.FiscalYearStart
		}
		if err = timedim.ValidateConfig(&timeCfg); err != nil {
			return "", "", fmt.Errorf("dimension %q: %w", d.Name, err)
		}
		if err = tx.QueryRow(ctx, `
			INSERT INTO model.dimension_def (model_id, revision_id, name, agg_rule, properties, source_property,
			                                 dimension_type, time_granularity, fiscal_year_start_month, tags, lineage_id)
			VALUES ($1::uuid, $2::uuid, $3, COALESCE(NULLIF($4::text,''),'sum'), COALESCE($5::jsonb,'[]'::jsonb), $6, $7, $8, $9,
			        COALESCE($10::text[],'{}'), COALESCE($11::uuid, gen_random_uuid()))
			RETURNING id::text`,
			modelID, revisionID, d.Name, d.AggRule, jsonArg(d.Properties), d.SourceProperty,
			dimType, d.TimeGranularity, d.FiscalYearStart, d.Tags, lineage(d.LineageID)).Scan(&newID); err != nil {
			return "", "", fmt.Errorf("dimension %q: %w", d.Name, err)
		}
		dimMap[d.ID] = newID
		for i, m := range d.Members {
			// A dated member must carry an ordinal (period_start, period_end
			// and time_index are all set or all NULL). The package's own
			// ordinals are not trusted anyway — ValidateAndReindex below
			// renumbers every leaf — so a missing one only needs a
			// placeholder; the member's position keeps them distinct.
			timeIndex := m.TimeIndex
			if timeIndex == nil && m.PeriodStart != nil && m.PeriodEnd != nil {
				timeIndex = &i
			}
			var newMemberID string
			if err = tx.QueryRow(ctx, `
				INSERT INTO model.dimension_member (dimension_id, code, label, properties, sort_order, period_start, period_end, time_index, lineage_id, formula)
				VALUES ($1::uuid, $2, $3, COALESCE($4::jsonb,'{}'::jsonb), $5, $6::date, $7::date, $8, COALESCE($9::uuid, gen_random_uuid()), NULLIF($10,''))
				RETURNING id::text`,
				newID, m.Code, m.Label, jsonArg(m.Properties), m.SortOrder, m.PeriodStart, m.PeriodEnd, timeIndex,
				lineage(m.LineageID), m.Formula).Scan(&newMemberID); err != nil {
				return "", "", fmt.Errorf("member %q of %q: %w", m.Code, d.Name, err)
			}
			memberMap[m.ID] = newMemberID
		}

		for _, p := range d.TypedProperties {
			if _, err = tx.Exec(ctx,
				`INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid, $2, COALESCE(NULLIF($3::text,''),'text'))`,
				newID, p.Name, p.DataType); err != nil {
				return "", "", fmt.Errorf("dimension property %q: %w", p.Name, err)
			}
		}
	}
	for _, d := range pkg.Dimensions {
		parent, err := optionalPackageRef(dimMap, d.ParentDimensionID)
		if err != nil {
			return "", "", fmt.Errorf("dimension %q: parent_dimension_id %w", d.Name, err)
		}
		if parent != nil {
			if _, err = tx.Exec(ctx, `UPDATE model.dimension_def SET parent_dimension_id=$2::uuid WHERE id=$1::uuid`,
				dimMap[d.ID], parent); err != nil {
				return "", "", fmt.Errorf("dimension parent of %q: %w", d.Name, err)
			}
		}
		source, err := optionalPackageRef(dimMap, d.SourceDimensionID)
		if err != nil {
			return "", "", fmt.Errorf("dimension %q: source_dimension_id %w", d.Name, err)
		}
		if source != nil {
			if _, err = tx.Exec(ctx, `UPDATE model.dimension_def SET source_dimension_id=$2::uuid WHERE id=$1::uuid`,
				dimMap[d.ID], source); err != nil {
				return "", "", fmt.Errorf("dimension source of %q: %w", d.Name, err)
			}
		}
		for _, m := range d.Members {
			parent, err := optionalPackageRef(memberMap, m.ParentMemberID)
			if err != nil {
				return "", "", fmt.Errorf("member %q of %q: parent_member_id %w", m.Code, d.Name, err)
			}
			if parent == nil {
				continue
			}
			if _, err = tx.Exec(ctx, `UPDATE model.dimension_member SET parent_member_id=$2::uuid WHERE id=$1::uuid`,
				memberMap[m.ID], parent); err != nil {
				return "", "", fmt.Errorf("member parent of %q: %w", m.Code, err)
			}
		}
		// Re-validate and re-index (parents now in place) rather than trust
		// the package's ordinals: the same invariants hold whichever writer
		// produced the members. Every dimension, not only time ones — this
		// is also where a standard dimension is refused dated members,
		// which the time_index placeholder above would otherwise let in.
		if err = timedim.ValidateAndReindex(ctx, tx, dimMap[d.ID]); err != nil {
			return "", "", fmt.Errorf("dimension %q: %w", d.Name, err)
		}
	}

	// Metrics + dependencies.
	metricMap := make(map[string]string, len(pkg.Metrics))
	refs.add("metric", metricMap)
	for _, m := range pkg.Metrics {
		var newID string
		timeSummary := m.TimeSummary
		if timeSummary == "" {
			timeSummary = "sum"
		}
		if err = tx.QueryRow(ctx, `
			INSERT INTO model.metric_def (model_id, revision_id, name, formula, storage_type, is_input, agg_rule, format, format_decimals, format_currency, time_summary, tags, lineage_id, label)
			VALUES ($1::uuid, $2::uuid, $3, $4, COALESCE(NULLIF($5::text,''),'oltp')::core.storage_type, $6,
			        COALESCE(NULLIF($7::text,''),'sum'), COALESCE(NULLIF($8::text,''),'number'), $9,
			        COALESCE(NULLIF($10::text,''),'$'), $11,
			        COALESCE($12::text[],'{}'), COALESCE($13::uuid, gen_random_uuid()), NULLIF(btrim($14::text),''))
			RETURNING id::text`,
			modelID, revisionID, m.Name, m.Formula, m.StorageType, m.IsInput, m.AggRule, m.Format, m.FormatDecimals, m.FormatCurrency, timeSummary, m.Tags,
			lineage(m.LineageID), m.Label).Scan(&newID); err != nil {
			return "", "", fmt.Errorf("metric %q: %w", m.Name, err)
		}
		metricMap[m.ID] = newID
	}
	// Rate operands point at other metrics, so they are set once every
	// metric exists, then held to the rule every metric writer applies —
	// a "rate" with nothing to divide fails in the scheduler on every
	// recalculation instead of on save.
	for _, m := range pkg.Metrics {
		num, err := packageRef(metricMap, m.AggNumeratorMetricID)
		if err != nil {
			return "", "", fmt.Errorf("metric %q: numerator %w", m.Name, err)
		}
		den, err := packageRef(metricMap, m.AggDenominatorMetricID)
		if err != nil {
			return "", "", fmt.Errorf("metric %q: denominator %w", m.Name, err)
		}
		if err = metricformula.ValidateAggRule(m.AggRule, m.IsInput, num, den, metricMap[m.ID]); err != nil {
			return "", "", fmt.Errorf("metric %q: %w", m.Name, err)
		}
		if num == "" && den == "" {
			continue
		}
		if _, err = tx.Exec(ctx, `
			UPDATE model.metric_def SET agg_numerator_metric_id=NULLIF($2,'')::uuid, agg_denominator_metric_id=NULLIF($3,'')::uuid
			WHERE id=$1::uuid`, metricMap[m.ID], num, den); err != nil {
			return "", "", fmt.Errorf("metric %q operands: %w", m.Name, err)
		}
	}
	for _, dep := range pkg.Dependencies {
		from, okFrom := metricMap[dep.MetricID]
		to, okTo := metricMap[dep.DependsOn]
		if !okFrom || !okTo {
			continue // dangling dependency in the package; skip rather than abort
		}
		if _, err = tx.Exec(ctx, `
			INSERT INTO model.calc_dependency
			    (metric_id, depends_on_metric_id, min_time_offset, max_time_offset, unbounded_past, unbounded_future)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
			from, to, dep.MinTimeOffset, dep.MaxTimeOffset, dep.UnboundedPast, dep.UnboundedFuture); err != nil {
			return "", "", fmt.Errorf("dependency: %w", err)
		}
	}

	// Grids (rollup ref remapped after), memberships.
	gridMap := make(map[string]string, len(pkg.Grids))
	refs.add("grid", gridMap)
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
		rollup, err := optionalPackageRef(gridMap, g.RollupSourceGridID)
		if err != nil {
			return "", "", fmt.Errorf("grid %q: rollup_source_grid_id %w", g.Name, err)
		}
		if rollup != nil {
			if _, err = tx.Exec(ctx, `UPDATE model.grid_def SET rollup_source_grid_id=$2::uuid WHERE id=$1::uuid`,
				gridMap[g.ID], rollup); err != nil {
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
	refs.add("form", formMap)
	for _, f := range pkg.Forms {
		fields := refs.doc(f.Fields, jsonRefs{})
		var newID string
		if err = tx.QueryRow(ctx, `
			INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
			VALUES ($1::uuid, $2::uuid, $3, $4, COALESCE($5::jsonb,'[]'::jsonb))
			RETURNING id::text`,
			modelID, revisionID, f.Name, f.Label, jsonArg(fields)).Scan(&newID); err != nil {
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
			VALUES ($1::uuid, COALESCE($2::jsonb,'{}'::jsonb), COALESCE(NULLIF($3::text,''),'draft')::runtime.record_status, $4::uuid)`,
			fid, jsonArg(rec.Data), rec.Status, importerID); err != nil {
			return "", "", fmt.Errorf("form record: %w", err)
		}
	}
	// mappingMap correlates each exported mapping's original ID to its
	// freshly created one, so the facts loop below can remap
	// Fact.SourceMappingID into a valid source_ref in this model.
	mappingMap := make(map[string]string, len(pkg.FormMappings))
	refs.add("mapping", mappingMap)
	for _, m := range pkg.FormMappings {
		fid, ok := formMap[m.FormID]
		if !ok {
			continue
		}
		mid, ok := metricMap[m.TargetMetricID]
		if !ok {
			continue
		}
		gridID, err := optionalPackageRef(gridMap, m.GridID)
		if err != nil {
			return "", "", fmt.Errorf("form mapping %q: grid_id %w", m.Name, err)
		}
		dimMappings := refs.cellDoc(m.DimensionMappings)
		var newMappingID string
		if err = tx.QueryRow(ctx, `
			INSERT INTO model.form_metric_mapping
			  (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id,
			   aggregation, posting_statuses, dimension_mappings, live_posting)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7::uuid, COALESCE(NULLIF($8::text,''),'sum'),
			        COALESCE($9::text[],'{approved}'), COALESCE($10::jsonb,'{}'::jsonb), COALESCE($11::bool, true))
			RETURNING id::text`,
			modelID, revisionID, fid, gridID, m.Name, m.SourceField, mid,
			m.Aggregation, m.PostingStatuses, jsonArg(dimMappings), m.LivePosting,
		).Scan(&newMappingID); err != nil {
			return "", "", fmt.Errorf("form mapping %q: %w", m.Name, err)
		}
		if m.ID != "" {
			mappingMap[m.ID] = newMappingID
		}
	}

	// Folders (parents remapped after), dashboards, widgets.
	folderMap := make(map[string]string, len(pkg.Folders))
	refs.add("folder", folderMap)
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
		parent, err := optionalPackageRef(folderMap, f.ParentID)
		if err != nil {
			return "", "", fmt.Errorf("folder %q: parent_id %w", f.Name, err)
		}
		if parent == nil {
			continue
		}
		if _, err = tx.Exec(ctx, `UPDATE model.dashboard_folder SET parent_id=$2::uuid WHERE id=$1::uuid`,
			folderMap[f.ID], parent); err != nil {
			return "", "", fmt.Errorf("folder parent of %q: %w", f.Name, err)
		}
	}

	dashMap := make(map[string]string, len(pkg.Dashboards))
	refs.add("dashboard", dashMap)
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
			VALUES ($1::uuid, $2::uuid, $3, COALESCE($4::text[],'{}'), $5, $6::uuid)
			RETURNING id::text`,
			modelID, revisionID, d.Name, d.Tags, d.Category, folderID).Scan(&newID); err != nil {
			return "", "", fmt.Errorf("dashboard %q: %w", d.Name, err)
		}
		dashMap[d.ID] = newID
	}

	// Workflows before widgets/automation so widget/rule refs can resolve.
	wfMap := make(map[string]string, len(pkg.Workflows))
	refs.add("workflow", wfMap)
	var appID string
	if err = tx.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID); err != nil {
		return "", "", err
	}
	for _, wf := range pkg.Workflows {
		schema := refs.doc(wf.ContextSchema, jsonRefs{})
		// subject_config binds the workflow to a form ({"form_id"}) or a
		// grid metric ({"grid_id","metric_id"}) — those are this package's
		// source-model IDs and must land on the freshly created objects, or
		// the imported workflow keeps pointing at another tenant's form.
		subject := refs.doc(wf.SubjectConfig, jsonRefs{})
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
			appID, revisionID, wf.Name, wf.Description, wf.TriggerEvent, wf.SubjectType, jsonArg(subject),
			jsonArg(wf.Steps), jsonArg(schema), status, importerID).Scan(&newID); err != nil {
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
	refs.add("rule", ruleMap)
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
			VALUES ($1::uuid, $2::uuid, $3, $4, COALESCE(NULLIF($5::text,''),'manual')::workflow.trigger_type, $6, COALESCE($7::bool, true),
			        $8::uuid, $9::uuid, $10::uuid)
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
	refs.add("integration", integrationMap)
	for _, ig := range pkg.Integrations {
		// target_type decides which kind the target resolves through, so
		// an absent one takes its column default here rather than in SQL.
		targetType, configType := ig.targetKinds()
		target := refs.column(targetType, ig.TargetID)
		if target != nil && *target == "" {
			target = nil // no target, not an invalid uuid
		}
		// A connector's config names its target again, and that copy is the
		// one its runs read and write (integration.Config.TargetID). The
		// rest of the config is the external system's, left as written.
		config, _ := rewriteRefs(ig.Config, jsonRefs{topOnly: true}, targetRefs(refs.ref, configType))
		var newIntegrationID string
		if err = tx.QueryRow(ctx, `
			INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, target_id, config)
			VALUES ($1::uuid, $2::uuid, $3, COALESCE(NULLIF($4::text,''),'csv_import'), $5, $6::uuid, COALESCE($7::jsonb,'{}'::jsonb))
			RETURNING id::text`,
			modelID, revisionID, ig.Name, ig.Type, targetType, target, jsonArg(config)).Scan(&newIntegrationID); err != nil {
			return "", "", fmt.Errorf("integration %q: %w", ig.Name, err)
		}
		if ig.ID != "" {
			integrationMap[ig.ID] = newIntegrationID
		}
	}

	// Widgets last: ref_id may point at a grid, form, metric, integration
	// or automation rule (widgetRefKinds).
	for _, d := range pkg.Dashboards {
		boxes := widgetGeometry(d.Widgets)
		for i, wd := range d.Widgets {
			refID := refs.column(widgetRefKinds[wd.WidgetType], wd.RefID)
			// widget_props carries its own metric/dimension IDs (chart series
			// and plotted dimension, kpi_scope, a grid's saved layout) — an
			// import always allocates new IDs for those, so copying the blob
			// verbatim guaranteed a dead chart in every imported model.
			props := refs.doc(wd.Props, jsonRefs{})
			colStart, colSpan := wd.legacyColumns()
			if _, err = tx.Exec(ctx, `
				INSERT INTO model.dashboard_widget
				  (dashboard_id, widget_type, ref_id, content, sort_order, col_start, col_span,
				   pos_x, pos_y, size_w, size_h, title, show_title, widget_props)
				VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
				dashMap[d.ID], wd.WidgetType, refID, wd.Content, wd.SortOrder, colStart, colSpan,
				boxes[i].x, boxes[i].y, boxes[i].w, boxes[i].h, wd.Title, wd.ShowTitle, jsonArg(props)); err != nil {
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
		dimMembers := refs.cellDoc(f.DimMembers)
		if _, err = tx.Exec(ctx, `
			INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by, source_ref)
			VALUES ($1::uuid, $2::uuid, $3, $4::uuid, COALESCE($5::jsonb,'{}'::jsonb), $6, $7::uuid, $8::uuid)`,
			modelID, revisionID, revisionName, mid,
			jsonArg(dimMembers), f.Value, importerID, sourceRef); err != nil {
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
