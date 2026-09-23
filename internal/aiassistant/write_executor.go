package aiassistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/internal/timedim"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// revisionIDMap runs a two-column (old_id, new_id) mapping query inside the
// revision-copy transaction and collects it, for the widget_props remap that
// can't be expressed as part of the surrounding SQL copy steps.
func revisionIDMap(ctx context.Context, tx pgx.Tx, query, modelID, newRevID, srcRevID string) (map[string]string, error) {
	rows, err := tx.Query(ctx, query, modelID, newRevID, srcRevID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var oldID, newID string
		if err := rows.Scan(&oldID, &newID); err != nil {
			return nil, err
		}
		out[oldID] = newID
	}
	return out, rows.Err()
}

// WriteExecutor executes write tool calls against the DB.
// It mirrors the SQL logic of the existing developer HTTP handlers.
type WriteExecutor struct {
	pool    *pgxpool.Pool
	modelID string
	revID   string
	userID  string
}

func NewWriteExecutor(pool *pgxpool.Pool, modelID, revID string) *WriteExecutor {
	return &WriteExecutor{pool: pool, modelID: modelID, revID: revID}
}

// NewWriteExecutorWithActor is NewWriteExecutor plus a real actor user ID,
// needed by the workflow_def/form_def tools: CreateWorkflowDefFull/
// UpdateWorkflowDefFull cast created_by/updated_by directly to ::uuid with
// no NULLIF, so an empty string 500s. Additive rather than changing
// NewWriteExecutor's signature — that constructor has 3 real call sites
// plus 17 in write_executor_test.go, none of which need an actor.
func NewWriteExecutorWithActor(pool *pgxpool.Pool, modelID, revID, userID string) *WriteExecutor {
	return &WriteExecutor{pool: pool, modelID: modelID, revID: revID, userID: userID}
}

// modelScopedResourceSQL resolves a resource ID to the model that owns it,
// the revision it belongs to, and its name-based identity within that
// revision; counterpart finds the same-named resource inside another revision
// (see requireInModel for why cross-revision references are remapped).
// Mirrors the ownership map of the same name in internal/gateway/handler.go,
// which backs requireResourceAccess on the HTTP endpoints; duplicated rather
// than shared because gateway imports this package and the dependency cannot
// run the other way.
// uuidShaped reports whether s looks like a UUID — used to decide whether a
// caller-supplied reference is an id or a name.
func uuidShaped(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			hexDigit := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !hexDigit {
				return false
			}
		}
	}
	return true
}

var modelScopedResourceSQL = map[string]struct{ lookup, counterpart string }{
	"metric": {
		lookup:      `SELECT model_id::text, COALESCE(revision_id::text,''), name FROM model.metric_def WHERE id=$1::uuid`,
		counterpart: `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3`,
	},
	"dimension": {
		lookup:      `SELECT model_id::text, COALESCE(revision_id::text,''), name FROM model.dimension_def WHERE id=$1::uuid`,
		counterpart: `SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3`,
	},
	"grid": {
		lookup:      `SELECT model_id::text, COALESCE(revision_id::text,''), name FROM model.grid_def WHERE id=$1::uuid`,
		counterpart: `SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3`,
	},
	"dashboard": {
		lookup:      `SELECT model_id::text, COALESCE(revision_id::text,''), name FROM model.dashboard_def WHERE id=$1::uuid`,
		counterpart: `SELECT id::text FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3`,
	},
	// A member's identity is (dimension name, member code) — codes are only
	// unique within a dimension. The 0x1f separator cannot appear in either.
	"dimension_member": {
		lookup: `SELECT d.model_id::text, COALESCE(d.revision_id::text,''), d.name || E'\x1f' || m.code
		         FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id WHERE m.id=$1::uuid`,
		counterpart: `SELECT m.id::text FROM model.dimension_member m
		              JOIN model.dimension_def d ON d.id = m.dimension_id
		              WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid AND d.name || E'\x1f' || m.code = $3`,
	},
}

// remapChartProps resolves the UUIDs inside a chart widget's props
// (chart.dimension_id, chart.metric_ids, chart.x/y_metric_id) into the
// executor's revision via requireInModel. Unknown keys pass through
// untouched; a ref with no counterpart in the working revision is refused,
// same as every other caller-supplied ID.
func (e *WriteExecutor) remapChartProps(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var props map[string]any
	if err := json.Unmarshal(raw, &props); err != nil {
		return nil, fmt.Errorf("widget_props is not a JSON object: %w", err)
	}
	chart, ok := props["chart"].(map[string]any)
	if !ok {
		return raw, nil
	}
	remapOne := func(kind, key string) error {
		id, _ := chart[key].(string)
		if id == "" {
			return nil
		}
		mapped, err := e.requireInModel(ctx, kind, id)
		if err != nil {
			return fmt.Errorf("chart %s: %w", key, err)
		}
		chart[key] = mapped
		return nil
	}
	if err := remapOne("dimension", "dimension_id"); err != nil {
		return nil, err
	}
	for _, key := range []string{"x_metric_id", "y_metric_id"} {
		if err := remapOne("metric", key); err != nil {
			return nil, err
		}
	}
	if ids, ok := chart["metric_ids"].([]any); ok {
		for i, v := range ids {
			id, _ := v.(string)
			if id == "" {
				continue
			}
			mapped, err := e.requireInModel(ctx, "metric", id)
			if err != nil {
				return nil, fmt.Errorf("chart metric_ids[%d]: %w", i, err)
			}
			ids[i] = mapped
		}
	}
	out, err := json.Marshal(props)
	if err != nil {
		return nil, fmt.Errorf("re-encode widget_props: %w", err)
	}
	return out, nil
}

// effectiveRevision decides which revision a create tool writes into. When
// the executor is revision-scoped, its scope WINS over any caller-supplied
// revision_id: the params come from a language model that echoes whatever
// revision IDs its read tools showed it, and honoring them let a proposal
// create rows directly in the ACTIVE revision instead of the session draft —
// the same isolation bypass requireInModel documents. An unscoped executor
// (revID == "", used only to create the draft itself) keeps honoring the
// param.
func (e *WriteExecutor) effectiveRevision(requested string) string {
	if e.revID != "" {
		return e.revID
	}
	return requested
}

// requireInModel confirms a caller-supplied resource ID belongs to the model
// this executor is scoped to and resolves it INTO the executor's revision,
// returning the ID every subsequent statement must use.
//
// The assistant runs as the developer who opened it, and that developer's
// access was checked once — against the MODEL. Every tool ID after that point
// arrives from the language model, so without this a tool call naming any UUID
// in the database would be executed against it: update_metric and
// delete_metric were plain "WHERE id=$1" with no scoping, where the HTTP
// endpoints behind the same actions call requireResourceAccess and answer 403.
// The assistant is meant to have a developer's capabilities, not a superset of
// them.
//
// Cross-revision references are remapped, not honored and not rejected. The
// model reads through tools scoped to the session's working revision, but a
// session's draft is created lazily on first confirm — so the IDs the model
// saw (and the developer pasted) were usually minted in the revision the
// draft was copied FROM. Honoring them verbatim is how a 27-step
// add_grid_metric proposal mutated the ACTIVE revision on 2026-08-25,
// silently bypassing the isolation aiProposalConfirm promises ("AI writes
// never target the active revision directly"); rejecting them would fail
// every first proposal after a promote. The draft is a copy, so the
// same-named counterpart the reference means is guaranteed to exist there —
// resolve to it. A reference with no counterpart (deleted, renamed, or from
// an unrelated lineage) is refused.
//
// Fails closed. A resource that does not resolve, or a query that errors, is
// refused rather than assumed to be in scope.
func (e *WriteExecutor) requireInModel(ctx context.Context, kind, id string) (string, error) {
	q, ok := modelScopedResourceSQL[kind]
	if !ok {
		return "", fmt.Errorf("cannot verify ownership of a %s", kind)
	}
	// LLMs routinely pass the NAME where the schema says id ("dimension_id":
	// "products" — seen live); resolve non-UUID references by name within
	// the working revision, the same identity the counterpart remap already
	// uses. dimension_member is excluded (its identity is composite).
	if !uuidShaped(id) && kind != "dimension_member" && e.revID != "" {
		var mapped string
		if err := e.pool.QueryRow(ctx, q.counterpart, e.modelID, e.revID, id).Scan(&mapped); err == nil {
			return mapped, nil
		}
		return "", fmt.Errorf("%s %q not found in the working revision — pass its exact name or id", kind, id)
	}
	var owner, revision, identity string
	if err := e.pool.QueryRow(ctx, q.lookup, id).Scan(&owner, &revision, &identity); err != nil {
		return "", fmt.Errorf("%s %s not found in this model", kind, id)
	}
	if owner != e.modelID {
		return "", fmt.Errorf("%s %s belongs to a different model", kind, id)
	}
	// revision == "" is a legacy pre-revision row; e.revID == "" is an
	// unscoped executor (used only to create the draft itself). Neither has
	// a revision boundary to enforce.
	if revision == "" || e.revID == "" || revision == e.revID {
		return id, nil
	}
	var mapped string
	if err := e.pool.QueryRow(ctx, q.counterpart, e.modelID, e.revID, identity).Scan(&mapped); err != nil {
		return "", fmt.Errorf("%s %s belongs to a different revision and has no counterpart in the working revision", kind, id)
	}
	return mapped, nil
}

// Execute runs a single write tool and returns (humanResult, createdID, error).
// createdID is non-empty when a new resource was created (used for rollback).
func (e *WriteExecutor) Execute(ctx context.Context, tool string, params json.RawMessage) (result, createdID string, err error) {
	switch tool {
	case "create_metric":
		return e.createMetric(ctx, params)
	case "update_metric":
		return e.updateMetric(ctx, params)
	case "delete_metric":
		return e.deleteMetric(ctx, params)
	case "create_dimension":
		return e.createDimension(ctx, params)
	case "add_dimension_member":
		return e.addDimensionMember(ctx, params)
	case "update_dimension_member":
		return e.updateDimensionMember(ctx, params)
	case "create_grid":
		return e.createGrid(ctx, params)
	case "add_grid_metric":
		return e.addGridMetric(ctx, params)
	case "add_grid_dimension":
		return e.addGridDimension(ctx, params)
	case "create_dashboard":
		return e.createDashboard(ctx, params)
	case "add_dashboard_widget":
		return e.addDashboardWidget(ctx, params)
	case "create_revision":
		return e.createRevision(ctx, params)
	case "create_workflow_def":
		return e.createWorkflowDef(ctx, params)
	case "update_workflow_def":
		return e.updateWorkflowDef(ctx, params)
	case "create_form_def":
		return e.createFormDef(ctx, params)
	case "update_form_def":
		return e.updateFormDef(ctx, params)
	case "delete_workflow_def":
		return e.deleteWorkflowDef(ctx, params)
	case "delete_form_def":
		return e.deleteFormDef(ctx, params)
	case "create_automation_rule":
		return e.createAutomationRule(ctx, params)
	case "update_automation_rule":
		return e.updateAutomationRule(ctx, params)
	case "delete_automation_rule":
		return e.deleteAutomationRule(ctx, params)
	case "create_business_role":
		return e.createBusinessRole(ctx, params)
	case "create_form_integration":
		return e.createFormIntegration(ctx, params)
	case "update_form_integration":
		return e.updateFormIntegration(ctx, params)
	case "delete_form_integration":
		return e.deleteFormIntegration(ctx, params)
	case "generate_migration":
		return e.generateMigration(ctx)
	case "apply_migration":
		return e.applyMigration(ctx, params)
	case "set_user_access_rules":
		return e.setUserAccessRules(ctx, params)
	default:
		return "", "", fmt.Errorf("unknown write tool: %s", tool)
	}
}

// ── create_metric ─────────────────────────────────────────────────────────────

type createMetricParams struct {
	Name           string `json:"name"`
	Formula        string `json:"formula"`
	IsInput        bool   `json:"is_input"`
	Format         string `json:"format"`
	FormatDecimals int    `json:"format_decimals"`
	FormatCurrency string `json:"format_currency"`
	AggRule        string `json:"agg_rule"`
	RevisionID     string `json:"revision_id"`
	// The two operands agg_rule "rate" divides. Without these the AI could
	// select "rate" but never supply what it divides, so the metric saved
	// clean and then failed in the scheduler on every recalculation.
	AggNumeratorMetricID   string `json:"agg_numerator_metric_id"`
	AggDenominatorMetricID string `json:"agg_denominator_metric_id"`
	// TimeSummary: aggregation across a time dimension (sum | average | min
	// | max | first | last | none). Empty = sum.
	TimeSummary string `json:"time_summary"`
}

func (e *WriteExecutor) createMetric(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p createMetricParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	if !p.IsInput && p.Formula == "" {
		return "", "", fmt.Errorf("formula is required for calculated metrics")
	}
	if p.Format == "" {
		p.Format = "number"
	}
	if p.FormatCurrency == "" {
		p.FormatCurrency = "$"
	}
	if p.AggRule == "" {
		p.AggRule = "sum"
	}
	if p.TimeSummary == "" {
		p.TimeSummary = "sum"
	}
	if !timedim.ValidTimeSummary(p.TimeSummary) {
		return "", "", fmt.Errorf("time_summary must be one of %s", strings.Join(timedim.TimeSummaries, ", "))
	}
	revID := e.effectiveRevision(p.RevisionID)

	// The same two checks the developer role's metric handler runs. Neither
	// ran here before, so the AI could save an aggregation rule the engine
	// does not implement, or a "rate" pointing at another revision's metric.
	if err := metricformula.ValidateAggRule(p.AggRule, p.IsInput,
		p.AggNumeratorMetricID, p.AggDenominatorMetricID, ""); err != nil {
		return "", "", err
	}
	if err := metricformula.ValidateAggOperands(ctx, e.pool, p.AggRule, e.modelID, revID,
		p.AggNumeratorMetricID, p.AggDenominatorMetricID); err != nil {
		return "", "", err
	}

	var formulaPtr *string
	var formulaEdges []metricformula.Edge
	if !p.IsInput && p.Formula != "" {
		formulaPtr = &p.Formula
		// Validate through internal/metricformula, the same service the
		// developer role's own metric handler uses. The hand-rolled check
		// that used to live here split the formula on operators and compared
		// the pieces to metric names, which was wrong in both directions: it
		// rejected every formula a developer can write with function calls or
		// legacy {name} references, because IFERROR and {revenue} are not
		// metric names — and it accepted formulas that do not parse, name
		// unknown functions, or reference themselves, all of which a
		// developer is refused.
		res, vErr := metricformula.Validate(ctx, e.pool, metricformula.Request{
			ModelID: e.modelID, RevisionID: revID, Name: p.Name, Formula: p.Formula,
		})
		if vErr != nil {
			return "", "", vErr
		}
		formulaEdges = res.Edges
	}

	var newID string
	var err error
	if revID != "" {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.metric_def (model_id, name, formula, is_input, revision_id, format, format_decimals, format_currency, agg_rule,
			                              agg_numerator_metric_id, agg_denominator_metric_id, time_summary)
			VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6, $7, $8, $9, NULLIF($10,'')::uuid, NULLIF($11,'')::uuid, $12)
			RETURNING id::text
		`, e.modelID, p.Name, formulaPtr, p.IsInput, revID, p.Format, p.FormatDecimals, p.FormatCurrency, p.AggRule,
			p.AggNumeratorMetricID, p.AggDenominatorMetricID, p.TimeSummary).Scan(&newID)
	} else {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.metric_def (model_id, name, formula, is_input, format, format_decimals, format_currency, agg_rule,
			                              agg_numerator_metric_id, agg_denominator_metric_id, time_summary)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, NULLIF($9,'')::uuid, NULLIF($10,'')::uuid, $11)
			RETURNING id::text
		`, e.modelID, p.Name, formulaPtr, p.IsInput, p.Format, p.FormatDecimals, p.FormatCurrency, p.AggRule,
			p.AggNumeratorMetricID, p.AggDenominatorMetricID, p.TimeSummary).Scan(&newID)
	}
	if err != nil {
		return "", "", fmt.Errorf("insert metric: %w", err)
	}

	// Wire formula dependencies from the edges the validator resolved. It
	// looked them up within the metric's own revision; the loop that used to
	// be here resolved names model-wide, so an edge could point at a
	// same-named metric belonging to a different revision.
	if er := metricformula.WriteDependencies(ctx, e.pool, newID, formulaEdges); er != nil {
		return "", "", fmt.Errorf("wire formula dependencies: %w", er)
	}

	return fmt.Sprintf("Metric '%s' created (id: %s)", p.Name, newID), newID, nil
}

// ── update_metric ─────────────────────────────────────────────────────────────

type updateMetricParams struct {
	MetricID       string `json:"metric_id"`
	Name           string `json:"name"`
	Formula        string `json:"formula"`
	AggRule        string `json:"agg_rule"`
	Format         string `json:"format"`
	FormatDecimals int    `json:"format_decimals"`
	FormatCurrency string `json:"format_currency"`

	AggNumeratorMetricID   string `json:"agg_numerator_metric_id"`
	AggDenominatorMetricID string `json:"agg_denominator_metric_id"`
	TimeSummary            string `json:"time_summary"`
}

func (e *WriteExecutor) updateMetric(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p updateMetricParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.MetricID == "" {
		return "", "", fmt.Errorf("metric_id is required")
	}
	mappedMetricID, err := e.requireInModel(ctx, "metric", p.MetricID)
	if err != nil {
		return "", "", err
	}
	p.MetricID = mappedMetricID
	if p.AggRule == "" {
		p.AggRule = "sum"
	}
	if p.TimeSummary == "" {
		p.TimeSummary = "sum"
	}
	if !timedim.ValidTimeSummary(p.TimeSummary) {
		return "", "", fmt.Errorf("time_summary must be one of %s", strings.Join(timedim.TimeSummaries, ", "))
	}
	if p.Format == "" {
		p.Format = "number"
	}
	if p.FormatCurrency == "" {
		p.FormatCurrency = "$"
	}
	// The metric's own revision is needed before validating, not after: refs
	// resolve within it, and passing MetricID is what lets the validator
	// reject a formula that references the metric being edited, directly or
	// through a cycle.
	var metricRev string
	if err := e.pool.QueryRow(ctx,
		`SELECT COALESCE(revision_id::text,'') FROM model.metric_def WHERE id=$1::uuid`,
		p.MetricID).Scan(&metricRev); err != nil {
		return "", "", fmt.Errorf("load metric: %w", err)
	}

	// is_input is the stored one: update_metric does not carry the flag, and
	// ValidateAggRule needs it to reject "formula" on an input metric.
	var metricIsInput bool
	if err := e.pool.QueryRow(ctx,
		`SELECT is_input FROM model.metric_def WHERE id=$1::uuid`, p.MetricID).Scan(&metricIsInput); err != nil {
		return "", "", fmt.Errorf("load metric: %w", err)
	}
	if err := metricformula.ValidateAggRule(p.AggRule, metricIsInput,
		p.AggNumeratorMetricID, p.AggDenominatorMetricID, p.MetricID); err != nil {
		return "", "", err
	}
	if err := metricformula.ValidateAggOperands(ctx, e.pool, p.AggRule, e.modelID, metricRev,
		p.AggNumeratorMetricID, p.AggDenominatorMetricID); err != nil {
		return "", "", err
	}

	var formulaPtr *string
	var formulaEdges []metricformula.Edge
	if p.Formula != "" {
		formulaPtr = &p.Formula
		res, vErr := metricformula.Validate(ctx, e.pool, metricformula.Request{
			ModelID: e.modelID, RevisionID: metricRev, MetricID: p.MetricID,
			Name: p.Name, Formula: p.Formula,
		})
		if vErr != nil {
			return "", "", vErr
		}
		formulaEdges = res.Edges
	}
	if _, err := e.pool.Exec(ctx, `
		UPDATE model.metric_def
		SET name=$2, formula=$3, agg_rule=$4, format=$5, format_decimals=$6, format_currency=$7,
		    agg_numerator_metric_id=NULLIF($8,'')::uuid, agg_denominator_metric_id=NULLIF($9,'')::uuid,
		    time_summary=$10
		WHERE id=$1::uuid
	`, p.MetricID, p.Name, formulaPtr, p.AggRule, p.Format, p.FormatDecimals, p.FormatCurrency,
		p.AggNumeratorMetricID, p.AggDenominatorMetricID, p.TimeSummary); err != nil {
		return "", "", fmt.Errorf("update metric: %w", err)
	}
	// Re-wire dependencies when the formula changed.
	if formulaPtr != nil {
		if er := metricformula.WriteDependencies(ctx, e.pool, p.MetricID, formulaEdges); er != nil {
			return "", "", fmt.Errorf("wire formula dependencies: %w", er)
		}
	}
	return fmt.Sprintf("Metric '%s' updated", p.Name), "", nil
}

// ── delete_metric ─────────────────────────────────────────────────────────────

func (e *WriteExecutor) deleteMetric(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		MetricID string `json:"metric_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.MetricID == "" {
		return "", "", fmt.Errorf("metric_id is required")
	}
	mappedMetricID, err := e.requireInModel(ctx, "metric", p.MetricID)
	if err != nil {
		return "", "", err
	}
	p.MetricID = mappedMetricID
	var name string
	_ = e.pool.QueryRow(ctx, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, p.MetricID).Scan(&name)
	// SYNC-01 cascade: a metric_kpi widget over a deleted metric showed a
	// confident 0 forever (ref_id has no FK).
	_, _ = e.pool.Exec(ctx, `DELETE FROM model.dashboard_widget WHERE ref_id = $1`, p.MetricID)
	if _, err := e.pool.Exec(ctx, `DELETE FROM model.metric_def WHERE id=$1::uuid`, p.MetricID); err != nil {
		return "", "", fmt.Errorf("delete metric: %w", err)
	}
	return fmt.Sprintf("Metric '%s' deleted", name), "", nil
}

// ── create_dimension ──────────────────────────────────────────────────────────

type createDimensionParams struct {
	Name                string `json:"name"`
	AggRule             string `json:"agg_rule"`
	RevisionID          string `json:"revision_id"`
	ParentDimensionName string `json:"parent_dimension_name"` // if set, this dimension is a child of that one — members' parent_code resolves against the PARENT dimension's members
	// Time dimension marker (spec §4.1): "standard" (default) or "time".
	// A time dimension needs time_granularity and fiscal_year_start_month,
	// and its members carry period_start/period_end instead of parents.
	DimensionType   string `json:"dimension_type"`
	TimeGranularity string `json:"time_granularity"`
	FiscalYearStart int    `json:"fiscal_year_start_month"`
	Members         []struct {
		Code        string `json:"code"`
		Label       string `json:"label"`
		ParentCode  string `json:"parent_code"`
		PeriodStart string `json:"period_start"`
		PeriodEnd   string `json:"period_end"`
	} `json:"members"`
}

func (e *WriteExecutor) createDimension(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p createDimensionParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	if p.AggRule == "" {
		p.AggRule = "sum"
	}
	timeCfg := timedim.Config{Type: p.DimensionType, Granularity: p.TimeGranularity, FiscalYearStartMonth: p.FiscalYearStart}
	if err := timedim.ValidateConfig(&timeCfg); err != nil {
		return "", "", err
	}
	if timeCfg.Type == timedim.TypeTime && p.ParentDimensionName != "" {
		return "", "", fmt.Errorf("a time dimension cannot have a parent dimension")
	}
	revID := e.effectiveRevision(p.RevisionID)

	var parentDimID *string
	if p.ParentDimensionName != "" {
		var pdID string
		if err := e.pool.QueryRow(ctx, `
			SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND name=$2
		`, e.modelID, p.ParentDimensionName).Scan(&pdID); err != nil {
			return "", "", fmt.Errorf("parent dimension %q not found: %w", p.ParentDimensionName, err)
		}
		parentDimID = &pdID
	}

	var newID string
	var err error
	var granularity *string
	var fiscalStart *int
	if timeCfg.Type == timedim.TypeTime {
		granularity, fiscalStart = &timeCfg.Granularity, &timeCfg.FiscalYearStartMonth
	}
	if revID != "" {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.dimension_def (model_id, name, agg_rule, revision_id, parent_dimension_id, dimension_type, time_granularity, fiscal_year_start_month)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5::uuid, $6, $7, $8) RETURNING id::text
		`, e.modelID, p.Name, p.AggRule, revID, parentDimID, timeCfg.Type, granularity, fiscalStart).Scan(&newID)
	} else {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.dimension_def (model_id, name, agg_rule, parent_dimension_id, dimension_type, time_granularity, fiscal_year_start_month)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6, $7) RETURNING id::text
		`, e.modelID, p.Name, p.AggRule, parentDimID, timeCfg.Type, granularity, fiscalStart).Scan(&newID)
	}
	if err != nil {
		return "", "", fmt.Errorf("insert dimension: %w", err)
	}
	if timeCfg.Type == timedim.TypeTime {
		// Time members go through the shared validation + reindex path in
		// one transaction, exactly as the developer console's do.
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			return "", "", err
		}
		defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
		// A member with dates is a leaf period; one without is an aggregate
		// period (H1, FY26). parent_code resolves against members created
		// earlier in this same call, so parents go first.
		codeToID := map[string]string{}
		for _, m := range p.Members {
			var start, end *time.Time
			var idx *int
			if m.PeriodStart != "" || m.PeriodEnd != "" {
				ps, err1 := timedim.ParseDate(m.PeriodStart)
				pe, err2 := timedim.ParseDate(m.PeriodEnd)
				if err1 != nil || err2 != nil {
					return "", "", fmt.Errorf("member %q: period_start and period_end (YYYY-MM-DD) go together; leave both empty for an aggregate period", m.Code)
				}
				start, end = &ps, &pe
				zero := 0
				idx = &zero
			}
			var parentID *string
			if m.ParentCode != "" {
				pid, ok := codeToID[m.ParentCode]
				if !ok {
					return "", "", fmt.Errorf("member %q: parent %q must be listed before it", m.Code, m.ParentCode)
				}
				parentID = &pid
			}
			var memID string
			if err := tx.QueryRow(ctx, `
				INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, parent_member_id)
				VALUES ($1::uuid, $2, $3, $4::date, $5::date, $6, $7::uuid) RETURNING id::text
			`, newID, m.Code, m.Label, start, end, idx, parentID).Scan(&memID); err != nil {
				return "", "", fmt.Errorf("insert time member %q: %w", m.Code, err)
			}
			codeToID[m.Code] = memID
		}
		if err := timedim.ValidateAndReindex(ctx, tx, newID); err != nil {
			return "", "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", "", err
		}
		msg := fmt.Sprintf("Time dimension '%s' (%s) created (id: %s)", p.Name, timeCfg.Granularity, newID)
		if len(p.Members) > 0 {
			msg += fmt.Sprintf(" with %d period(s)", len(p.Members))
		}
		return msg, newID, nil
	}

	// When members are children of a declared parent dimension, parent_code must
	// resolve against THAT dimension's existing members, not the (empty, in-progress)
	// set of members being created here.
	codeToID := map[string]string{}
	if parentDimID != nil {
		rows, qErr := e.pool.Query(ctx, `SELECT code, id::text FROM model.dimension_member WHERE dimension_id=$1::uuid`, *parentDimID)
		if qErr == nil {
			for rows.Next() {
				var code, id string
				if rows.Scan(&code, &id) == nil {
					codeToID[code] = id
				}
			}
			rows.Close()
		}
	}

	// Insert members if provided, resolving parent codes.
	for _, m := range p.Members {
		var parentID *string
		if m.ParentCode != "" {
			if pid, ok := codeToID[m.ParentCode]; ok {
				parentID = &pid
			}
		}
		var memID string
		er := e.pool.QueryRow(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, sort_order)
			VALUES ($1::uuid, $2, $3, $4::uuid, (
				SELECT COALESCE(MAX(sort_order),0)+1 FROM model.dimension_member WHERE dimension_id=$1::uuid
			)) RETURNING id::text
		`, newID, m.Code, m.Label, parentID).Scan(&memID)
		if er == nil && parentDimID == nil {
			// Same-dimension hierarchy: later members in this same call may reference
			// an earlier one as their parent.
			codeToID[m.Code] = memID
		}
	}

	msg := fmt.Sprintf("Dimension '%s' created (id: %s)", p.Name, newID)
	if len(p.Members) > 0 {
		msg += fmt.Sprintf(" with %d member(s)", len(p.Members))
	}
	return msg, newID, nil
}

// ── add_dimension_member ──────────────────────────────────────────────────────

func (e *WriteExecutor) addDimensionMember(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		DimensionID string `json:"dimension_id"`
		Code        string `json:"code"`
		Label       string `json:"label"`
		ParentCode  string `json:"parent_code"`
		PeriodStart string `json:"period_start"`
		PeriodEnd   string `json:"period_end"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.DimensionID == "" || p.Code == "" {
		return "", "", fmt.Errorf("dimension_id and code are required")
	}
	mappedDimID, err := e.requireInModel(ctx, "dimension", p.DimensionID)
	if err != nil {
		return "", "", err
	}
	p.DimensionID = mappedDimID

	// A time dimension's member is a period: dates instead of a parent,
	// validated and indexed with the rest of the dimension in one
	// transaction.
	if cfg, cErr := timedim.LoadConfig(ctx, e.pool, p.DimensionID); cErr == nil && cfg.Type == timedim.TypeTime {
		// Dated = leaf period; undated = aggregate period (H1, FY26). A
		// parent, when given, must be an aggregate of the same dimension
		// (checked with the rest of the hierarchy by ValidateAndReindex).
		var start, end *time.Time
		var idx *int
		if p.PeriodStart != "" || p.PeriodEnd != "" {
			ps, err1 := timedim.ParseDate(p.PeriodStart)
			pe, err2 := timedim.ParseDate(p.PeriodEnd)
			if err1 != nil || err2 != nil {
				return "", "", fmt.Errorf("period_start and period_end (YYYY-MM-DD) go together; leave both empty for an aggregate period")
			}
			if err := timedim.ValidatePeriod(cfg, timedim.Period{Code: p.Code, Start: ps, End: pe}); err != nil {
				return "", "", err
			}
			start, end = &ps, &pe
			zero := 0
			idx = &zero
		}
		var parentID *string
		if p.ParentCode != "" {
			var pid string
			if err := e.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
				p.DimensionID, p.ParentCode).Scan(&pid); err != nil {
				return "", "", fmt.Errorf("parent period %q not found in this dimension", p.ParentCode)
			}
			parentID = &pid
		}
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			return "", "", err
		}
		defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
		var newID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, parent_member_id)
			VALUES ($1::uuid, $2, $3, $4::date, $5::date, $6, $7::uuid) RETURNING id::text
		`, p.DimensionID, p.Code, p.Label, start, end, idx, parentID).Scan(&newID); err != nil {
			return "", "", fmt.Errorf("insert time member: %w", err)
		}
		if err := timedim.ValidateAndReindex(ctx, tx, p.DimensionID); err != nil {
			return "", "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", "", err
		}
		if start == nil {
			return fmt.Sprintf("Aggregate period '%s' (%s) added (id: %s)", p.Label, p.Code, newID), newID, nil
		}
		return fmt.Sprintf("Period '%s' (%s, %s..%s) added (id: %s)", p.Label, p.Code, p.PeriodStart, p.PeriodEnd, newID), newID, nil
	}
	if p.PeriodStart != "" || p.PeriodEnd != "" {
		return "", "", fmt.Errorf("period_start/period_end apply only to a time dimension's members")
	}

	var parentID *string
	autoCreatedParent := false
	if p.ParentCode != "" {
		// Resolve parent_code against the declared parent dimension when this
		// dimension is a child (e.g. Cabinet -> Department); otherwise same-dimension.
		var parentDimensionID *string
		_ = e.pool.QueryRow(ctx, `SELECT parent_dimension_id::text FROM model.dimension_def WHERE id=$1::uuid`, p.DimensionID).Scan(&parentDimensionID)
		lookupDim := p.DimensionID
		if parentDimensionID != nil {
			lookupDim = *parentDimensionID
		}
		var pid string
		er := e.pool.QueryRow(ctx, `
			SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2
		`, lookupDim, p.ParentCode).Scan(&pid)
		switch {
		case er == nil:
			parentID = &pid
		case errors.Is(er, pgx.ErrNoRows) && lookupDim == p.DimensionID:
			// A batch import (e.g. from an uploaded file) commonly references
			// a root/group value — here "total" — as everyone's parent
			// without ever listing it as its own row, so no step in the plan
			// creates it. Rather than fail every single member under it
			// (which is what made this non-deterministic: whether it works
			// depends entirely on whether the LLM happened to also emit a
			// step for the parent), create it as a top-level member the
			// first time it's referenced — later steps in the same batch
			// that reference the same code then resolve normally via the
			// lookup above. Cross-dimension parents (lookupDim != DimensionID,
			// e.g. Cabinet -> Department) are never auto-created: that parent
			// belongs to a different, presumably already-populated dimension,
			// so a miss there is a real error, not an ordering gap.
			if cerr := e.pool.QueryRow(ctx, `
				INSERT INTO model.dimension_member (dimension_id, code, label, sort_order)
				VALUES ($1::uuid, $2, $2, (
					SELECT COALESCE(MAX(sort_order),0)+1 FROM model.dimension_member WHERE dimension_id=$1::uuid
				)) RETURNING id::text
			`, lookupDim, p.ParentCode).Scan(&pid); cerr != nil {
				return "", "", fmt.Errorf("parent member %q not found, and creating it as a top-level member failed: %w", p.ParentCode, cerr)
			}
			parentID = &pid
			autoCreatedParent = true
		default:
			return "", "", fmt.Errorf("look up parent member %q: %w", p.ParentCode, er)
		}
	}

	var newID string
	err = e.pool.QueryRow(ctx, `
		INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, sort_order)
		VALUES ($1::uuid, $2, $3, $4::uuid, (
			SELECT COALESCE(MAX(sort_order),0)+1 FROM model.dimension_member WHERE dimension_id=$1::uuid
		)) RETURNING id::text
	`, p.DimensionID, p.Code, p.Label, parentID).Scan(&newID)
	if err != nil {
		return "", "", fmt.Errorf("insert dimension member: %w", err)
	}
	result := fmt.Sprintf("Dimension member '%s' (%s) added (id: %s)", p.Label, p.Code, newID)
	if autoCreatedParent {
		result += fmt.Sprintf(" — parent '%s' didn't exist yet, created it as a top-level member", p.ParentCode)
	}
	return result, newID, nil
}

// ── update_dimension_member ───────────────────────────────────────────────────

// updateDimensionMember changes an EXISTING member — the AI Developer's twin
// of the developer console's member PATCH (label, re-parent, properties
// merge). Added for a real request the AI could not fulfil: "move members
// whose category property is hardware/software under the matching parent" —
// there was a tool to ADD members but none to move one.
func (e *WriteExecutor) updateDimensionMember(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		DimensionID string `json:"dimension_id"`
		Code        string `json:"code"`
		Label       string `json:"label"`
		// ParentCode moves the member under that parent; omitted/"" leaves
		// the parent unchanged. ClearParent=true makes it a top-level member.
		ParentCode  string            `json:"parent_code"`
		ClearParent bool              `json:"clear_parent"`
		Properties  map[string]string `json:"properties"` // merged into existing
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.DimensionID == "" || p.Code == "" {
		return "", "", fmt.Errorf("dimension_id and code are required")
	}
	mappedDimID, err := e.requireInModel(ctx, "dimension", p.DimensionID)
	if err != nil {
		return "", "", err
	}
	p.DimensionID = mappedDimID

	var memberID string
	if err := e.pool.QueryRow(ctx, `
		SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2
	`, p.DimensionID, p.Code).Scan(&memberID); err != nil {
		return "", "", fmt.Errorf("member %q not found in dimension (call list_dimensions to see codes)", p.Code)
	}

	var changed []string
	if p.Label != "" {
		if _, err := e.pool.Exec(ctx, `UPDATE model.dimension_member SET label=$2 WHERE id=$1::uuid`, memberID, p.Label); err != nil {
			return "", "", fmt.Errorf("update label: %w", err)
		}
		changed = append(changed, fmt.Sprintf("label → %q", p.Label))
	}

	switch {
	case p.ClearParent:
		if _, err := e.pool.Exec(ctx, `UPDATE model.dimension_member SET parent_member_id=NULL WHERE id=$1::uuid`, memberID); err != nil {
			return "", "", fmt.Errorf("clear parent: %w", err)
		}
		changed = append(changed, "parent cleared (now top-level)")
	case p.ParentCode != "":
		// Same cross-dimension-aware resolution as add_dimension_member, but
		// strict: an update naming a missing parent is a mistake to surface,
		// not an ordering gap to paper over with an auto-created member.
		var parentDimensionID *string
		_ = e.pool.QueryRow(ctx, `SELECT parent_dimension_id::text FROM model.dimension_def WHERE id=$1::uuid`, p.DimensionID).Scan(&parentDimensionID)
		lookupDim := p.DimensionID
		if parentDimensionID != nil {
			lookupDim = *parentDimensionID
		}
		var parentID string
		if err := e.pool.QueryRow(ctx, `
			SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2
		`, lookupDim, p.ParentCode).Scan(&parentID); err != nil {
			return "", "", fmt.Errorf("parent member %q not found (add it first with add_dimension_member)", p.ParentCode)
		}
		if parentID == memberID {
			return "", "", fmt.Errorf("a member cannot be its own parent")
		}
		// Same-dimension re-parent: refuse a cycle (new parent being a
		// descendant of the member we're moving) before it corrupts every
		// rollup walk over this hierarchy.
		if lookupDim == p.DimensionID {
			cur := parentID
			for cur != "" {
				var up *string
				if err := e.pool.QueryRow(ctx, `SELECT parent_member_id::text FROM model.dimension_member WHERE id=$1::uuid`, cur).Scan(&up); err != nil || up == nil {
					break
				}
				if *up == memberID {
					return "", "", fmt.Errorf("cannot move %q under %q: that member is its own descendant (would create a cycle)", p.Code, p.ParentCode)
				}
				cur = *up
			}
		}
		if _, err := e.pool.Exec(ctx, `UPDATE model.dimension_member SET parent_member_id=$2::uuid WHERE id=$1::uuid`, memberID, parentID); err != nil {
			return "", "", fmt.Errorf("set parent: %w", err)
		}
		changed = append(changed, fmt.Sprintf("parent → %s", p.ParentCode))
	}

	if len(p.Properties) > 0 {
		propJSON, _ := json.Marshal(p.Properties)
		if _, err := e.pool.Exec(ctx, `
			UPDATE model.dimension_member SET properties = COALESCE(properties,'{}'::jsonb) || $2::jsonb WHERE id=$1::uuid
		`, memberID, string(propJSON)); err != nil {
			return "", "", fmt.Errorf("merge properties: %w", err)
		}
		changed = append(changed, fmt.Sprintf("properties merged (%d)", len(p.Properties)))
	}

	if len(changed) == 0 {
		return "", "", fmt.Errorf("nothing to change: provide label, parent_code, clear_parent, or properties")
	}
	return fmt.Sprintf("Dimension member '%s' updated: %s", p.Code, strings.Join(changed, "; ")), memberID, nil
}

// ── create_grid ───────────────────────────────────────────────────────────────

type createGridParams struct {
	Name         string   `json:"name"`
	RevisionID   string   `json:"revision_id"`
	MetricIDs    []string `json:"metric_ids"`
	DimensionIDs []string `json:"dimension_ids"`
}

func (e *WriteExecutor) createGrid(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p createGridParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	revID := e.effectiveRevision(p.RevisionID)

	var newID string
	var err error
	if revID != "" {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.grid_def (model_id, name, revision_id)
			VALUES ($1::uuid, $2, $3::uuid) RETURNING id::text
		`, e.modelID, p.Name, revID).Scan(&newID)
	} else {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.grid_def (model_id, name)
			VALUES ($1::uuid, $2) RETURNING id::text
		`, e.modelID, p.Name).Scan(&newID)
	}
	if err != nil {
		return "", "", fmt.Errorf("insert grid: %w", err)
	}

	// A metric may only belong to one grid at a time (model.grid_metric has
	// a DB-level UNIQUE(metric_id) constraint) — check before each insert,
	// same as addGridMetric below, so a conflicting metric is skipped and
	// reported rather than silently no-op'd via ON CONFLICT DO NOTHING
	// while the returned message still claimed every requested metric was
	// attached.
	attached := 0
	var skipped []string
	for i, mid := range p.MetricIDs {
		var existingGridName string
		err := e.pool.QueryRow(ctx, `
			SELECT gd.name FROM model.grid_metric gm
			JOIN model.grid_def gd ON gd.id = gm.grid_id
			WHERE gm.metric_id=$1::uuid
			LIMIT 1
		`, mid).Scan(&existingGridName)
		if err == nil {
			skipped = append(skipped, fmt.Sprintf("%s (already in grid %q)", mid, existingGridName))
			continue
		}
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO model.grid_metric (grid_id, metric_id, sort_order)
			VALUES ($1::uuid, $2::uuid, $3) ON CONFLICT DO NOTHING
		`, newID, mid, i); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (insert failed: %v)", mid, err))
			continue
		}
		attached++
	}
	for _, did := range p.DimensionIDs {
		_, _ = e.pool.Exec(ctx, `
			INSERT INTO model.grid_dimension (grid_id, dimension_id)
			VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING
		`, newID, did)
	}
	if len(p.DimensionIDs) > 0 {
		var gridModelID, gridRevID string
		_ = e.pool.QueryRow(ctx, `SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.grid_def WHERE id=$1::uuid`, newID).Scan(&gridModelID, &gridRevID)
		if err := metricformula.ValidateGridTime(ctx, e.pool, gridModelID, gridRevID, newID); err != nil {
			_, _ = e.pool.Exec(ctx, `DELETE FROM model.grid_def WHERE id=$1::uuid`, newID)
			return "", "", fmt.Errorf("grid configuration: %w", err)
		}
	}

	msg := fmt.Sprintf("Grid '%s' created (id: %s, %d/%d metrics attached, %d dims)",
		p.Name, newID, attached, len(p.MetricIDs), len(p.DimensionIDs))
	if len(skipped) > 0 {
		msg += fmt.Sprintf(" — skipped (already assigned elsewhere): %s", strings.Join(skipped, "; "))
	}
	return msg, newID, nil
}

// ── add_grid_metric ───────────────────────────────────────────────────────────

func (e *WriteExecutor) addGridMetric(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		GridID   string `json:"grid_id"`
		MetricID string `json:"metric_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.GridID == "" || p.MetricID == "" {
		return "", "", fmt.Errorf("grid_id and metric_id are required")
	}
	// Both sides: the same-revision check below compares them to each other,
	// which two resources in another model satisfy just as well.
	mappedGridID, err := e.requireInModel(ctx, "grid", p.GridID)
	if err != nil {
		return "", "", err
	}
	p.GridID = mappedGridID
	mappedMetricID, err := e.requireInModel(ctx, "metric", p.MetricID)
	if err != nil {
		return "", "", err
	}
	p.MetricID = mappedMetricID

	var gridRev, metricRev string
	_ = e.pool.QueryRow(ctx, `SELECT COALESCE(revision_id::text,'') FROM model.grid_def WHERE id=$1::uuid`, p.GridID).Scan(&gridRev)
	_ = e.pool.QueryRow(ctx, `SELECT COALESCE(revision_id::text,'') FROM model.metric_def WHERE id=$1::uuid`, p.MetricID).Scan(&metricRev)
	if gridRev != metricRev {
		return "", "", fmt.Errorf("metric does not belong to the same revision as the grid")
	}

	// A metric may only belong to one grid at a time. Block the add (rather than
	// silently moving it) and name the grid that already owns it so the developer
	// can decide whether to remove it there first.
	var existingGridName string
	err = e.pool.QueryRow(ctx, `
		SELECT gd.name FROM model.grid_metric gm
		JOIN model.grid_def gd ON gd.id = gm.grid_id
		WHERE gm.metric_id=$1::uuid AND gm.grid_id!=$2::uuid
		LIMIT 1
	`, p.MetricID, p.GridID).Scan(&existingGridName)
	if err == nil {
		return "", "", fmt.Errorf("metric is already in grid %q — a metric can only belong to one grid; remove it there first", existingGridName)
	}

	var sortOrder int
	_ = e.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sort_order),0)+1 FROM model.grid_metric WHERE grid_id=$1::uuid`, p.GridID).Scan(&sortOrder)
	if err := e.gridMembershipTx(ctx, p.GridID, `
		INSERT INTO model.grid_metric (grid_id, metric_id, sort_order)
		VALUES ($1::uuid, $2::uuid, $3) ON CONFLICT DO NOTHING
	`, p.GridID, p.MetricID, sortOrder); err != nil {
		return "", "", fmt.Errorf("add grid metric: %w", err)
	}
	return "Metric added to grid", "", nil
}

// gridMembershipTx applies a grid membership change and re-runs the
// revision's time validation in the same transaction — the AI Developer's
// twin of the developer console's grid handlers, so a grid can never gain a
// second time dimension or strand a time-series metric off its axis.
func (e *WriteExecutor) gridMembershipTx(ctx context.Context, gridID, sql string, args ...any) error {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return err
	}
	var modelID, revisionID string
	if err := tx.QueryRow(ctx,
		`SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.grid_def WHERE id=$1::uuid`, gridID,
	).Scan(&modelID, &revisionID); err != nil {
		return err
	}
	if err := metricformula.ValidateGridTime(ctx, tx, modelID, revisionID, gridID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── add_grid_dimension ────────────────────────────────────────────────────────

func (e *WriteExecutor) addGridDimension(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		GridID      string `json:"grid_id"`
		DimensionID string `json:"dimension_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.GridID == "" || p.DimensionID == "" {
		return "", "", fmt.Errorf("grid_id and dimension_id are required")
	}
	mappedGridID, err := e.requireInModel(ctx, "grid", p.GridID)
	if err != nil {
		return "", "", err
	}
	p.GridID = mappedGridID
	mappedDimID, err := e.requireInModel(ctx, "dimension", p.DimensionID)
	if err != nil {
		return "", "", err
	}
	p.DimensionID = mappedDimID
	if err := e.gridMembershipTx(ctx, p.GridID, `
		INSERT INTO model.grid_dimension (grid_id, dimension_id)
		VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING
	`, p.GridID, p.DimensionID); err != nil {
		return "", "", fmt.Errorf("add grid dimension: %w", err)
	}
	return "Dimension added to grid", "", nil
}

// ── create_dashboard ──────────────────────────────────────────────────────────

func (e *WriteExecutor) createDashboard(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Name       string   `json:"name"`
		Tags       []string `json:"tags"`
		RevisionID string   `json:"revision_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	if p.Tags == nil {
		p.Tags = []string{}
	}
	revID := e.effectiveRevision(p.RevisionID)
	if revID == "" {
		_ = e.pool.QueryRow(ctx, `SELECT COALESCE(active_revision_id::text,'') FROM core.model WHERE id=$1::uuid`, e.modelID).Scan(&revID)
	}

	var newID string
	var err error
	if revID != "" {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.dashboard_def (model_id, name, tags, revision_id)
			VALUES ($1::uuid, $2, $3, $4::uuid) RETURNING id::text
		`, e.modelID, p.Name, p.Tags, revID).Scan(&newID)
	} else {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.dashboard_def (model_id, name, tags)
			VALUES ($1::uuid, $2, $3) RETURNING id::text
		`, e.modelID, p.Name, p.Tags).Scan(&newID)
	}
	if err != nil {
		return "", "", fmt.Errorf("insert dashboard: %w", err)
	}
	return fmt.Sprintf("Dashboard '%s' created (id: %s)", p.Name, newID), newID, nil
}

// ── add_dashboard_widget ──────────────────────────────────────────────────────

func (e *WriteExecutor) addDashboardWidget(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		DashboardID string          `json:"dashboard_id"`
		WidgetType  string          `json:"widget_type"`
		RefID       *string         `json:"ref_id"`
		Content     *string         `json:"content"`
		PosX        int             `json:"pos_x"`
		PosY        int             `json:"pos_y"`
		SizeW       int             `json:"size_w"`
		SizeH       int             `json:"size_h"`
		WidgetProps json.RawMessage `json:"widget_props"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.DashboardID == "" || p.WidgetType == "" {
		return "", "", fmt.Errorf("dashboard_id and widget_type are required")
	}
	mappedDashID, err := e.requireInModel(ctx, "dashboard", p.DashboardID)
	if err != nil {
		return "", "", err
	}
	p.DashboardID = mappedDashID
	// The typed refs get the same in-model + cross-revision resolution as the
	// dashboard itself: a widget storing another revision's UUID is exactly
	// the "dashboard renders blank" failure the console shows when refs go
	// stale — the widget looks configured but resolves to nothing at render
	// time. Widget types whose ref is not a metric/grid (forms, automation
	// rules, integrations) pass through unchanged, as before.
	if p.RefID != nil && *p.RefID != "" {
		if kind, ok := map[string]string{"metric_kpi": "metric", "chart": "grid", "grid": "grid"}[p.WidgetType]; ok {
			mappedRef, refErr := e.requireInModel(ctx, kind, *p.RefID)
			if refErr != nil {
				return "", "", refErr
			}
			p.RefID = &mappedRef
		}
	}
	// A chart's widget_props carry their own UUIDs (plotted dimension and
	// series metrics); remap them the same way or the chart body would query
	// the wrong revision even when ref_id is right.
	if p.WidgetType == "chart" && len(p.WidgetProps) > 0 && string(p.WidgetProps) != "null" {
		remapped, propErr := e.remapChartProps(ctx, p.WidgetProps)
		if propErr != nil {
			return "", "", propErr
		}
		p.WidgetProps = remapped
	}
	if p.SizeW < 20 {
		p.SizeW = 200
	}
	if p.SizeH < 20 {
		p.SizeH = 60
	}
	var propsStr *string
	if len(p.WidgetProps) > 0 && string(p.WidgetProps) != "null" {
		s := string(p.WidgetProps)
		propsStr = &s
	}

	var newID string
	err = e.pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_widget
		  (dashboard_id, widget_type, ref_id, content, pos_x, pos_y, size_w, size_h, widget_props, sort_order, col_start, col_span)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, 0, 1, 12)
		RETURNING id::text
	`, p.DashboardID, p.WidgetType, p.RefID, p.Content,
		p.PosX, p.PosY, p.SizeW, p.SizeH, propsStr).Scan(&newID)
	if err != nil {
		return "", "", fmt.Errorf("insert widget: %w", err)
	}
	return fmt.Sprintf("Widget '%s' added to dashboard (id: %s)", p.WidgetType, newID), newID, nil
}

// ── create_revision ───────────────────────────────────────────────────────────

func (e *WriteExecutor) createRevision(ctx context.Context, raw json.RawMessage) (result, createdID string, err error) {
	var p struct {
		Name             string `json:"name"`
		SourceRevisionID string `json:"source_revision_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}

	// Transactional: previously a bare sequence of independent pool.Exec
	// calls (several explicitly "best-effort, ignore error"), which could
	// leave a genuinely inconsistent draft behind on a mid-copy failure —
	// unlike the manual duplicate-revision handler (developerRevisions),
	// hardened into one pgx.Tx for exactly this reason. Every step below
	// now returns its error instead of swallowing it, since a single
	// aborted statement poisons the rest of the transaction anyway.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var newID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO model.revision (model_id, name, description)
		VALUES ($1::uuid, $2, '') RETURNING id::text
	`, e.modelID, p.Name).Scan(&newID); err != nil {
		return "", "", fmt.Errorf("insert revision: %w", err)
	}

	// Resolve copy source.
	srcID := p.SourceRevisionID
	if srcID == "" {
		_ = tx.QueryRow(ctx, `
			SELECT COALESCE(active_revision_id::text,'') FROM core.model WHERE id=$1::uuid
		`, e.modelID).Scan(&srcID)
	}
	if srcID != "" {
		// Copy metrics
		if _, err := tx.Exec(ctx, `
			INSERT INTO model.metric_def (model_id, name, formula, is_input, revision_id, format, format_decimals, format_currency, agg_rule, time_summary)
			SELECT model_id, name, formula, is_input, $2::uuid, format, format_decimals, format_currency, agg_rule, time_summary
			FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$3::uuid
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("copy metrics into new revision: %w", err)
		}

		// Remap agg_rule='rate' operands onto the new revision's own copies.
		// The INSERT above deliberately omits agg_numerator/denominator_
		// metric_id — the rows they must point at do not exist until it has
		// run, and copying the old ids verbatim would leave the ratio reading
		// the SOURCE revision's inputs. Before 2026-08-25 this path did
		// neither: the operands were silently dropped, so every copied rate
		// metric failed every recalculation with "rate needs both a numerator
		// and a denominator metric" — found live when the Test app's
		// avg_price went blank after a promote-then-edit cycle. Matched by
		// name, mirroring Step A3 of the developer-console duplicate handler
		// (internal/gateway/handler.go), which got this right all along.
		if _, err := tx.Exec(ctx, `
			UPDATE model.metric_def nm
			SET agg_numerator_metric_id = (
			        SELECT n.id FROM model.metric_def n
			        WHERE n.model_id = nm.model_id AND n.revision_id = nm.revision_id
			          AND n.name = (SELECT o.name FROM model.metric_def o WHERE o.id = om.agg_numerator_metric_id)
			    ),
			    agg_denominator_metric_id = (
			        SELECT n.id FROM model.metric_def n
			        WHERE n.model_id = nm.model_id AND n.revision_id = nm.revision_id
			          AND n.name = (SELECT o.name FROM model.metric_def o WHERE o.id = om.agg_denominator_metric_id)
			    )
			FROM model.metric_def om
			WHERE nm.model_id = $1::uuid AND nm.revision_id = $2::uuid
			  AND om.model_id = $1::uuid AND om.revision_id = $3::uuid
			  AND om.name = nm.name
			  AND (om.agg_numerator_metric_id IS NOT NULL OR om.agg_denominator_metric_id IS NOT NULL)
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("remap rate operands into new revision: %w", err)
		}

		// Copy calc_dependency (formula dependency edges), re-resolving both
		// endpoints by name the same way new_grid_metrics below re-resolves
		// its metric — without this, every calc metric in the new draft
		// keeps its formula text but loses its dependency edges, so the
		// Dependency Graph tab renders it with no lines to what it actually
		// depends on and the formula-integrity checks lose visibility into
		// it. Mirrors the equivalent CTE step in developerRevisionAction
		// (internal/gateway/handler.go) — the developer-console "Duplicate
		// revision" action already does this; this AI-draft path didn't.
		if _, err := tx.Exec(ctx, `
			INSERT INTO model.calc_dependency
			    (metric_id, depends_on_metric_id, min_time_offset, max_time_offset, unbounded_past, unbounded_future)
			SELECT new_m.id, new_dep.id, cd.min_time_offset, cd.max_time_offset, cd.unbounded_past, cd.unbounded_future
			FROM model.calc_dependency cd
			JOIN model.metric_def old_m   ON old_m.id = cd.metric_id             AND old_m.revision_id = $3::uuid
			JOIN model.metric_def old_dep ON old_dep.id = cd.depends_on_metric_id AND old_dep.revision_id = $3::uuid
			JOIN model.metric_def new_m   ON new_m.model_id = old_m.model_id     AND new_m.name = old_m.name     AND new_m.revision_id = $2::uuid
			JOIN model.metric_def new_dep ON new_dep.model_id = old_dep.model_id AND new_dep.name = old_dep.name AND new_dep.revision_id = $2::uuid
			WHERE old_m.model_id = $1::uuid
			ON CONFLICT DO NOTHING
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("copy calc dependencies into new revision: %w", err)
		}

		// Copy dimensions, their members, and grids — mirrors the developer-console
		// revision-duplication handler in internal/gateway/handler.go.
		//
		// parent_dimension_id / parent_member_id are deliberately NOT remapped in
		// this same statement: within one WITH query every sub-statement shares a
		// single pre-statement snapshot, so an UPDATE CTE here could never see the
		// rows new_dims/new_members insert moments earlier in the SAME statement —
		// that remap runs as a separate Exec below, once these inserts have committed.
		if _, err := tx.Exec(ctx, `
			WITH
			new_dims AS (
				INSERT INTO model.dimension_def (model_id, name, agg_rule, properties, revision_id, source_property,
				                                 dimension_type, time_granularity, fiscal_year_start_month)
				SELECT model_id, name, agg_rule, properties, $2::uuid, source_property,
				       dimension_type, time_granularity, fiscal_year_start_month
				FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$3::uuid
				RETURNING id AS new_id, name
			),
			dim_map AS (
				SELECT o.id AS old_id, n.new_id
				FROM model.dimension_def o
				JOIN new_dims n ON n.name = o.name
				WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
			),
			new_members AS (
				INSERT INTO model.dimension_member (dimension_id, code, label, properties, sort_order, period_start, period_end, time_index)
				SELECT dm.new_id, m.code, m.label, m.properties, m.sort_order, m.period_start, m.period_end, m.time_index
				FROM model.dimension_member m
				JOIN dim_map dm ON dm.old_id = m.dimension_id
				RETURNING id
			),
			new_grids AS (
				INSERT INTO model.grid_def (model_id, name, revision_id)
				SELECT model_id, name, $2::uuid
				FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$3::uuid
				RETURNING id AS new_id, name
			),
			new_grid_metrics AS (
				INSERT INTO model.grid_metric (grid_id, metric_id, sort_order)
				SELECT ng.new_id, new_m.id, gm.sort_order
				FROM model.grid_metric gm
				JOIN model.grid_def old_g ON old_g.id = gm.grid_id AND old_g.revision_id = $3::uuid
				JOIN new_grids ng ON ng.name = old_g.name
				JOIN model.metric_def old_m ON old_m.id = gm.metric_id
				JOIN model.metric_def new_m ON new_m.model_id = old_m.model_id
				                          AND new_m.name = old_m.name
				                          AND new_m.revision_id = $2::uuid
				RETURNING grid_id
			),
			new_grid_dims AS (
				INSERT INTO model.grid_dimension (grid_id, dimension_id, display_level)
				SELECT ng.new_id, dm.new_id, gd.display_level
				FROM model.grid_dimension gd
				JOIN model.grid_def old_g ON old_g.id = gd.grid_id AND old_g.revision_id = $3::uuid
				JOIN new_grids ng ON ng.name = old_g.name
				JOIN dim_map dm ON dm.old_id = gd.dimension_id
				RETURNING grid_id
			)
			SELECT
				(SELECT count(*) FROM new_members) +
				(SELECT count(*) FROM new_grid_metrics) +
				(SELECT count(*) FROM new_grid_dims)
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("copy dimensions/grids into new revision: %w", err)
		}

		// Remap parent_dimension_id / parent_member_id now that the inserts above
		// are visible within the same transaction. A remap failure now aborts
		// (and rolls back) the whole draft, rather than silently leaving a
		// dimension hierarchy half-remapped.
		if _, err := tx.Exec(ctx, `
			WITH dim_map AS (
				SELECT o.id AS old_id, n.id AS new_id
				FROM model.dimension_def o
				JOIN model.dimension_def n ON n.name = o.name AND n.revision_id = $2::uuid
				WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
			),
			dim_parent_fix AS (
				UPDATE model.dimension_def nd
				SET parent_dimension_id = pm.new_id
				FROM model.dimension_def od
				JOIN dim_map dm ON dm.old_id = od.id
				JOIN dim_map pm ON pm.old_id = od.parent_dimension_id
				WHERE nd.id = dm.new_id AND od.parent_dimension_id IS NOT NULL
				RETURNING nd.id
			),
			-- A copied revision must be self-contained (no references back
			-- into the source revision), so source_dimension_id (the
			-- property-derived-dimension link) is remapped the same way
			-- parent_dimension_id is above. Mirrors Step A2's dim_source_fix
			-- in the developer-console duplicate-revision handler.
			dim_source_fix AS (
				UPDATE model.dimension_def nd
				SET source_dimension_id = sm.new_id
				FROM model.dimension_def od
				JOIN dim_map dm ON dm.old_id = od.id
				JOIN dim_map sm ON sm.old_id = od.source_dimension_id
				WHERE nd.id = dm.new_id AND od.source_dimension_id IS NOT NULL
				RETURNING nd.id
			),
			member_map AS (
				SELECT om.id AS old_id, nm.id AS new_id
				FROM model.dimension_member om
				JOIN dim_map dm ON dm.old_id = om.dimension_id
				JOIN model.dimension_member nm ON nm.dimension_id = dm.new_id AND nm.code = om.code
			),
			member_parent_fix AS (
				UPDATE model.dimension_member nmem
				SET parent_member_id = pmm.new_id
				FROM model.dimension_member omem
				JOIN member_map mm ON mm.old_id = omem.id
				JOIN member_map pmm ON pmm.old_id = omem.parent_member_id
				WHERE nmem.id = mm.new_id AND omem.parent_member_id IS NOT NULL
				RETURNING nmem.id
			)
			SELECT (SELECT count(*) FROM dim_parent_fix) + (SELECT count(*) FROM dim_source_fix) + (SELECT count(*) FROM member_parent_fix)
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("remap dimension hierarchy into new revision: %w", err)
		}

		// Remap rollup_source_grid_id now that both the referenced and
		// referencing grids exist as new-revision rows (same snapshot-
		// visibility reasoning as above). Mirrors Step C2 of the
		// developer-console duplicate-revision handler — without this, a
		// copied rollup grid still points at the SOURCE revision's grid,
		// breaking self-containment.
		if _, err := tx.Exec(ctx, `
			WITH grid_map AS (
				SELECT o.id AS old_id, n.id AS new_id
				FROM model.grid_def o
				JOIN model.grid_def n ON n.name = o.name AND n.revision_id = $2::uuid
				WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
			)
			UPDATE model.grid_def ng
			SET rollup_source_grid_id = sm.new_id
			FROM model.grid_def og
			JOIN grid_map gm ON gm.old_id = og.id
			JOIN grid_map sm ON sm.old_id = og.rollup_source_grid_id
			WHERE ng.id = gm.new_id AND og.rollup_source_grid_id IS NOT NULL
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("remap grid rollup-source into new revision: %w", err)
		}

		// Copy dimension properties (typed member-attribute schemas,
		// model.dimension_property — distinct from the properties JSONB
		// copied with dimension_def above). Mirrors Step D of the
		// developer-console duplicate-revision handler — this AI-draft
		// path never copied these at all before.
		if _, err := tx.Exec(ctx, `
			INSERT INTO model.dimension_property (dimension_id, name, data_type)
			SELECT nd.id, p.name, p.data_type
			FROM model.dimension_property p
			JOIN model.dimension_def od ON od.id = p.dimension_id
			JOIN model.dimension_def nd ON nd.model_id = od.model_id AND nd.name = od.name AND nd.revision_id = $2::uuid
			WHERE od.model_id=$1::uuid AND od.revision_id=$3::uuid
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("copy dimension properties into new revision: %w", err)
		}

		// Copy user-entered fact values (runtime.fact_input). source_ref
		// and entered_at are copied verbatim, not defaulted — dropping
		// them would misclassify every copied form-posted row as
		// direct-entry, the same B1 hazard fixed in the developer-console
		// handler this pass. This AI-draft path never copied facts at all
		// before, so an AI-created draft revision started with every
		// input metric blank.
		if _, err := tx.Exec(ctx, `
			INSERT INTO runtime.fact_input
			  (model_id, revision_name, metric_id, dim_members, value, entered_by, revision_id, source_ref, entered_at)
			SELECT
				fi.model_id, fi.revision_name,
				new_m.id,
				(SELECT COALESCE(jsonb_object_agg(new_d.id::text, kv.val), '{}'::jsonb)
				 FROM jsonb_each_text(fi.dim_members) kv(old_key, val)
				 JOIN model.dimension_def old_d ON old_d.id::text = kv.old_key
				 JOIN model.dimension_def new_d ON new_d.model_id = old_d.model_id
				                               AND new_d.name = old_d.name
				                               AND new_d.revision_id = $2::uuid),
				fi.value, fi.entered_by,
				$2::uuid,
				fi.source_ref, fi.entered_at
			FROM runtime.fact_input fi
			JOIN model.metric_def old_m ON old_m.id = fi.metric_id
			JOIN model.metric_def new_m ON new_m.model_id = old_m.model_id
			                           AND new_m.name = old_m.name
			                           AND new_m.revision_id = $2::uuid
			WHERE fi.revision_id = $3::uuid AND fi.model_id = $1::uuid
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("copy facts into new revision: %w", err)
		}

		// Copy integrations, remapping the target to the new revision's
		// copy of the grid/dashboard/form it points at. Mirrors Step G of
		// the developer-console duplicate-revision handler — this
		// AI-draft path never copied integrations at all before.
		if _, err := tx.Exec(ctx, `
			INSERT INTO model.integration_def (model_id, name, type, target_type, target_id, config, revision_id)
			SELECT i.model_id, i.name, i.type, i.target_type,
				COALESCE(
					(SELECT ng.id FROM model.grid_def og
					 JOIN model.grid_def ng ON ng.model_id = og.model_id AND ng.name = og.name AND ng.revision_id = $2::uuid
					 WHERE og.id = i.target_id AND i.target_type = 'grid'),
					(SELECT ndd.id FROM model.dashboard_def odd
					 JOIN model.dashboard_def ndd ON ndd.model_id = odd.model_id AND ndd.name = odd.name AND ndd.revision_id = $2::uuid
					 WHERE odd.id = i.target_id AND i.target_type = 'dashboard'),
					(SELECT nf.id FROM model.form_def ofd
					 JOIN model.form_def nf ON nf.model_id = ofd.model_id AND nf.name = ofd.name AND nf.revision_id = $2::uuid
					 WHERE ofd.id = i.target_id AND i.target_type = 'form'),
					i.target_id),
				i.config, $2::uuid
			FROM model.integration_def i
			WHERE i.model_id=$1::uuid AND i.revision_id=$3::uuid
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("copy integrations into new revision: %w", err)
		}

		// Copy forms, their records, and form-metric mappings. Field
		// definitions embed dimension_id/metric_id refs and mapping rows embed
		// form/grid/metric/dimension refs — all remapped by name join against
		// the rows already inserted above. Unresolvable refs (e.g. a field
		// pointing at a dimension deleted from the source revision) are kept
		// as-is rather than dropped. Mirrors Step E of the developer-console
		// duplicate-revision handler (internal/gateway/handler.go) verbatim —
		// this AI-draft path didn't copy forms at all before, so
		// update_form_def could only ever find forms created earlier in the
		// same session, never ones that predate it.
		if _, err := tx.Exec(ctx, `
			WITH
			dim_map AS (
				SELECT o.id AS old_id, n.id AS new_id
				FROM model.dimension_def o
				JOIN model.dimension_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
				WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
			),
			metric_map AS (
				SELECT o.id AS old_id, n.id AS new_id
				FROM model.metric_def o
				JOIN model.metric_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
				WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
			),
			new_forms AS (
				INSERT INTO model.form_def (model_id, name, label, fields, revision_id)
				SELECT f.model_id, f.name, f.label,
					COALESCE((
						SELECT jsonb_agg(
							e.elem
							|| COALESCE((SELECT jsonb_build_object('dimension_id', dm.new_id::text) FROM dim_map dm WHERE dm.old_id::text = e.elem->>'dimension_id'), '{}'::jsonb)
							|| COALESCE((SELECT jsonb_build_object('metric_id', mm.new_id::text) FROM metric_map mm WHERE mm.old_id::text = e.elem->>'metric_id'), '{}'::jsonb)
							ORDER BY e.ord)
						FROM jsonb_array_elements(CASE WHEN jsonb_typeof(f.fields) = 'array' THEN f.fields ELSE '[]'::jsonb END) WITH ORDINALITY AS e(elem, ord)
					), '[]'::jsonb),
					$2::uuid
				FROM model.form_def f
				WHERE f.model_id=$1::uuid AND f.revision_id=$3::uuid
				RETURNING id AS new_id, name
			),
			form_map AS (
				SELECT o.id AS old_id, n.new_id
				FROM model.form_def o
				JOIN new_forms n ON n.name = o.name
				WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
			),
			new_records AS (
				INSERT INTO runtime.form_record (form_id, data, status, created_by)
				SELECT fm.new_id, rec.data, rec.status, rec.created_by
				FROM runtime.form_record rec
				JOIN form_map fm ON fm.old_id = rec.form_id
				RETURNING id
			),
			new_mappings AS (
				INSERT INTO model.form_metric_mapping
				  (model_id, form_id, grid_id, name, source_field, target_metric_id, aggregation,
				   posting_statuses, dimension_mappings, live_posting, revision_id)
				SELECT fmm.model_id, fm.new_id,
					(SELECT ng.id FROM model.grid_def og
					 JOIN model.grid_def ng ON ng.model_id = og.model_id AND ng.name = og.name AND ng.revision_id = $2::uuid
					 WHERE og.id = fmm.grid_id),
					fmm.name, fmm.source_field,
					mm.new_id, fmm.aggregation, fmm.posting_statuses,
					COALESCE((
						SELECT jsonb_object_agg(COALESCE(dm.new_id::text, kv.key), kv.value)
						FROM jsonb_each(fmm.dimension_mappings) kv
						LEFT JOIN dim_map dm ON dm.old_id::text = kv.key
					), '{}'::jsonb),
					fmm.live_posting, $2::uuid
				FROM model.form_metric_mapping fmm
				JOIN form_map fm ON fm.old_id = fmm.form_id
				JOIN metric_map mm ON mm.old_id = fmm.target_metric_id
				RETURNING id
			)
			SELECT
				(SELECT count(*) FROM new_forms) +
				(SELECT count(*) FROM new_records) +
				(SELECT count(*) FROM new_mappings)
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("copy forms into new revision: %w", err)
		}

		// Copy dashboard folders first (dashboards below reference them by
		// folder_id), then remap parent_id in a separate Exec once the new
		// rows are visible. Mirrors Step F of the developer-console
		// duplicate-revision handler — this AI-draft path never copied
		// folders or dashboard.category/folder_id at all before.
		if _, err := tx.Exec(ctx, `
			INSERT INTO model.dashboard_folder (model_id, name, revision_id)
			SELECT model_id, name, $2::uuid
			FROM model.dashboard_folder WHERE model_id=$1::uuid AND revision_id=$3::uuid
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("copy dashboard folders into new revision: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			WITH folder_map AS (
				SELECT o.id AS old_id, n.id AS new_id
				FROM model.dashboard_folder o
				JOIN model.dashboard_folder n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
				WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
			)
			UPDATE model.dashboard_folder nf
			SET parent_id = pm.new_id
			FROM model.dashboard_folder ofo
			JOIN folder_map fmap ON fmap.old_id = ofo.id
			JOIN folder_map pm ON pm.old_id = ofo.parent_id
			WHERE nf.id = fmap.new_id AND ofo.parent_id IS NOT NULL
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("remap dashboard folder parents into new revision: %w", err)
		}

		// Copy dashboards + widgets
		rows, err := tx.Query(ctx, `
			SELECT id::text, name, tags, category, folder_id::text FROM model.dashboard_def
			WHERE model_id=$1::uuid AND revision_id=$2::uuid
		`, e.modelID, srcID)
		if err != nil {
			return "", "", fmt.Errorf("query dashboards to copy: %w", err)
		}
		type dashToCopy struct {
			id, name, category string
			tags               []string
			folderID           *string
		}
		var dashRows []dashToCopy
		for rows.Next() {
			var d dashToCopy
			if err := rows.Scan(&d.id, &d.name, &d.tags, &d.category, &d.folderID); err != nil {
				rows.Close()
				return "", "", fmt.Errorf("scan dashboard to copy: %w", err)
			}
			dashRows = append(dashRows, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", "", fmt.Errorf("iterate dashboards to copy: %w", err)
		}
		// rows must be fully consumed and closed above before issuing more
		// queries on the same tx — pgx serializes all statements in a
		// transaction onto one connection.
		for _, d := range dashRows {
			var newDashID string
			if err := tx.QueryRow(ctx, `
				INSERT INTO model.dashboard_def (model_id, name, tags, category, revision_id, folder_id)
				VALUES ($1::uuid, $2, $3, $4, $5::uuid,
					(SELECT nf.id FROM model.dashboard_folder ofd
					 JOIN model.dashboard_folder nf ON nf.model_id = ofd.model_id AND nf.name = ofd.name AND nf.revision_id = $5::uuid
					 WHERE ofd.id = $6::uuid))
				RETURNING id::text
			`, e.modelID, d.name, d.tags, d.category, newID, d.folderID).Scan(&newDashID); err != nil {
				return "", "", fmt.Errorf("copy dashboard %q: %w", d.name, err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO model.dashboard_widget
				  (dashboard_id, widget_type, ref_id, content, title, show_title, sort_order, col_start, col_span, pos_x, pos_y, size_w, size_h, widget_props)
				SELECT $2::uuid, widget_type, ref_id, content, title, show_title, sort_order, col_start, col_span, pos_x, pos_y, size_w, size_h, widget_props
				FROM model.dashboard_widget WHERE dashboard_id=$1::uuid
			`, d.id, newDashID); err != nil {
				return "", "", fmt.Errorf("copy widgets for dashboard %q: %w", d.name, err)
			}
		}

		// Copy workflow definitions and automation rules (application-scoped,
		// resolved through this model's application). context_schema entries
		// binding a context variable to a dimension are remapped to the new
		// revision's dimension; rules' workflow/form/grid refs are remapped
		// the same way. Mirrors Step H of the developer-console
		// duplicate-revision handler verbatim — an AI-created workflow needs
		// an automation rule to ever fire, and update_workflow_def can only
		// find a pre-session workflow if this copy step exists.
		if _, err := tx.Exec(ctx, `
			WITH
			dim_map AS (
				SELECT o.id AS old_id, n.id AS new_id
				FROM model.dimension_def o
				JOIN model.dimension_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
				WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
			),
			app AS (SELECT application_id FROM core.model WHERE id=$1::uuid),
			new_wfs AS (
				INSERT INTO workflow.workflow_def
				  (application_id, name, description, trigger_event, subject_type, subject_config, steps,
				   status, created_by, updated_by, published_at, archived_at, context_schema, revision_id)
				SELECT wd.application_id, wd.name, wd.description, wd.trigger_event, wd.subject_type, wd.subject_config, wd.steps,
					wd.status, wd.created_by, wd.updated_by, wd.published_at, wd.archived_at,
					COALESCE((
						SELECT jsonb_agg(
							e.elem
							|| COALESCE((SELECT jsonb_build_object('dimension_id', dm.new_id::text) FROM dim_map dm WHERE dm.old_id::text = e.elem->>'dimension_id'), '{}'::jsonb)
							ORDER BY e.ord)
						FROM jsonb_array_elements(wd.context_schema) WITH ORDINALITY AS e(elem, ord)
					), '[]'::jsonb),
					$2::uuid
				FROM workflow.workflow_def wd
				WHERE wd.application_id = (SELECT application_id FROM app)
				  AND wd.revision_id = $3::uuid
				RETURNING id AS new_id, name
			),
			wf_map AS (
				SELECT o.id AS old_id, n.new_id
				FROM workflow.workflow_def o
				JOIN new_wfs n ON n.name = o.name
				WHERE o.application_id = (SELECT application_id FROM app)
				  AND o.revision_id = $3::uuid
			),
			new_rules AS (
				INSERT INTO workflow.automation_rule
				  (application_id, name, description, trigger_type, workflow_name, enabled,
				   workflow_def_id, source_form_id, source_grid_id, revision_id)
				SELECT ar.application_id, ar.name, ar.description, ar.trigger_type, ar.workflow_name, ar.enabled,
					wm.new_id,
					(SELECT nf.id FROM model.form_def ofd
					 JOIN model.form_def nf ON nf.model_id = ofd.model_id AND nf.name = ofd.name AND nf.revision_id = $2::uuid
					 WHERE ofd.id = ar.source_form_id),
					(SELECT ng.id FROM model.grid_def og
					 JOIN model.grid_def ng ON ng.model_id = og.model_id AND ng.name = og.name AND ng.revision_id = $2::uuid
					 WHERE og.id = ar.source_grid_id),
					$2::uuid
				FROM workflow.automation_rule ar
				LEFT JOIN wf_map wm ON wm.old_id = ar.workflow_def_id
				WHERE ar.application_id = (SELECT application_id FROM app)
				  AND ar.revision_id = $3::uuid
				RETURNING id
			)
			SELECT (SELECT count(*) FROM new_wfs) + (SELECT count(*) FROM new_rules)
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("copy workflows/automation rules into new revision: %w", err)
		}

		// Remap dashboard_widget.ref_id to the new revision's copy of
		// whatever it points at. Must run last, now that grids/forms/
		// metrics/integrations/automation rules all have their
		// new-revision copies — mirrors the developer-console
		// duplicate-revision handler's own Step I, fixed the same pass,
		// so the two paths don't immediately re-diverge on the exact bug
		// just fixed in the other one. "import" widgets resolve against
		// grid_def too, not integration_def — confirmed against
		// web/src/consoles/business/DashboardWidgets.tsx's ImportWidget.
		if _, err := tx.Exec(ctx, `
			WITH
			app AS (SELECT application_id FROM core.model WHERE id=$1::uuid),
			dash_map AS (
				SELECT od.id AS old_id, nd.id AS new_id
				FROM model.dashboard_def od
				JOIN model.dashboard_def nd ON nd.model_id = od.model_id AND nd.name = od.name AND nd.revision_id = $2::uuid
				WHERE od.model_id=$1::uuid AND od.revision_id=$3::uuid
			),
			widget_map AS (
				SELECT nw.id AS new_widget_id, ow.widget_type, ow.ref_id AS old_ref_id
				FROM model.dashboard_widget ow
				JOIN dash_map dm ON dm.old_id = ow.dashboard_id
				JOIN model.dashboard_widget nw ON nw.dashboard_id = dm.new_id
				                               AND nw.widget_type = ow.widget_type
				                               AND nw.sort_order = ow.sort_order
				WHERE ow.ref_id IS NOT NULL AND ow.ref_id <> ''
			),
			remap AS (
				UPDATE model.dashboard_widget w
				SET ref_id = COALESCE(
					(SELECT ng.id::text FROM model.grid_def og
					 JOIN model.grid_def ng ON ng.model_id = og.model_id AND ng.name = og.name AND ng.revision_id = $2::uuid
					 WHERE og.id::text = wm.old_ref_id AND wm.widget_type IN ('grid', 'chart', 'import')),
					(SELECT nf.id::text FROM model.form_def ofd
					 JOIN model.form_def nf ON nf.model_id = ofd.model_id AND nf.name = ofd.name AND nf.revision_id = $2::uuid
					 WHERE ofd.id::text = wm.old_ref_id AND wm.widget_type = 'form'),
					(SELECT nm.id::text FROM model.metric_def om
					 JOIN model.metric_def nm ON nm.model_id = om.model_id AND nm.name = om.name AND nm.revision_id = $2::uuid
					 WHERE om.id::text = wm.old_ref_id AND wm.widget_type = 'metric_kpi'),
					(SELECT ni.id::text FROM model.integration_def oi
					 JOIN model.integration_def ni ON ni.model_id = oi.model_id AND ni.name = oi.name AND ni.revision_id = $2::uuid
					 WHERE oi.id::text = wm.old_ref_id AND wm.widget_type = 'integration_button'),
					(SELECT nr.id::text FROM workflow.automation_rule oar
					 JOIN workflow.automation_rule nr ON nr.application_id = oar.application_id AND nr.name = oar.name AND nr.revision_id = $2::uuid
					 WHERE oar.id::text = wm.old_ref_id AND wm.widget_type = 'automation_button'
					   AND oar.application_id = (SELECT application_id FROM app)),
					wm.old_ref_id
				)
				FROM widget_map wm
				WHERE w.id = wm.new_widget_id
				RETURNING w.id
			)
			SELECT count(*) FROM remap
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("remap dashboard widget refs into new revision: %w", err)
		}

		// widget_props carries metric/dimension IDs of its own (chart series
		// and plotted dimension, chart context defaults, kpi_scope) that the
		// ref_id remap above doesn't reach. Same fix, same pass, and for the
		// same reason as the ref_id remap: the developer-console duplicate
		// path (duplicateRevision Step J) does this too, and these two copies
		// must not diverge.
		metricIDMap, err := revisionIDMap(ctx, tx,
			`SELECT o.id::text, n.id::text FROM model.metric_def o
			 JOIN model.metric_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
			 WHERE o.model_id = $1::uuid AND o.revision_id = $3::uuid`, e.modelID, newID, srcID)
		if err != nil {
			return "", "", fmt.Errorf("map metrics for widget props: %w", err)
		}
		dimIDMap, err := revisionIDMap(ctx, tx,
			`SELECT o.id::text, n.id::text FROM model.dimension_def o
			 JOIN model.dimension_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
			 WHERE o.model_id = $1::uuid AND o.revision_id = $3::uuid`, e.modelID, newID, srcID)
		if err != nil {
			return "", "", fmt.Errorf("map dimensions for widget props: %w", err)
		}
		type widgetPropRow struct {
			id    string
			props []byte
		}
		var propRowsOut []widgetPropRow
		propRows, err := tx.Query(ctx, `
			SELECT w.id::text, w.widget_props
			FROM model.dashboard_widget w
			JOIN model.dashboard_def d ON d.id = w.dashboard_id
			WHERE d.model_id = $1::uuid AND d.revision_id = $2::uuid AND w.widget_props IS NOT NULL
		`, e.modelID, newID)
		if err != nil {
			return "", "", fmt.Errorf("load widget props to remap: %w", err)
		}
		for propRows.Next() {
			var wp widgetPropRow
			if err := propRows.Scan(&wp.id, &wp.props); err != nil {
				propRows.Close()
				return "", "", fmt.Errorf("scan widget props to remap: %w", err)
			}
			propRowsOut = append(propRowsOut, wp)
		}
		propRows.Close()
		if err := propRows.Err(); err != nil {
			return "", "", fmt.Errorf("load widget props to remap: %w", err)
		}
		for _, wp := range propRowsOut {
			remapped, changed := modeltransfer.RemapWidgetPropsIDs(wp.props, metricIDMap, dimIDMap)
			if !changed {
				continue
			}
			if _, err := tx.Exec(ctx,
				`UPDATE model.dashboard_widget SET widget_props = $2::jsonb WHERE id = $1::uuid`,
				wp.id, string(remapped)); err != nil {
				return "", "", fmt.Errorf("remap widget props into new revision: %w", err)
			}
		}

		// Business-role dashboard grants follow the copies, or activating
		// this revision hides every dashboard from every business user —
		// same reasoning as duplicateRevision's Step K.
		if _, err := tx.Exec(ctx, `
			INSERT INTO identity.business_role_dashboard (role_id, dashboard_id)
			SELECT brd.role_id, nd.id
			FROM identity.business_role_dashboard brd
			JOIN model.dashboard_def od ON od.id = brd.dashboard_id
			JOIN model.dashboard_def nd ON nd.model_id = od.model_id AND nd.name = od.name AND nd.revision_id = $2::uuid
			WHERE od.model_id = $1::uuid AND od.revision_id = $3::uuid
			ON CONFLICT DO NOTHING
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("copy dashboard grants into new revision: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("commit revision copy: %w", err)
	}

	return fmt.Sprintf("Revision '%s' created (id: %s)", p.Name, newID), newID, nil
}

// ── create_workflow_def ───────────────────────────────────────────────────────

type createWorkflowDefParams struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	TriggerEvent string `json:"trigger_event"`
	RevisionID   string `json:"revision_id"`
}

func (e *WriteExecutor) createWorkflowDef(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p createWorkflowDefParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	if p.TriggerEvent == "" {
		p.TriggerEvent = "manual"
	}
	revID := e.effectiveRevision(p.RevisionID)

	var appID string
	if err := e.pool.QueryRow(ctx, `
		SELECT application_id::text FROM core.model WHERE id=$1::uuid
	`, e.modelID).Scan(&appID); err != nil {
		return "", "", fmt.Errorf("resolve application: %w", err)
	}

	ws := workflow.NewStore(e.pool)
	def, err := ws.CreateWorkflowDefFull(ctx, appID, revID, p.Name, p.Description, p.TriggerEvent, e.userID)
	if err != nil {
		return "", "", fmt.Errorf("create workflow: %w", err)
	}
	return fmt.Sprintf(
		"Workflow '%s' created (id: %s, status: draft, no steps yet). Use update_workflow_def to add steps — it starts inert and stays that way until a developer publishes it from the Workflows tab.",
		p.Name, def.ID,
	), def.ID, nil
}

// ── update_workflow_def ───────────────────────────────────────────────────────

type updateWorkflowDefParams struct {
	WorkflowDefID string          `json:"workflow_def_id"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	TriggerEvent  string          `json:"trigger_event"`
	SubjectType   string          `json:"subject_type"`
	Steps         json.RawMessage `json:"steps"`
	ContextSchema json.RawMessage `json:"context_schema"`
	SubjectConfig json.RawMessage `json:"subject_config"`
	// SingleActiveInstance mirrors the console's per-definition dedup
	// switch; nil leaves it unchanged.
	SingleActiveInstance *bool `json:"single_active_instance"`
}

// updateWorkflowDef fetches the current row first for two reasons: (1) to
// verify the target is visible in the session's working revision before
// mutating anything outside its scope, and (2) because
// UpdateWorkflowDefFull always overwrites name/description/trigger_event/
// subject_type (no COALESCE, unlike steps/context_schema/subject_config) —
// an AI call that only means to change steps must not blank these out just
// because it didn't resupply them.
func (e *WriteExecutor) updateWorkflowDef(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p updateWorkflowDefParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.WorkflowDefID == "" {
		return "", "", fmt.Errorf("workflow_def_id is required")
	}

	// Resolve into the working revision (an id read before the draft
	// existed points at the active revision's row; the draft's same-named
	// copy is the one this session may edit) and accept a name in place of
	// the id, both via the resolver every workflow tool shares.
	resolvedID, err := resolveWorkflowDefRef(ctx, e.pool, e.modelID, e.revID, p.WorkflowDefID)
	if err != nil {
		return "", "", err
	}
	p.WorkflowDefID = resolvedID
	var curName, curDescription, curTriggerEvent, curSubjectType string
	if err := e.pool.QueryRow(ctx, `
		SELECT name, COALESCE(description,''), trigger_event, COALESCE(subject_type,'')
		FROM workflow.workflow_def WHERE id=$1::uuid
	`, p.WorkflowDefID).Scan(&curName, &curDescription, &curTriggerEvent, &curSubjectType); err != nil {
		return "", "", fmt.Errorf("workflow %s not found: %w", p.WorkflowDefID, err)
	}

	name := p.Name
	if name == "" {
		name = curName
	}
	description := p.Description
	if description == "" {
		description = curDescription
	}
	triggerEvent := p.TriggerEvent
	if triggerEvent == "" {
		triggerEvent = curTriggerEvent
	}
	subjectType := p.SubjectType
	if subjectType == "" {
		subjectType = curSubjectType
	}

	ws := workflow.NewStore(e.pool)
	def, err := ws.UpdateWorkflowDefFull(ctx, p.WorkflowDefID, name, description, triggerEvent, subjectType, e.userID, p.Steps, p.ContextSchema, p.SubjectConfig)
	if err != nil {
		return "", "", fmt.Errorf("update workflow: %w", err)
	}
	if p.SingleActiveInstance != nil && *p.SingleActiveInstance != def.SingleActiveInstance {
		if err := ws.SetWorkflowDefSingleActiveInstance(ctx, p.WorkflowDefID, *p.SingleActiveInstance); err != nil {
			return "", "", fmt.Errorf("set single_active_instance: %w", err)
		}
		def.SingleActiveInstance = *p.SingleActiveInstance
	}
	var stepCount int
	if arr, uErr := unmarshalArrayLen(def.Steps); uErr == nil {
		stepCount = arr
	}
	// The console saves an incomplete draft too; what it adds is the
	// Validate verdict next to it, so the model learns what a developer
	// would still have to fix rather than discovering it at publish.
	return fmt.Sprintf("Workflow '%s' updated (status: %s, %d step(s), single_active_instance: %v) — %s",
		def.Name, def.Status, stepCount, def.SingleActiveInstance, validationNote(def)), "", nil
}

// unmarshalArrayLen returns len(raw) when raw is a JSON array, else an error.
func unmarshalArrayLen(raw json.RawMessage) (int, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0, err
	}
	return len(arr), nil
}

// ── create_form_def ───────────────────────────────────────────────────────────

type createFormDefParams struct {
	Name       string              `json:"name"`
	Label      string              `json:"label"`
	Fields     []crudapp.FormField `json:"fields"`
	RevisionID string              `json:"revision_id"`
}

func (e *WriteExecutor) createFormDef(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p createFormDefParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	if p.Label == "" {
		p.Label = p.Name
	}
	revID := e.effectiveRevision(p.RevisionID)

	fs := crudapp.NewStore(e.pool)
	form, err := fs.CreateForm(ctx, e.modelID, revID, p.Name, p.Label, p.Fields)
	if err != nil {
		return "", "", fmt.Errorf("create form: %w", err)
	}
	return fmt.Sprintf("Form '%s' created (id: %s, %d field(s))", p.Name, form.ID, len(p.Fields)), form.ID, nil
}

// ── update_form_def ───────────────────────────────────────────────────────────

type updateFormDefParams struct {
	FormID string              `json:"form_id"`
	Name   string              `json:"name"`
	Label  string              `json:"label"`
	Fields []crudapp.FormField `json:"fields"`
}

// updateFormDef fetches the current row first for two reasons: (1) to
// verify the target is visible in the session's working revision, and (2)
// because crudapp.UpdateForm has no partial-update semantics at all (every
// column is unconditionally overwritten, unlike UpdateWorkflowDefFull's
// COALESCE on steps) — an AI call that only means to rename the form must
// not silently wipe its fields just because it didn't resupply them.
func (e *WriteExecutor) updateFormDef(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p updateFormDefParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.FormID == "" {
		return "", "", fmt.Errorf("form_id is required")
	}

	// Forms are copied per revision exactly as workflows are, so an id read
	// before the draft existed is remapped to the draft's same-named copy
	// rather than refused (this path used to refuse, while
	// update_workflow_def remapped — a divergence between two tools that
	// should behave alike).
	resolvedID, err := resolveFormDefRef(ctx, e.pool, e.modelID, e.revID, p.FormID)
	if err != nil {
		return "", "", err
	}
	p.FormID = resolvedID
	var curName, curLabel string
	var curFieldsJSON []byte
	if err := e.pool.QueryRow(ctx, `
		SELECT name, label, fields FROM model.form_def WHERE id=$1::uuid
	`, p.FormID).Scan(&curName, &curLabel, &curFieldsJSON); err != nil {
		return "", "", fmt.Errorf("form %s not found: %w", p.FormID, err)
	}

	name := p.Name
	if name == "" {
		name = curName
	}
	label := p.Label
	if label == "" {
		label = curLabel
	}
	fields := p.Fields
	if fields == nil {
		_ = json.Unmarshal(curFieldsJSON, &fields)
	}

	fs := crudapp.NewStore(e.pool)
	if err := fs.UpdateForm(ctx, p.FormID, name, label, fields); err != nil {
		return "", "", fmt.Errorf("update form: %w", err)
	}
	return fmt.Sprintf("Form '%s' updated (%d field(s))", name, len(fields)), "", nil
}

// ── generate_migration ────────────────────────────────────────────────────────

func (e *WriteExecutor) generateMigration(ctx context.Context) (string, string, error) {
	// Check that there's a pending schema diff to generate.
	var versionNumber int
	err := e.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(version_number),0)+1 FROM model.schema_migration WHERE model_id=$1::uuid
	`, e.modelID).Scan(&versionNumber)
	if err != nil {
		return "", "", fmt.Errorf("query version: %w", err)
	}
	// The heavy lifting (DDL diff) is done by the existing migrationGenerate handler
	// which uses internal migration logic. For the AI executor we call a lightweight
	// check and return a descriptive result — actual generation requires the full
	// handler chain. Signal to the user that they should click "Generate Migration"
	// in the UI for the DDL preview, or we trigger a self-HTTP call.
	return fmt.Sprintf("Migration v%d is ready to generate. Use the Migrations tab to preview the DDL and confirm, or confirm this action to trigger generation.", versionNumber), "", nil
}

// ── apply_migration ───────────────────────────────────────────────────────────

func (e *WriteExecutor) applyMigration(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		VersionNumber int `json:"version_number"`
	}
	_ = json.Unmarshal(raw, &p)
	// Signal that migration apply should be done through the UI for safety.
	return fmt.Sprintf("To apply migration v%d: confirm in the Migrations tab so you can review the DDL first. This action cannot be rolled back automatically.", p.VersionNumber), "", nil
}

// ── set_user_access_rules ─────────────────────────────────────────────────────

// setUserAccessRulesParams identifies everything by NAME — user by email,
// member by dimension name + member code — never by UUID. The one lesson
// every AI-driven build so far has taught is that a language model cannot be
// trusted to transcribe UUIDs (it has emitted placeholders, duplicated ids,
// and once mistyped a pasted UUID by a single character); names are what it
// actually knows, and the executor owns the resolution.
type setUserAccessRulesParams struct {
	UserEmail string `json:"user_email"`
	Rules     []struct {
		Dimension  string `json:"dimension"`
		MemberCode string `json:"member_code"`
		Access     string `json:"access"`
	} `json:"rules"`
}

// setUserAccessRules replaces the target user's entire access-rule set,
// mirroring the business-admin console's PUT /access-rules (delete-then-
// insert) — this is a business-admin capability deliberately extended to the
// AI Developer at the owner's direction (2026-08-26), the first AI tool that
// reaches outside the developer role's own powers.
//
// Scoping: the target user must hold a role assignment in a workspace of the
// SAME CUSTOMER as the application being edited — the same boundary the
// business-admin handler enforces for its caller, applied here to the target
// (the AI session's own actor was only ever authorized against the model).
//
// Members resolve against the ACTIVE revision, not the session draft:
// identity.user_access_rule is runtime enforcement (writeguard reads it
// against live data now), not model authoring — a rule pointing at a draft's
// member copies would do nothing until promote and the draft's ids would be
// wrong for the currently-active data anyway.
func (e *WriteExecutor) setUserAccessRules(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p setUserAccessRulesParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if strings.TrimSpace(p.UserEmail) == "" {
		return "", "", fmt.Errorf("user_email is required")
	}
	for i, r := range p.Rules {
		if r.Access != "read" && r.Access != "hidden" {
			return "", "", fmt.Errorf("rule %d: access must be \"read\" or \"hidden\", got %q", i+1, r.Access)
		}
		if strings.TrimSpace(r.Dimension) == "" || strings.TrimSpace(r.MemberCode) == "" {
			return "", "", fmt.Errorf("rule %d: dimension and member_code are required", i+1)
		}
	}

	// Target user, constrained to the application's customer. Fails closed:
	// an email outside this customer's workspaces resolves to nothing.
	var targetID string
	if err := e.pool.QueryRow(ctx, `
		SELECT u.id::text FROM identity.user u
		WHERE lower(u.email) = lower($2)
		  AND EXISTS (
		      SELECT 1 FROM identity.role_assignment ra
		      JOIN core.workspace w ON w.id = ra.workspace_id
		      WHERE ra.user_id = u.id
		        AND w.customer_id = (
		            SELECT COALESCE(a.customer_id, ws.customer_id)
		            FROM core.model m
		            JOIN core.application a ON a.id = m.application_id
		            LEFT JOIN core.workspace ws ON ws.id = a.workspace_id
		            WHERE m.id = $1::uuid
		        )
		  )
	`, e.modelID, strings.TrimSpace(p.UserEmail)).Scan(&targetID); err != nil {
		return "", "", fmt.Errorf("no user %q in this application's workspaces", p.UserEmail)
	}

	var activeRev string
	if err := e.pool.QueryRow(ctx, `
		SELECT COALESCE(active_revision_id::text,
		       (SELECT id::text FROM model.revision WHERE model_id=$1::uuid ORDER BY created_at LIMIT 1))
		FROM core.model WHERE id=$1::uuid
	`, e.modelID).Scan(&activeRev); err != nil {
		return "", "", fmt.Errorf("resolve active revision: %w", err)
	}

	type resolved struct{ memberID, access, label string }
	rules := make([]resolved, 0, len(p.Rules))
	for _, r := range p.Rules {
		var memberID string
		if err := e.pool.QueryRow(ctx, `
			SELECT m.id::text FROM model.dimension_member m
			JOIN model.dimension_def d ON d.id = m.dimension_id
			WHERE d.model_id = $1::uuid AND lower(d.name) = lower($2)
			  AND (d.revision_id = $3::uuid OR d.revision_id IS NULL)
			  AND m.code = $4
		`, e.modelID, r.Dimension, activeRev, r.MemberCode).Scan(&memberID); err != nil {
			return "", "", fmt.Errorf("no member %q in dimension %q (active revision)", r.MemberCode, r.Dimension)
		}
		rules = append(rules, resolved{memberID, r.Access, r.Dimension + "/" + r.MemberCode})
	}

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`DELETE FROM identity.user_access_rule WHERE user_id=$1::uuid`, targetID); err != nil {
		return "", "", fmt.Errorf("clear existing rules: %w", err)
	}
	for _, r := range rules {
		if _, err := tx.Exec(ctx, `
			INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access)
			VALUES ($1::uuid, 'dimension_member', $2, $3)
		`, targetID, r.memberID, r.access); err != nil {
			return "", "", fmt.Errorf("insert rule for %s: %w", r.label, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("commit rules: %w", err)
	}

	// Same audit event the business-admin endpoint writes, so "who changed
	// this user's access" has one answer regardless of which door was used.
	auditlog.Log(ctx, e.pool, zerolog.Nop(), auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserAccessRulesUpdated,
		ActorUserID: e.userID, ActorRole: "developer(ai_assistant)",
		ResourceType: "identity_user", ResourceID: targetID,
		Metadata: map[string]string{"source": "ai_assistant", "rules": fmt.Sprintf("%d", len(rules))},
	})

	parts := make([]string, len(rules))
	for i, r := range rules {
		parts[i] = r.label + "=" + r.access
	}
	return fmt.Sprintf("Access rules for %s replaced: %s (all previous rules removed; unlisted members stay fully accessible)",
		p.UserEmail, strings.Join(parts, ", ")), "", nil
}
