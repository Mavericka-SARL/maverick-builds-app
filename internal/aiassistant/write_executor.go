package aiassistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/modeledit"
	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/internal/rollup"
	"github.com/mavericks-engine/mavericks/internal/tags"
	"github.com/mavericks-engine/mavericks/internal/timedim"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/dbx"
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
// ExecDB is what a WriteExecutor runs its statements on: the pool, or — for
// a proposal's dry run (NewDryRunWriteExecutor) — a transaction that is
// always rolled back. Every store the executor uses is built on it
// (workflow.NewStoreOn, crudapp.NewStoreOn), so every tool runs in a dry run
// exactly as it would for real.
type ExecDB = dbx.DB

// ErrNotDryRunnable is a dry run's answer for a step it could not run (a
// step that panicked, or a gateway hook the dry run was not given);
// confirming runs it.
var ErrNotDryRunnable = errors.New("checked only when the proposal is confirmed")

type WriteExecutor struct {
	pool    ExecDB
	modelID string
	revID   string
	userID  string
	// propCounterparts remembers, for this executor's lifetime (one
	// confirmed proposal), which working-revision property a property id
	// from another revision was matched to. Matching is by name, so once a
	// step renames that property, a later step passing the same id must
	// keep reaching it rather than whatever now carries the old name.
	propCounterparts map[string]string
	hooks            Hooks
}

// Hooks are gateway operations the executor calls rather than copies, so
// the assistant is held to exactly what the developer endpoints do. Each is
// optional: an executor without one (unit tests, the draft-creating
// executor) skips the plan check or refuses the tool that needs it.
type Hooks struct {
	// CheckMetrics and CheckMembers refuse a creation the tenant's plan does
	// not allow — the checks POST /api/developer/metrics and the member
	// endpoints run. Without them the assistant could build past a limit
	// the developer is held to.
	CheckMetrics func(ctx context.Context, modelID string, adding int) error
	CheckMembers func(ctx context.Context, dimensionID string, adding int) error
	// PostFormIntegration posts every eligible record of a form integration
	// into its metric: the developer's backfill, and what the developer's
	// integration update runs after saving.
	PostFormIntegration func(ctx context.Context, integrationID string) (int, error)
	// ImportFile imports a spreadsheet attached to the session through the
	// Import Wizard's pipeline (parse, column map, name resolution, write
	// guard, plan limits, recalculation, audit) and returns a summary.
	ImportFile func(ctx context.Context, req FileImportRequest) (string, error)
	// WriteValues writes a write_input_values step's values through the same
	// pipeline, as typed cells (write guard, plan limits, recalculation,
	// audit), and returns a summary.
	WriteValues func(ctx context.Context, req ValuesWriteRequest) (string, error)
	// RecallPreview returns the target, reshape and column map of this
	// session's latest preview_file_import of a file's sheet that reported no
	// errors. An import_file_data naming only the file and sheet repeats it:
	// the assistant found a clean preview live and then proposed the import
	// without the params it had just proven.
	RecallPreview func(ctx context.Context, file, sheet string) (FileImportRequest, bool)
	// SaveConnector creates or updates a REST API connector — an HTTPS API,
	// an SFTP file or a model link — through the developer's own save path
	// (validation, ownership, a model link's both-models rule, schedule,
	// audit), on the database this executor runs on. Returns its id.
	SaveConnector func(ctx context.Context, req ConnectorSave) (string, error)
	// SaveConnection creates or updates a sign-in connection. The secret
	// parts of its credential come from the confirmation card, which the
	// gateway reads; a proposal check saves it without them.
	SaveConnection func(ctx context.Context, req ConnectionSave) (string, error)
	// RunIntegration runs, tests (mode "test") or dry-runs an integration
	// through the developer's own Run now and Test routes, as the developer,
	// and waits for the result. A proposal check answers ErrNotDryRunnable.
	RunIntegration func(ctx context.Context, integrationID, mode string) (string, error)
	// TriggerRule fires an automation rule through its manual trigger route,
	// as the developer. A proposal check answers ErrNotDryRunnable.
	TriggerRule func(ctx context.Context, ruleID string, payload map[string]any) (string, error)
}

// WithHooks sets the executor's gateway hooks and returns it.
func (e *WriteExecutor) WithHooks(h Hooks) *WriteExecutor {
	e.hooks = h
	return e
}

func (e *WriteExecutor) checkMetrics(ctx context.Context, adding int) error {
	if e.hooks.CheckMetrics == nil {
		return nil
	}
	return e.hooks.CheckMetrics(ctx, e.modelID, adding)
}

func (e *WriteExecutor) checkMembers(ctx context.Context, dimensionID string, adding int) error {
	if e.hooks.CheckMembers == nil || adding <= 0 {
		return nil
	}
	return e.hooks.CheckMembers(ctx, dimensionID, adding)
}

func NewWriteExecutor(pool *pgxpool.Pool, modelID, revID string) *WriteExecutor {
	return &WriteExecutor{pool: pool, modelID: modelID, revID: revID}
}

// NewDryRunWriteExecutor runs steps on tx, which the caller rolls back:
// what confirming a proposal would do, without keeping any of it. The
// gateway hooks (file import, form postings) it is given must check on tx
// rather than write.
func NewDryRunWriteExecutor(tx pgx.Tx, modelID, revID, userID string) *WriteExecutor {
	return &WriteExecutor{pool: tx, modelID: modelID, revID: revID, userID: userID}
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
	"form": {
		lookup:      `SELECT model_id::text, COALESCE(revision_id::text,''), name FROM model.form_def WHERE id=$1::uuid`,
		counterpart: `SELECT id::text FROM model.form_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3`,
	},
	"integration": {
		lookup:      `SELECT model_id::text, COALESCE(revision_id::text,''), name FROM model.integration_def WHERE id=$1::uuid`,
		counterpart: `SELECT id::text FROM model.integration_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3`,
	},
	// Folder names are not unique, so a counterpart must be the only folder
	// of that name — otherwise the reference is refused as ambiguous.
	"dashboard_folder": {
		lookup: `SELECT model_id::text, COALESCE(revision_id::text,''), name FROM model.dashboard_folder WHERE id=$1::uuid`,
		counterpart: `SELECT min(id::text) FROM model.dashboard_folder
		              WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3 HAVING count(*) = 1`,
	},
	// A widget has no name. Its identity is its dashboard's name, its type
	// and its place on the canvas — what a revision copy preserves — and a
	// counterpart must be the only widget matching it.
	"dashboard_widget": {
		lookup: `SELECT d.model_id::text, COALESCE(d.revision_id::text,''),
		                d.name || E'\x1f' || w.widget_type || E'\x1f' || w.pos_x || E'\x1f' || w.pos_y || E'\x1f' || w.size_w || E'\x1f' || w.size_h
		         FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id = w.dashboard_id WHERE w.id=$1::uuid`,
		counterpart: `SELECT min(w.id::text) FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id = w.dashboard_id
		              WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid
		                AND d.name || E'\x1f' || w.widget_type || E'\x1f' || w.pos_x || E'\x1f' || w.pos_y || E'\x1f' || w.size_w || E'\x1f' || w.size_h = $3
		              HAVING count(*) = 1`,
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

// remapGridWidgetMetrics resolves a grid widget's metric_ids — the metrics
// it shows, in order — into the executor's revision, by id or by name. The
// list is also taken under "metrics", the name create_grid reads it by; left
// there it would have been dropped without a word.
func (e *WriteExecutor) remapGridWidgetMetrics(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var props map[string]any
	if err := json.Unmarshal(raw, &props); err != nil {
		return nil, fmt.Errorf("widget_props is not a JSON object: %w", err)
	}
	if v, ok := props["metrics"]; ok {
		if _, both := props["metric_ids"]; !both {
			props["metric_ids"] = v
		}
		delete(props, "metrics")
	}
	v, ok := props["metric_ids"]
	if !ok || v == nil {
		return raw, nil
	}
	ids, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("metric_ids is the list of metrics the grid widget shows, in order (ids or names)")
	}
	for i, item := range ids {
		id, _ := item.(string)
		if id == "" {
			return nil, fmt.Errorf("metric_ids[%d] is not a metric id or name", i)
		}
		mapped, err := e.requireInModel(ctx, "metric", id)
		if err != nil {
			return nil, fmt.Errorf("grid widget metric_ids[%d]: %w", i, err)
		}
		ids[i] = mapped
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
	if !uuidShaped(id) && kind != "dimension_member" && kind != "dashboard_widget" && e.revID != "" {
		var mapped string
		if err := e.pool.QueryRow(ctx, q.counterpart, e.modelID, e.revID, id).Scan(&mapped); err == nil {
			return mapped, nil
		}
		return "", fmt.Errorf("%s %q not found in the working revision — pass its exact name or id%s", kind, id, e.availableNames(ctx, kind))
	}
	var owner, revision, identity string
	// Only a UUID-shaped id reaches the ::uuid lookup: a malformed one fails
	// the cast, which inside the plan check's transaction aborts it, so the
	// near-match below could not run (live: a widget id with one character
	// too many).
	lookupErr := fmt.Errorf("not a uuid")
	if uuidShaped(id) {
		lookupErr = e.pool.QueryRow(ctx, q.lookup, id).Scan(&owner, &revision, &identity)
	}
	if err := lookupErr; err != nil {
		// A UUID one or two characters off the id of exactly one of the
		// model's grids (metrics, …) is that one: the model copies ids from
		// list output and slips a character — live, "4d0b" for "4d0d", again
		// after the error printed the right one. It is looked for across the
		// model's revisions, because the plan was checked on the active
		// revision and runs in the draft copied from it; the counterpart
		// remap below then finds the draft's own.
		near := e.nearID(ctx, kind, id)
		if near == "" {
			return "", fmt.Errorf("%s %s not found in this model%s", kind, id, e.availableNames(ctx, kind))
		}
		id = near
		if err := e.pool.QueryRow(ctx, q.lookup, id).Scan(&owner, &revision, &identity); err != nil {
			return "", fmt.Errorf("%s %s not found in this model%s", kind, id, e.availableNames(ctx, kind))
		}
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
	case "update_dimension":
		return e.updateDimension(ctx, params)
	case "add_dimension_member":
		return e.addDimensionMember(ctx, params)
	case "update_dimension_member":
		return e.updateDimensionMember(ctx, params)
	case "add_dimension_property":
		return e.addDimensionProperty(ctx, params)
	case "update_dimension_property":
		return e.updateDimensionProperty(ctx, params)
	case "delete_dimension_property":
		return e.deleteDimensionProperty(ctx, params)
	case "create_grid":
		return e.createGrid(ctx, params)
	case "add_grid_metric":
		return e.addGridMetric(ctx, params)
	case "add_grid_dimension":
		return e.addGridDimension(ctx, params)
	case "create_dashboard":
		return e.createDashboard(ctx, params)
	case "set_tags":
		return e.setTags(ctx, params)
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
	case "set_user_access_rules":
		return e.setUserAccessRules(ctx, params)
	case "create_file_integration":
		return e.createFileIntegration(ctx, params)
	case "write_input_values":
		return e.writeInputValues(ctx, params)
	case "import_file_data":
		return e.importFileData(ctx, params)
	case "create_export_integration":
		return e.createExportIntegration(ctx, params)
	case "update_integration":
		return e.updateIntegration(ctx, params)
	case "delete_integration":
		return e.deleteIntegration(ctx, params)
	case "create_api_integration":
		return e.createAPIIntegration(ctx, params)
	case "update_api_integration":
		return e.updateAPIIntegration(ctx, params)
	case "create_connection":
		return e.createConnection(ctx, params)
	case "update_connection":
		return e.updateConnection(ctx, params)
	case "run_integration":
		return e.runIntegration(ctx, params)
	case "trigger_automation_rule":
		return e.triggerAutomationRule(ctx, params)
	}
	if fn, ok := e.editTools()[tool]; ok {
		return fn(ctx, params)
	}
	return "", "", fmt.Errorf("unknown write tool: %s", tool)
}

// ── create_metric ─────────────────────────────────────────────────────────────

type createMetricParams struct {
	Name    string `json:"name"`
	Label   string `json:"label"` // display label; empty derives it from the name
	Formula string `json:"formula"`
	IsInput bool   `json:"is_input"`
	Format  string `json:"format"`
	// FormatDecimals: the decimal places shown. Left out, two for a number,
	// currency or percentage — a default of none showed a 0.3 increment as
	// 0 and a 5.6% rate as 6% in every model the assistant built.
	FormatDecimals *int   `json:"format_decimals"`
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
	TimeSummary string   `json:"time_summary"`
	Tags        []string `json:"tags"`
	// PicklistDimension (format "picklist"): the dimension, by name or id,
	// whose members the metric's cells hold.
	PicklistDimension string `json:"picklist_dimension"`
	// PicklistAllowParents: its cells may hold a member with members under
	// it; off, a cell holds a leaf.
	PicklistAllowParents bool `json:"picklist_allow_parents"`
	// HighlightRules tint its cells (metricformula.HighlightRule).
	HighlightRules json.RawMessage `json:"highlight_rules"`
}

func (e *WriteExecutor) createMetric(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p createMetricParams
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	if err := metricformula.ValidMetricName(p.Name); err != nil {
		return "", "", err
	}
	if err := metricformula.ValidMetricFormat(p.Format); err != nil {
		return "", "", err
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
	decimals := defaultDecimals(p.Format)
	if p.FormatDecimals != nil {
		decimals = *p.FormatDecimals
	}
	revID := e.effectiveRevision(p.RevisionID)
	if err := metricformula.CheckNameFreeOfOtherKind(ctx, e.pool, e.modelID, revID, p.Name, "metric"); err != nil {
		return "", "", err
	}
	picklist, err := metricformula.ResolvePicklist(ctx, e.pool, e.modelID, revID, p.Format, p.PicklistDimension, p.AggRule, p.TimeSummary, p.IsInput)
	if err != nil {
		return "", "", err
	}
	p.AggRule, p.TimeSummary = picklist.AggRule, picklist.TimeSummary
	highlights, err := metricformula.CheckHighlightRules(ctx, e.pool, e.modelID, revID, p.Name, p.HighlightRules)
	if err != nil {
		return "", "", err
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
			Text: p.Format == metricformula.FormatText,
		})
		if vErr != nil {
			return "", "", vErr
		}
		formulaEdges = res.Edges
	}
	if err := e.checkMetrics(ctx, 1); err != nil {
		return "", "", err
	}

	var newID string
	if revID != "" {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.metric_def (model_id, name, formula, is_input, revision_id, format, format_decimals, format_currency, agg_rule,
			                              agg_numerator_metric_id, agg_denominator_metric_id, time_summary, tags)
			VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6, $7, $8, $9, NULLIF($10,'')::uuid, NULLIF($11,'')::uuid, $12, $13)
			RETURNING id::text
		`, e.modelID, p.Name, formulaPtr, p.IsInput, revID, p.Format, decimals, p.FormatCurrency, p.AggRule,
			p.AggNumeratorMetricID, p.AggDenominatorMetricID, p.TimeSummary, tags.Clean(p.Tags)).Scan(&newID)
	} else {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.metric_def (model_id, name, formula, is_input, format, format_decimals, format_currency, agg_rule,
			                              agg_numerator_metric_id, agg_denominator_metric_id, time_summary, tags)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, NULLIF($9,'')::uuid, NULLIF($10,'')::uuid, $11, $12)
			RETURNING id::text
		`, e.modelID, p.Name, formulaPtr, p.IsInput, p.Format, decimals, p.FormatCurrency, p.AggRule,
			p.AggNumeratorMetricID, p.AggDenominatorMetricID, p.TimeSummary, tags.Clean(p.Tags)).Scan(&newID)
	}
	if err != nil {
		if metricformula.IsUniqueViolation(err) {
			return "", "", metricformula.MetricNameTaken(err, p.Name)
		}
		return "", "", fmt.Errorf("insert metric: %w", err)
	}
	if l := strings.TrimSpace(p.Label); l != "" {
		if _, err := e.pool.Exec(ctx, `UPDATE model.metric_def SET label=$2 WHERE id=$1::uuid`, newID, l); err != nil {
			return "", "", fmt.Errorf("set label: %w", err)
		}
	}
	if picklist.DimensionID != "" {
		if _, err := e.pool.Exec(ctx, `UPDATE model.metric_def SET picklist_dimension_id=$2::uuid, picklist_allow_parents=$3 WHERE id=$1::uuid`, newID, picklist.DimensionID, p.PicklistAllowParents); err != nil {
			return "", "", fmt.Errorf("set pick-list dimension: %w", err)
		}
	}
	if string(highlights) != "[]" {
		if _, err := e.pool.Exec(ctx, `UPDATE model.metric_def SET highlight_rules=$2::jsonb WHERE id=$1::uuid`, newID, string(highlights)); err != nil {
			return "", "", fmt.Errorf("set highlight rules: %w", err)
		}
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

// defaultDecimals is create_metric's decimal places when the step leaves
// them out: two for a number, currency or percentage, none for the other
// formats (a date, text, pick-list or boolean shows no decimals).
func defaultDecimals(format string) int {
	switch format {
	case "number", "currency", "percentage":
		return 2
	}
	return 0
}

// ── update_metric ─────────────────────────────────────────────────────────────

type updateMetricParams struct {
	MetricID string `json:"metric_id"`
	Name     string `json:"name"`
	// Label: left out keeps it; "" clears it back to the derived label.
	Label          *string `json:"label"`
	Formula        string  `json:"formula"`
	AggRule        string  `json:"agg_rule"`
	Format         string  `json:"format"`
	FormatDecimals int     `json:"format_decimals"`
	FormatCurrency string  `json:"format_currency"`

	AggNumeratorMetricID   string `json:"agg_numerator_metric_id"`
	AggDenominatorMetricID string `json:"agg_denominator_metric_id"`
	TimeSummary            string `json:"time_summary"`
	// Left out keeps the metric's tags; a list (even empty) replaces them.
	Tags *[]string `json:"tags"`
	// PicklistDimension: the dimension (name or id) a pick-list's cells
	// hold members of; left out keeps it while the format stays "picklist".
	PicklistDimension string `json:"picklist_dimension"`
	// PicklistAllowParents: left out keeps it.
	PicklistAllowParents *bool `json:"picklist_allow_parents"`
	// IsInput switches the metric between input and calculated in place
	// (modeledit.SwitchMetricKind); left out keeps it. Becoming calculated
	// takes a formula, and drop_values for an input holding values.
	IsInput    *bool `json:"is_input"`
	DropValues bool  `json:"drop_values"`
	// HighlightRules: left out keeps them; [] removes them.
	HighlightRules json.RawMessage `json:"highlight_rules"`
}

func (e *WriteExecutor) updateMetric(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p updateMetricParams
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.MetricID == "" {
		return "", "", fmt.Errorf("metric_id is required")
	}
	mappedMetricID, err := e.requireInModel(ctx, "metric", p.MetricID)
	if err != nil {
		// Live, the assistant "updated" metrics it had never created.
		return "", "", fmt.Errorf("%w — update_metric changes an existing metric; to make a new one, propose create_metric", err)
	}
	p.MetricID = mappedMetricID
	// A partial update, as the developer's PATCH: a key left out keeps the
	// stored value (left out, the name was once written as "" and every
	// other setting reset). A key sent empty takes its default.
	var sent map[string]json.RawMessage
	_ = json.Unmarshal(raw, &sent)
	has := func(k string) bool { _, ok := sent[k]; return ok }

	// The metric's own revision is needed before validating, not after: refs
	// resolve within it, and passing MetricID is what lets the validator
	// reject a formula that references the metric being edited, directly or
	// through a cycle. is_input is the stored one unless the step switches
	// it (is_input): ValidateAggRule needs it to reject "formula" on an
	// input metric.
	var metricRev, storedName string
	var metricIsInput bool
	var st updateMetricParams
	if err := e.pool.QueryRow(ctx, `
		SELECT COALESCE(revision_id::text,''), is_input, name, COALESCE(formula,''), COALESCE(agg_rule,''),
		       COALESCE(format,''), COALESCE(format_decimals,0), COALESCE(format_currency,''),
		       COALESCE(agg_numerator_metric_id::text,''), COALESCE(agg_denominator_metric_id::text,''), COALESCE(time_summary,''),
		       COALESCE(picklist_dimension_id::text,'')
		FROM model.metric_def WHERE id=$1::uuid`, p.MetricID).Scan(&metricRev, &metricIsInput, &storedName,
		&st.Formula, &st.AggRule, &st.Format, &st.FormatDecimals, &st.FormatCurrency,
		&st.AggNumeratorMetricID, &st.AggDenominatorMetricID, &st.TimeSummary, &st.PicklistDimension); err != nil {
		return "", "", fmt.Errorf("load metric: %w", err)
	}
	if err := metricformula.CheckRenameFreeOfOtherKind(ctx, e.pool, p.MetricID, p.Name, "metric"); err != nil {
		return "", "", err
	}
	if p.Name == "" {
		p.Name = storedName
	} else if p.Name != storedName {
		if err := metricformula.ValidMetricName(p.Name); err != nil {
			return "", "", err
		}
	}
	if has("format") {
		if err := metricformula.ValidMetricFormat(p.Format); err != nil {
			return "", "", err
		}
	}
	formulaSent := has("formula") && p.Formula != ""
	switchKind := p.IsInput != nil && *p.IsInput != metricIsInput
	if switchKind {
		if *p.IsInput {
			if formulaSent {
				return "", "", fmt.Errorf("an input metric has no formula — send is_input true without one")
			}
			if !has("agg_rule") && (st.AggRule == string(rollup.AggFormula) || st.AggRule == string(rollup.AggRate)) {
				st.AggRule = "sum"
			}
			st.Formula = ""
		} else if !formulaSent {
			return "", "", fmt.Errorf("a calculated metric needs a formula — send it with is_input false")
		}
		metricIsInput = *p.IsInput
	}
	for _, k := range []struct {
		key      string
		dst, src *string
	}{
		{"formula", &p.Formula, &st.Formula}, {"agg_rule", &p.AggRule, &st.AggRule},
		{"format", &p.Format, &st.Format}, {"format_currency", &p.FormatCurrency, &st.FormatCurrency},
		{"agg_numerator_metric_id", &p.AggNumeratorMetricID, &st.AggNumeratorMetricID},
		{"agg_denominator_metric_id", &p.AggDenominatorMetricID, &st.AggDenominatorMetricID},
		{"time_summary", &p.TimeSummary, &st.TimeSummary},
	} {
		if !has(k.key) {
			*k.dst = *k.src
		}
	}
	if !has("format_decimals") {
		p.FormatDecimals = st.FormatDecimals
	}
	{
		// A pick-list keeps its dimension unless the step changes it;
		// leaving the format drops it. Becoming one takes the pick-list's
		// own aggregation and time summary unless the step sets them.
		format := p.Format
		if format == "" {
			format = "number"
		}
		if !has("picklist_dimension") {
			p.PicklistDimension = st.PicklistDimension
			if format != metricformula.FormatPicklist {
				p.PicklistDimension = ""
			}
		}
		if format == metricformula.FormatText && metricIsInput {
			// A text input's notes have no total.
			if !has("agg_rule") {
				p.AggRule = ""
			}
			if !has("time_summary") {
				p.TimeSummary = ""
			}
		}
		if format == metricformula.FormatPicklist || (format == metricformula.FormatText && !metricIsInput) {
			// A pick-list's and a calculated text's totals are none or formula.
			if !has("agg_rule") && p.AggRule != string(rollup.AggNone) && p.AggRule != string(rollup.AggFormula) {
				p.AggRule = ""
			}
			if !has("time_summary") {
				p.TimeSummary = ""
			}
		}
		pl, err := metricformula.ResolvePicklist(ctx, e.pool, e.modelID, metricRev, format, p.PicklistDimension, p.AggRule, p.TimeSummary, metricIsInput)
		if err != nil {
			return "", "", err
		}
		p.PicklistDimension, p.AggRule, p.TimeSummary = pl.DimensionID, pl.AggRule, pl.TimeSummary
	}
	if p.AggRule == "" {
		p.AggRule = "sum"
	}
	if p.AggRule != string(rollup.AggRate) {
		// Operands belong to a ratio only: moving off it drops them unless
		// the step sets them.
		if !has("agg_numerator_metric_id") {
			p.AggNumeratorMetricID = ""
		}
		if !has("agg_denominator_metric_id") {
			p.AggDenominatorMetricID = ""
		}
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
	if err := metricformula.ValidateAggRule(p.AggRule, metricIsInput,
		p.AggNumeratorMetricID, p.AggDenominatorMetricID, p.MetricID); err != nil {
		return "", "", err
	}
	if err := metricformula.ValidateAggOperands(ctx, e.pool, p.AggRule, e.modelID, metricRev,
		p.AggNumeratorMetricID, p.AggDenominatorMetricID); err != nil {
		return "", "", err
	}

	var formulaPtr *string
	if p.Formula != "" {
		formulaPtr = &p.Formula
	}
	var formulaEdges []metricformula.Edge
	if formulaSent {
		res, vErr := metricformula.Validate(ctx, e.pool, metricformula.Request{
			ModelID: e.modelID, RevisionID: metricRev, MetricID: p.MetricID,
			Name: validationName(p.Name, storedName), Formula: p.Formula,
			Text: p.Format == metricformula.FormatText && !metricIsInput,
		})
		if vErr != nil {
			return "", "", vErr
		}
		formulaEdges = res.Edges
	}
	if switchKind {
		if err := modeledit.SwitchMetricKind(ctx, e.pool, p.MetricID, metricIsInput, p.DropValues); err != nil {
			return "", "", err
		}
	}
	if _, err := e.pool.Exec(ctx, `
		UPDATE model.metric_def
		SET name=$2, formula=$3, agg_rule=$4, format=$5, format_decimals=$6, format_currency=$7,
		    agg_numerator_metric_id=NULLIF($8,'')::uuid, agg_denominator_metric_id=NULLIF($9,'')::uuid,
		    time_summary=$10, tags=COALESCE($11, tags), picklist_dimension_id=NULLIF($12,'')::uuid,
		    picklist_allow_parents = CASE WHEN NULLIF($12,'') IS NULL THEN false ELSE COALESCE($13, picklist_allow_parents) END,
		    is_input=$14
		WHERE id=$1::uuid
	`, p.MetricID, p.Name, formulaPtr, p.AggRule, p.Format, p.FormatDecimals, p.FormatCurrency,
		p.AggNumeratorMetricID, p.AggDenominatorMetricID, p.TimeSummary, optionalTags(p.Tags), p.PicklistDimension, p.PicklistAllowParents,
		metricIsInput); err != nil {
		if metricformula.IsUniqueViolation(err) {
			return "", "", metricformula.MetricNameTaken(err, p.Name)
		}
		return "", "", fmt.Errorf("update metric: %w", err)
	}
	if p.Label != nil {
		if _, err := e.pool.Exec(ctx, `UPDATE model.metric_def SET label=NULLIF(btrim($2),'') WHERE id=$1::uuid`, p.MetricID, *p.Label); err != nil {
			return "", "", fmt.Errorf("set label: %w", err)
		}
	}
	if has("highlight_rules") {
		highlights, err := metricformula.CheckHighlightRules(ctx, e.pool, e.modelID, metricRev, p.Name, p.HighlightRules)
		if err != nil {
			return "", "", err
		}
		if _, err := e.pool.Exec(ctx, `UPDATE model.metric_def SET highlight_rules=$2::jsonb WHERE id=$1::uuid`, p.MetricID, string(highlights)); err != nil {
			return "", "", fmt.Errorf("set highlight rules: %w", err)
		}
	}
	// Re-wire dependencies when the formula changed.
	if formulaSent {
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
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.MetricID == "" {
		return "", "", fmt.Errorf("metric_id is required")
	}
	mappedMetricID, err := e.requireInModel(ctx, "metric", p.MetricID)
	if err != nil {
		return "", "", err
	}
	p.MetricID = mappedMetricID
	var name string
	_ = e.pool.QueryRow(ctx, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, p.MetricID).Scan(&name)
	// Refused while another metric reads it (METRIC_IN_USE names them), as
	// the developer's delete is.
	if err := metricformula.CheckMetricNotInUse(ctx, e.pool, p.MetricID); err != nil {
		return "", "", err
	}
	// The same widget cleanup as the developer's delete: KPI tiles over the
	// metric go, and the metric leaves every chart's series (a chart left
	// plotting nothing goes too).
	if err := modeledit.DropWidgetsReferencing(ctx, e.pool, p.MetricID); err != nil {
		return "", "", err
	}
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
	ParentDimensionID   string `json:"parent_dimension_id"`   // the same, by id (either field)
	// Time dimension marker (spec §4.1): "standard" (default) or "time".
	// A time dimension needs time_granularity and fiscal_year_start_month,
	// and its members carry period_start/period_end instead of parents.
	DimensionType   string   `json:"dimension_type"`
	TimeGranularity string   `json:"time_granularity"`
	FiscalYearStart int      `json:"fiscal_year_start_month"`
	Tags            []string `json:"tags"`
	// BusinessMaintained: business users add, rename and remove its members.
	BusinessMaintained bool `json:"business_maintained"`
	// A property grouping (metricformula.ValidateGrouping, the developer
	// console's rules): this dimension's members group the source
	// dimension's members by their value of the declared property
	// source_property. The source is named by id or name (either field);
	// derive_members adds one member per distinct value.
	SourceDimensionID   string `json:"source_dimension_id"`
	SourceDimensionName string `json:"source_dimension_name"`
	SourceProperty      string `json:"source_property"`
	DeriveMembers       bool   `json:"derive_members"`
	Members             []struct {
		Code        string `json:"code"`
		Label       string `json:"label"`
		ParentCode  string `json:"parent_code"`
		PeriodStart string `json:"period_start"`
		PeriodEnd   string `json:"period_end"`
		// Properties are the member's property values, stored as the
		// developer console's member PATCH stores them.
		Properties map[string]string `json:"properties"`
		// Formula makes it a calculated member ({RF} - {LY}).
		Formula string `json:"formula"`
	} `json:"members"`
}

// memberPropertyKeys is every property name any listed member carries.
func (p *createDimensionParams) memberPropertyKeys() map[string]string {
	keys := map[string]string{}
	for _, m := range p.Members {
		for k := range m.Properties {
			keys[k] = ""
		}
	}
	return keys
}

func (e *WriteExecutor) createDimension(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p createDimensionParams
	if err := decodeParams(raw, &p); err != nil {
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
	if p.BusinessMaintained && (timeCfg.Type == timedim.TypeTime || p.SourceDimensionID != "" || p.SourceDimensionName != "") {
		return "", "", fmt.Errorf("a time dimension or a property grouping cannot be business-maintained: its members are not typed")
	}
	revID := e.effectiveRevision(p.RevisionID)

	// The parent resolves within the working revision (by name or id) and
	// passes the developer console's rules (metricformula.
	// ValidateParentDimension): same model and revision, not a time
	// dimension.
	var parentDimID *string
	if ref := firstNonEmpty(p.ParentDimensionID, p.ParentDimensionName); ref != "" {
		pdID, err := e.resolveDimensionRef(ctx, ref, revID)
		if err != nil {
			return "", "", fmt.Errorf("%s: parent dimension: %w", metricformula.CodeInvalidParentDimension, err)
		}
		if err := metricformula.ValidateParentDimension(ctx, e.pool, metricformula.ParentDimension{
			ModelID: e.modelID, RevisionID: revID, DimensionType: timeCfg.Type, ParentDimensionID: pdID,
		}); err != nil {
			return "", "", err
		}
		parentDimID = &pdID
	}

	sourceDimID, sourceProp, err := e.resolveGrouping(ctx, &p, revID, timeCfg.Type, parentDimID != nil)
	if err != nil {
		return "", "", err
	}
	if err := e.checkMembers(ctx, "", len(p.Members)); err != nil {
		return "", "", err
	}

	// A member property key no declaration could ever name is refused
	// before anything is written: its values would be unreadable.
	for _, m := range p.Members {
		for k := range m.Properties {
			if err := metricformula.CheckPropertyKey(strings.TrimSpace(k)); err != nil {
				return "", "", fmt.Errorf("member %q: %w", m.Code, err)
			}
		}
	}
	if err := metricformula.CheckNameFreeOfOtherKind(ctx, e.pool, e.modelID, revID, p.Name, "dimension"); err != nil {
		return "", "", err
	}
	var newID string
	var granularity *string
	var fiscalStart *int
	if timeCfg.Type == timedim.TypeTime {
		granularity, fiscalStart = &timeCfg.Granularity, &timeCfg.FiscalYearStartMonth
	}
	if revID != "" {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.dimension_def (model_id, name, agg_rule, revision_id, parent_dimension_id, dimension_type, time_granularity, fiscal_year_start_month, tags,
			                                 source_dimension_id, source_property)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5::uuid, $6, $7, $8, $9, $10::uuid, $11) RETURNING id::text
		`, e.modelID, p.Name, p.AggRule, revID, parentDimID, timeCfg.Type, granularity, fiscalStart, tags.Clean(p.Tags),
			sourceDimID, sourceProp).Scan(&newID)
	} else {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.dimension_def (model_id, name, agg_rule, parent_dimension_id, dimension_type, time_granularity, fiscal_year_start_month, tags,
			                                 source_dimension_id, source_property)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6, $7, $8, $9::uuid, $10) RETURNING id::text
		`, e.modelID, p.Name, p.AggRule, parentDimID, timeCfg.Type, granularity, fiscalStart, tags.Clean(p.Tags),
			sourceDimID, sourceProp).Scan(&newID)
	}
	if err != nil {
		if metricformula.IsUniqueViolation(err) {
			return "", "", metricformula.DimensionNameTaken(err, p.Name)
		}
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
				INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, parent_member_id, properties)
				VALUES ($1::uuid, $2, $3, $4::date, $5::date, $6, $7::uuid, $8::jsonb) RETURNING id::text
			`, newID, m.Code, m.Label, start, end, idx, parentID, memberPropertiesJSON(m.Properties)).Scan(&memID); err != nil {
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
		return msg + e.undeclaredPropertyNote(ctx, newID, p.memberPropertyKeys()), newID, nil
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
			INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, sort_order, properties)
			VALUES ($1::uuid, $2, $3, $4::uuid, (
				SELECT COALESCE(MAX(sort_order),0)+1 FROM model.dimension_member WHERE dimension_id=$1::uuid
			), $5::jsonb) RETURNING id::text
		`, newID, m.Code, m.Label, parentID, memberPropertiesJSON(m.Properties)).Scan(&memID)
		if er == nil && parentDimID == nil {
			// Same-dimension hierarchy: later members in this same call may reference
			// an earlier one as their parent.
			codeToID[m.Code] = memID
		}
	}

	if p.BusinessMaintained {
		if err := metricformula.CheckBusinessMaintainable(ctx, e.pool, newID); err != nil {
			return "", "", err
		}
		if _, err := e.pool.Exec(ctx, `UPDATE model.dimension_def SET business_maintained = true WHERE id=$1::uuid`, newID); err != nil {
			return "", "", fmt.Errorf("set business_maintained: %w", err)
		}
	}
	msg := fmt.Sprintf("Dimension '%s' created (id: %s)", p.Name, newID)
	if len(p.Members) > 0 {
		msg += fmt.Sprintf(" with %d member(s)", len(p.Members))
	}
	if sourceDimID != nil {
		msg += fmt.Sprintf(", grouping the source dimension's members by %s", *sourceProp)
		if p.DeriveMembers {
			missing, err := metricformula.MissingGroupingMembers(ctx, e.pool, newID, *sourceDimID, *sourceProp)
			if err == nil {
				if err = e.checkMembers(ctx, newID, len(missing)); err != nil {
					// The developer's create checks before the dimension
					// exists; this path only knows the count afterwards.
					_, _ = e.pool.Exec(ctx, `DELETE FROM model.dimension_def WHERE id=$1::uuid`, newID)
					return "", "", err
				}
				var added []string
				if added, err = metricformula.DeriveGroupingMembers(ctx, e.pool, newID, missing); err == nil {
					msg += fmt.Sprintf("; derived %d member(s) from its values: %s", len(added), strings.Join(added, ", "))
				}
			}
			if err != nil {
				return "", "", fmt.Errorf("derive grouping members: %w", err)
			}
		}
	}
	for _, m := range p.Members {
		if strings.TrimSpace(m.Formula) != "" {
			if err := modeledit.SetMemberFormula(ctx, e.pool, newID, m.Code, m.Formula); err != nil {
				return "", "", fmt.Errorf("member %s: %w", m.Code, err)
			}
		}
	}
	return msg + e.undeclaredPropertyNote(ctx, newID, p.memberPropertyKeys()), newID, nil
}

// resolveGrouping validates create_dimension's property grouping with the
// developer console's validator (metricformula.ValidateGrouping), the
// source resolved into the working revision by id or name. It returns nil
// pointers when no grouping was asked for.
func (e *WriteExecutor) resolveGrouping(ctx context.Context, p *createDimensionParams, revID, dimType string, hasParent bool) (*string, *string, error) {
	ref := p.SourceDimensionID
	if ref == "" {
		ref = p.SourceDimensionName
	}
	if ref == "" && strings.TrimSpace(p.SourceProperty) == "" {
		if p.DeriveMembers {
			return nil, nil, fmt.Errorf("%s: derive_members needs source_dimension_id (or source_dimension_name) and source_property",
				metricformula.CodeInvalidGrouping)
		}
		return nil, nil, nil
	}
	var sourceID string
	if ref != "" {
		mapped, err := e.resolveDimensionRef(ctx, ref, revID)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: source dimension: %w", metricformula.CodeInvalidGrouping, err)
		}
		sourceID = mapped
	}
	declared, err := metricformula.ValidateGrouping(ctx, e.pool, metricformula.Grouping{
		ModelID: e.modelID, RevisionID: revID, DimensionType: dimType, HasParentDimension: hasParent,
		SourceDimensionID: sourceID, SourceProperty: p.SourceProperty,
	})
	if err != nil {
		return nil, nil, err
	}
	return &sourceID, &declared, nil
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
		// Properties are the member's property values, stored as the
		// developer console's member PATCH stores them.
		Properties map[string]string `json:"properties"`
		// Formula makes it a calculated member ({RF} - {LY}).
		Formula string `json:"formula"`
	}
	if err := decodeParams(raw, &p); err != nil {
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
	props, err := metricformula.CheckMemberProperties(ctx, e.pool, p.DimensionID, p.Properties, nil)
	if err != nil {
		return "", "", err
	}
	propsJSON := memberPropertiesJSON(props)
	propNote := e.undeclaredPropertyNote(ctx, p.DimensionID, props)
	if err := e.checkMembers(ctx, p.DimensionID, 1); err != nil {
		return "", "", err
	}

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
			INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, parent_member_id, properties)
			VALUES ($1::uuid, $2, $3, $4::date, $5::date, $6, $7::uuid, $8::jsonb) RETURNING id::text
		`, p.DimensionID, p.Code, p.Label, start, end, idx, parentID, propsJSON).Scan(&newID); err != nil {
			if metricformula.IsMemberCodeTaken(err) {
				return "", "", metricformula.MemberCodeTaken(err, p.Code)
			}
			return "", "", fmt.Errorf("insert time member: %w", err)
		}
		if err := timedim.ValidateAndReindex(ctx, tx, p.DimensionID); err != nil {
			return "", "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", "", err
		}
		if parentID != nil {
			if _, _, _, err := modeledit.SplitIfFirstChild(ctx, e.pool, p.DimensionID, *parentID, p.Code); err != nil {
				return "", "", fmt.Errorf("move the parent's values to its first child: %w", err)
			}
		}
		if start == nil {
			return fmt.Sprintf("Aggregate period '%s' (%s) added (id: %s)%s", p.Label, p.Code, newID, propNote), newID, nil
		}
		return fmt.Sprintf("Period '%s' (%s, %s..%s) added (id: %s)%s", p.Label, p.Code, p.PeriodStart, p.PeriodEnd, newID, propNote), newID, nil
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
			// Two new rows, the parent and the member: the plan check above
			// asked for one, and a dimension one member below its limit
			// ended one above it.
			if err := e.checkMembers(ctx, lookupDim, 2); err != nil {
				return "", "", err
			}
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
		INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, sort_order, properties)
		VALUES ($1::uuid, $2, $3, $4::uuid, (
			SELECT COALESCE(MAX(sort_order),0)+1 FROM model.dimension_member WHERE dimension_id=$1::uuid
		), $5::jsonb) RETURNING id::text
	`, p.DimensionID, p.Code, p.Label, parentID, propsJSON).Scan(&newID)
	if err != nil {
		if metricformula.IsMemberCodeTaken(err) {
			return "", "", metricformula.MemberCodeTaken(err, p.Code)
		}
		return "", "", fmt.Errorf("insert dimension member: %w", err)
	}
	// A leaf that gains its first child hands its values down to it, as the
	// developer's member create does.
	if parentID != nil {
		if _, _, _, err := modeledit.SplitIfFirstChild(ctx, e.pool, p.DimensionID, *parentID, p.Code); err != nil {
			return "", "", fmt.Errorf("move the parent's values to its first child: %w", err)
		}
	}
	result := fmt.Sprintf("Dimension member '%s' (%s) added (id: %s)", p.Label, p.Code, newID)
	if autoCreatedParent {
		result += fmt.Sprintf(" — parent '%s' didn't exist yet, created it as a top-level member", p.ParentCode)
	}
	if strings.TrimSpace(p.Formula) != "" {
		if err := modeledit.SetMemberFormula(ctx, e.pool, p.DimensionID, p.Code, p.Formula); err != nil {
			return "", "", err
		}
		result += " as a calculated member = " + strings.TrimSpace(p.Formula)
	}
	return result + propNote, newID, nil
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
		// Code identifies the member; NewCode renames it.
		Code    string `json:"code"`
		NewCode string `json:"new_code"`
		Label   string `json:"label"`
		// ParentCode moves the member under that parent; omitted/"" leaves
		// the parent unchanged. ClearParent=true makes it a top-level member.
		ParentCode  string `json:"parent_code"`
		ClearParent bool   `json:"clear_parent"`
		// A time member's dates; both left out keep the current ones.
		PeriodStart string            `json:"period_start"`
		PeriodEnd   string            `json:"period_end"`
		Properties  map[string]string `json:"properties"` // merged into existing
		// Formula: left out keeps it; "" makes an ordinary member again;
		// else a calculated member ({RF} - {LY}).
		Formula *string `json:"formula"`
	}
	if err := decodeParams(raw, &p); err != nil {
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

	var memberID, curLabel string
	var curParent *string
	var curStart, curEnd *time.Time
	if err := e.pool.QueryRow(ctx, `
		SELECT id::text, label, parent_member_id::text, period_start, period_end
		FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2
	`, p.DimensionID, p.Code).Scan(&memberID, &curLabel, &curParent, &curStart, &curEnd); err != nil {
		return "", "", fmt.Errorf("member %q not found in dimension (call list_dimensions to see codes)", p.Code)
	}
	if p.Label == "" && p.NewCode == "" && p.ParentCode == "" && !p.ClearParent &&
		p.PeriodStart == "" && p.PeriodEnd == "" && len(p.Properties) == 0 && p.Formula == nil {
		return "", "", fmt.Errorf("nothing to change: provide new_code, label, parent_code, clear_parent, period_start/period_end, properties or formula")
	}

	var changed []string
	code, label, parentID := p.Code, curLabel, curParent
	if p.NewCode != "" && p.NewCode != p.Code {
		code = p.NewCode
		changed = append(changed, fmt.Sprintf("code → %s", code))
	}
	if p.Label != "" {
		label = p.Label
		changed = append(changed, fmt.Sprintf("label → %q", p.Label))
	}

	switch {
	case p.ClearParent:
		parentID = nil
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
		var pid string
		if err := e.pool.QueryRow(ctx, `
			SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2
		`, lookupDim, p.ParentCode).Scan(&pid); err != nil {
			return "", "", fmt.Errorf("parent member %q not found (add it first with add_dimension_member)", p.ParentCode)
		}
		if pid == memberID {
			return "", "", fmt.Errorf("a member cannot be its own parent")
		}
		// Same-dimension re-parent: refuse a cycle (new parent being a
		// descendant of the member we're moving) before it corrupts every
		// rollup walk over this hierarchy.
		if lookupDim == p.DimensionID {
			cur := pid
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
		parentID = &pid
		changed = append(changed, fmt.Sprintf("parent → %s", p.ParentCode))
	}

	// A time member is written whole through the developer endpoint's path:
	// dates, parent and code validated with the rest of the dimension and
	// re-indexed in one transaction. Dates left out keep the member's own,
	// so a partial change does not turn a leaf period into an aggregate.
	start, end := p.PeriodStart, p.PeriodEnd
	if start == "" && end == "" && curStart != nil && curEnd != nil {
		start, end = curStart.Format("2006-01-02"), curEnd.Format("2006-01-02")
	}
	period, isTime, dated, err := modeledit.MemberPeriod(ctx, e.pool, p.DimensionID, start, end)
	if err != nil {
		return "", "", err
	}
	if p.PeriodStart != "" || p.PeriodEnd != "" {
		changed = append(changed, fmt.Sprintf("period %s..%s", p.PeriodStart, p.PeriodEnd))
	}
	if isTime {
		_, err = modeledit.WriteTimeMember(ctx, e.pool, p.DimensionID, memberID, code, label, period, dated, parentID)
	} else {
		_, err = e.pool.Exec(ctx, `UPDATE model.dimension_member SET code=$2, label=$3, parent_member_id=$4::uuid WHERE id=$1::uuid`,
			memberID, code, label, parentID)
	}
	if err != nil {
		if metricformula.IsMemberCodeTaken(err) {
			return "", "", metricformula.MemberCodeTaken(err, code)
		}
		return "", "", fmt.Errorf("update member: %w", err)
	}

	if len(p.Properties) > 0 {
		var stored map[string]string
		var storedJSON []byte
		if err := e.pool.QueryRow(ctx, `SELECT COALESCE(properties,'{}'::jsonb) FROM model.dimension_member WHERE id=$1::uuid`, memberID).Scan(&storedJSON); err == nil {
			_ = json.Unmarshal(storedJSON, &stored)
		}
		props, err := metricformula.CheckMemberProperties(ctx, e.pool, p.DimensionID, p.Properties, stored)
		if err != nil {
			return "", "", err
		}
		propJSON, _ := json.Marshal(props)
		if _, err := e.pool.Exec(ctx, `
			UPDATE model.dimension_member SET properties = COALESCE(properties,'{}'::jsonb) || $2::jsonb WHERE id=$1::uuid
		`, memberID, string(propJSON)); err != nil {
			return "", "", fmt.Errorf("merge properties: %w", err)
		}
		changed = append(changed, fmt.Sprintf("properties merged (%d)%s", len(props),
			e.undeclaredPropertyNote(ctx, p.DimensionID, props)))
	}
	// What the developer's member edit does to stored data: a new code
	// re-keys the facts, results and widget settings filed under the old
	// one, and a top-level member placed under a parent that had no
	// children takes over that parent's values.
	if err := modeledit.RekeyMemberCode(ctx, e.pool, p.DimensionID, p.Code, code); err != nil {
		return "", "", err
	}
	if parentID != nil && curParent == nil {
		if _, _, _, err := modeledit.SplitIfFirstChild(ctx, e.pool, p.DimensionID, *parentID, code); err != nil {
			return "", "", fmt.Errorf("move the parent's values to its first child: %w", err)
		}
	}
	if p.Formula != nil {
		code := p.Code
		if strings.TrimSpace(p.NewCode) != "" {
			code = strings.TrimSpace(p.NewCode)
		}
		if err := modeledit.SetMemberFormula(ctx, e.pool, p.DimensionID, code, *p.Formula); err != nil {
			return "", "", err
		}
		if strings.TrimSpace(*p.Formula) == "" {
			changed = append(changed, "no longer calculated")
		} else {
			changed = append(changed, "calculated = "+strings.TrimSpace(*p.Formula))
		}
	}
	return fmt.Sprintf("Dimension member '%s' updated: %s", p.Code, strings.Join(changed, "; ")), memberID, nil
}

// ── add_dimension_property ────────────────────────────────────────────────────

// addDimensionProperty declares a typed member property on a dimension — the
// AI Developer's twin of the developer console's POST
// /api/developer/dimensions/{id}/properties. Only a declared property can be
// read by a formula as dimension.property, typed by data_type, so without
// this the assistant could set member values it could never make usable.
// The declaration goes through the same validator as the developer
// endpoint, so the two paths cannot drift.
func (e *WriteExecutor) addDimensionProperty(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		DimensionID string `json:"dimension_id"`
		Name        string `json:"name"`
		DataType    string `json:"data_type"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.DimensionID == "" {
		return "", "", fmt.Errorf("dimension_id is required")
	}
	mappedDimID, err := e.requireInModel(ctx, "dimension", p.DimensionID)
	if err != nil {
		return "", "", err
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.DataType == "" {
		p.DataType = "text" // the developer endpoint's default
	}
	if err := metricformula.ValidatePropertyDeclaration(ctx, e.pool, mappedDimID, "", p.Name, p.DataType); err != nil {
		return "", "", err
	}
	var newID string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO model.dimension_property (dimension_id, name, data_type)
		VALUES ($1::uuid, $2, $3) RETURNING id::text
	`, mappedDimID, p.Name, p.DataType).Scan(&newID); err != nil {
		return "", "", fmt.Errorf("insert dimension property: %w", err)
	}
	withValue, undeclared, err := metricformula.AdoptPropertyValues(ctx, e.pool, mappedDimID, p.Name)
	if err != nil {
		return "", "", fmt.Errorf("adopt property values: %w", err)
	}
	msg := fmt.Sprintf("Property '%s' (%s) declared (id: %s) — formulas can now read it as <dimension>.%s; %d member(s) have a value",
		p.Name, p.DataType, newID, p.Name, withValue)
	if withValue == 0 && len(undeclared) > 0 {
		msg += fmt.Sprintf(". WARNING: the members' values are under %s, not '%s' — set them under '%s' with update_dimension_member, or declare the name they use, or every formula reading '%s' sees blanks",
			strings.Join(undeclared, ", "), p.Name, p.Name, p.Name)
	}
	return msg, newID, nil
}

// ── update_dimension_property / delete_dimension_property ────────────────────

// dimensionPropertyRef is a declared property resolved within the working
// revision: the dimension it is declared on and its current declaration.
type dimensionPropertyRef struct {
	dimID, id, name, dataType string
	// matchedByName: the caller passed a property id from another
	// revision, mapped to its working-revision counterpart.
	matchedByName bool
}

// resolveDimensionProperty finds the property the model named — by its
// current name (case-insensitively, as formulas read it) or by id — on the
// dimension it named, both scoped to the executor's model and revision.
// A property id of another dimension, or of another model's dimension, is
// refused; an id from another revision of the same dimension goes through
// crossRevisionProperty.
func (e *WriteExecutor) resolveDimensionProperty(ctx context.Context, dimensionRef, propertyRef string) (dimensionPropertyRef, error) {
	var r dimensionPropertyRef
	if dimensionRef == "" {
		return r, fmt.Errorf("dimension_id is required")
	}
	propertyRef = strings.TrimSpace(propertyRef)
	if propertyRef == "" {
		return r, fmt.Errorf("property is required: the property's current name or id")
	}
	dimID, err := e.requireInModel(ctx, "dimension", dimensionRef)
	if err != nil {
		return r, err
	}
	r.dimID = dimID
	if !uuidShaped(propertyRef) {
		if err := e.pool.QueryRow(ctx, `
			SELECT id::text, name, data_type FROM model.dimension_property
			WHERE dimension_id=$1::uuid AND lower(name)=lower($2)
		`, dimID, propertyRef).Scan(&r.id, &r.name, &r.dataType); err != nil {
			return r, fmt.Errorf("property %q is not declared on this dimension — list_dimensions shows its declared properties", propertyRef)
		}
		return r, nil
	}
	var ownerDim, name string
	if err := e.pool.QueryRow(ctx,
		`SELECT dimension_id::text, name FROM model.dimension_property WHERE id=$1::uuid`, propertyRef,
	).Scan(&ownerDim, &name); err != nil {
		return r, fmt.Errorf("property %s not found", propertyRef)
	}
	mappedOwner, err := e.requireInModel(ctx, "dimension", ownerDim)
	if err != nil {
		return r, fmt.Errorf("property %s: %w", propertyRef, err)
	}
	if mappedOwner != dimID {
		return r, fmt.Errorf("property %s is declared on a different dimension", propertyRef)
	}
	if ownerDim == dimID {
		r.id = propertyRef
		if err := e.pool.QueryRow(ctx,
			`SELECT name, data_type FROM model.dimension_property WHERE id=$1::uuid`, propertyRef,
		).Scan(&r.name, &r.dataType); err != nil {
			return r, fmt.Errorf("property %s not found", propertyRef)
		}
		return r, nil
	}
	return e.crossRevisionProperty(ctx, r, propertyRef, ownerDim, name)
}

// crossRevisionProperty maps a property id from another revision (typically
// the active one, whose ids the read tools list before a draft exists) to
// its counterpart in the working revision. There is no lineage column, so
// the counterpart is found by name, as requireInModel does for the
// dimension itself — but only while that name still identifies the same
// declaration:
//
//   - an id already matched by an earlier step of this proposal keeps its
//     match, so a rename by that earlier step does not re-point it;
//   - otherwise the working revision's same-named property must have been
//     copied with the revision (not declared since), and the dimension's
//     declared names must still be the ones of the id's revision (no
//     rename or delete since the copy). A mismatch is refused with a
//     request for the current name, rather than landing on a different
//     property — a developer PATCH/DELETE addresses the exact id and
//     cannot hit this either.
func (e *WriteExecutor) crossRevisionProperty(ctx context.Context, r dimensionPropertyRef, propertyRef, ownerDim, name string) (dimensionPropertyRef, error) {
	if prev, ok := e.propCounterparts[propertyRef]; ok {
		if err := e.pool.QueryRow(ctx, `
			SELECT name, data_type FROM model.dimension_property WHERE id=$1::uuid AND dimension_id=$2::uuid
		`, prev, r.dimID).Scan(&r.name, &r.dataType); err != nil {
			return r, fmt.Errorf("property %s (matched earlier in this proposal to the working revision's property %s) no longer exists — it was deleted", propertyRef, prev)
		}
		r.id = prev
		r.matchedByName = true
		return r, nil
	}
	refuse := fmt.Errorf("property %s belongs to another revision, and the working revision's declarations on this dimension have changed since it was copied, so the id cannot be matched safely — pass the property's current name (list_dimensions shows it)", propertyRef)
	var copied, sameNames bool
	err := e.pool.QueryRow(ctx, `
		SELECT p.id::text, p.name, p.data_type,
		       p.created_at <= rv.created_at,
		       (SELECT array_agg(lower(x.name) ORDER BY lower(x.name)) FROM model.dimension_property x WHERE x.dimension_id=$1::uuid)
		         IS NOT DISTINCT FROM
		       (SELECT array_agg(lower(y.name) ORDER BY lower(y.name)) FROM model.dimension_property y WHERE y.dimension_id=$3::uuid)
		FROM model.dimension_property p
		JOIN model.dimension_def d ON d.id = p.dimension_id
		JOIN model.revision rv ON rv.id = d.revision_id
		WHERE p.dimension_id=$1::uuid AND lower(p.name)=lower($2)
	`, r.dimID, name, ownerDim).Scan(&r.id, &r.name, &r.dataType, &copied, &sameNames)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, refuse
	}
	if err != nil {
		return r, fmt.Errorf("resolve property %s: %w", propertyRef, err)
	}
	if !copied || !sameNames {
		return r, refuse
	}
	if e.propCounterparts == nil {
		e.propCounterparts = map[string]string{}
	}
	e.propCounterparts[propertyRef] = r.id
	r.matchedByName = true
	return r, nil
}

// matchNote is appended to a step result when the property was given as an
// id from another revision, so the matched declaration is visible.
func (r dimensionPropertyRef) matchNote() string {
	if !r.matchedByName {
		return ""
	}
	return fmt.Sprintf(" (the id given is from another revision; matched to this revision's property %s)", r.id)
}

// updateDimensionProperty renames and/or retypes a declared property — the
// AI Developer's twin of the developer console's PATCH
// /api/developer/dimensions/{id}/properties/{propId}: a partial update (a
// field left out keeps its value), checked by the same validator, and a
// rename carried through every member's value and any dimension grouped by
// the property (metricformula.RenamePropertyValues) in one transaction.
// The developer path then recalculates the metrics reading the dimension;
// here the write lands in the AI's draft, and promoting the draft
// recomputes every calculated metric of the revision (activateRevision).
func (e *WriteExecutor) updateDimensionProperty(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		DimensionID string `json:"dimension_id"`
		Property    string `json:"property"`
		Name        string `json:"name"`
		DataType    string `json:"data_type"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	prop, err := e.resolveDimensionProperty(ctx, p.DimensionID, p.Property)
	if err != nil {
		return "", "", err
	}
	name, dataType := strings.TrimSpace(p.Name), strings.TrimSpace(p.DataType)
	if name == "" && dataType == "" {
		return "", "", fmt.Errorf("nothing to change: provide name, data_type, or both")
	}
	if name == "" {
		name = prop.name
	}
	if dataType == "" {
		dataType = prop.dataType
	}
	if err := metricformula.ValidatePropertyDeclaration(ctx, e.pool, prop.dimID, prop.id, name, dataType); err != nil {
		return "", "", err
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE model.dimension_property SET name=$2, data_type=$3 WHERE id=$1::uuid`,
		prop.id, name, dataType); err != nil {
		return "", "", fmt.Errorf("update dimension property: %w", err)
	}
	if err := metricformula.RenamePropertyValues(ctx, tx, prop.dimID, prop.name, name); err != nil {
		return "", "", fmt.Errorf("rename member values: %w", err)
	}
	// Formulas reading the old name are rewritten in the same transaction,
	// exactly as the developer rename does.
	rewritten, err := metricformula.RenamePropertyInFormulas(ctx, tx, prop.dimID, prop.name, name)
	if err != nil {
		return "", "", fmt.Errorf("rename property in formulas: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	var changed []string
	switch {
	case name == prop.name:
	case strings.EqualFold(name, prop.name):
		changed = append(changed, fmt.Sprintf("renamed to '%s' (case only — formulas read property names regardless of case)", name))
	default:
		changed = append(changed, fmt.Sprintf("renamed to '%s' (member values moved with it; %d formula(s) rewritten to the new name)", name, len(rewritten)))
	}
	if dataType != prop.dataType {
		changed = append(changed, fmt.Sprintf("type %s → %s", prop.dataType, dataType))
	}
	if len(changed) == 0 {
		changed = append(changed, "unchanged")
	}
	return fmt.Sprintf("Property '%s' updated: %s%s", prop.name, strings.Join(changed, "; "), prop.matchNote()), prop.id, nil
}

// deleteDimensionProperty removes a property declaration — the AI
// Developer's twin of the developer console's DELETE
// /api/developer/dimensions/{id}/properties/{propId}. As there, member
// values stored under the name stay (undeclared, unreadable by formulas),
// and the delete is refused with PROPERTY_IN_USE while a formula of the
// draft still reads the property.
func (e *WriteExecutor) deleteDimensionProperty(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		DimensionID string `json:"dimension_id"`
		Property    string `json:"property"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	prop, err := e.resolveDimensionProperty(ctx, p.DimensionID, p.Property)
	if err != nil {
		return "", "", err
	}
	if err := metricformula.CheckPropertyNotInUse(ctx, e.pool, prop.dimID, prop.name); err != nil {
		return "", "", err
	}
	tag, err := e.pool.Exec(ctx, `DELETE FROM model.dimension_property WHERE id=$1::uuid AND dimension_id=$2::uuid`, prop.id, prop.dimID)
	if err != nil {
		return "", "", fmt.Errorf("delete dimension property: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", "", fmt.Errorf("property %q not found", prop.name)
	}
	return fmt.Sprintf("Property '%s' deleted%s", prop.name, prop.matchNote()), "", nil
}

// memberPropertiesJSON is the JSONB a new member's "properties" are stored
// as, in the shape the developer console's member PATCH merges in: a flat
// {name: string value} object. Nil or empty gives the column default {}.
func memberPropertiesJSON(props map[string]string) string {
	if len(props) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(props)
	return string(b)
}

// undeclaredPropertyNote names the given property keys that the dimension
// has not declared. The value is stored either way, exactly as the
// developer console stores it, but a formula cannot read it as
// dimension.property until it is declared — the note tells the model so.
func (e *WriteExecutor) undeclaredPropertyNote(ctx context.Context, dimensionID string, props map[string]string) string {
	if len(props) == 0 {
		return ""
	}
	declared := map[string]bool{}
	rows, err := e.pool.Query(ctx, `SELECT lower(name) FROM model.dimension_property WHERE dimension_id=$1::uuid`, dimensionID)
	if err != nil {
		return ""
	}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			declared[n] = true
		}
	}
	rows.Close()
	var missing []string
	for k := range props {
		if !declared[strings.ToLower(k)] {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	sort.Strings(missing)
	return fmt.Sprintf(" — note: %s not declared on this dimension; propose add_dimension_property with exactly that name to let formulas read it",
		strings.Join(missing, ", "))
}

// ── create_grid ───────────────────────────────────────────────────────────────

type createGridParams struct {
	Name         string   `json:"name"`
	RevisionID   string   `json:"revision_id"`
	MetricIDs    []string `json:"metric_ids"`
	DimensionIDs []string `json:"dimension_ids"`
	// "metrics" and "dimensions" are what the model writes (by name): they
	// used to be dropped without a word, leaving every grid it created with
	// no metric and no dimension.
	Metrics    []string `json:"metrics"`
	Dimensions []string `json:"dimensions"`
	Tags       []string `json:"tags"`
}

func (e *WriteExecutor) createGrid(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p createGridParams
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	p.MetricIDs = append(p.MetricIDs, p.Metrics...)
	p.DimensionIDs = append(p.DimensionIDs, p.Dimensions...)
	revID := e.effectiveRevision(p.RevisionID)

	var newID string
	var err error
	if revID != "" {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.grid_def (model_id, name, revision_id, tags)
			VALUES ($1::uuid, $2, $3::uuid, $4) RETURNING id::text
		`, e.modelID, p.Name, revID, tags.Clean(p.Tags)).Scan(&newID)
	} else {
		err = e.pool.QueryRow(ctx, `
			INSERT INTO model.grid_def (model_id, name, tags)
			VALUES ($1::uuid, $2, $3) RETURNING id::text
		`, e.modelID, p.Name, tags.Clean(p.Tags)).Scan(&newID)
	}
	if err != nil {
		return "", "", fmt.Errorf("insert grid: %w", err)
	}

	// Attach through add_grid_metric and add_grid_dimension, so every id gets
	// their checks: it belongs to this model, it resolves into the working
	// revision (or by name), the one-grid rule, and the grid's time
	// validation. Raw inserts here once filed another model's metric, and a
	// metric of a revision with no counterpart, under a new grid, refused
	// metric names, and reported dimensions attached that were not.
	// Dimensions before metrics: a metric using a time function attached to
	// a grid that has no time dimension YET was refused
	// (TIME_DIMENSION_REQUIRED) although the same step was adding one.
	var attachedMetrics, attachedDims int
	var skipped []string
	for _, did := range p.DimensionIDs {
		params, _ := json.Marshal(map[string]string{"grid_id": newID, "dimension_id": did})
		if _, _, err := e.addGridDimension(ctx, params); err != nil {
			skipped = append(skipped, fmt.Sprintf("dimension %s: %v", did, err))
			continue
		}
		attachedDims++
	}
	for _, mid := range p.MetricIDs {
		params, _ := json.Marshal(map[string]string{"grid_id": newID, "metric_id": mid})
		if _, _, err := e.addGridMetric(ctx, params); err != nil {
			skipped = append(skipped, fmt.Sprintf("metric %s: %v", mid, err))
			continue
		}
		attachedMetrics++
	}

	// All or nothing: a grid missing what the step listed is not the grid
	// that was proposed. Reported as "not attached" it passed the plan check
	// with formulas written in place of metric names.
	if len(skipped) > 0 {
		// The grid row and what did attach go with it (grid_metric and
		// grid_dimension cascade): a failed step leaves nothing behind.
		_, _ = e.pool.Exec(ctx, `DELETE FROM model.grid_def WHERE id=$1::uuid`, newID)
		return "", "", fmt.Errorf("grid %q: could not attach %s — create each metric (create_metric) in an earlier step and list it here by name",
			p.Name, strings.Join(skipped, "; "))
	}
	return fmt.Sprintf("Grid '%s' created (id: %s, %d metrics, %d dimensions attached)",
		p.Name, newID, attachedMetrics, attachedDims), newID, nil
}

// ── add_grid_metric ───────────────────────────────────────────────────────────

func (e *WriteExecutor) addGridMetric(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		GridID   string `json:"grid_id"`
		MetricID string `json:"metric_id"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.GridID == "" || p.MetricID == "" {
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
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.GridID == "" || p.DimensionID == "" {
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

// ── set_tags ──────────────────────────────────────────────────────────────────

// tagTables are the definitions that carry tags, keyed by the kind name
// requireInModel resolves (by id, or by exact name in the working revision).
var tagTables = map[string]string{
	"metric":    "model.metric_def",
	"dimension": "model.dimension_def",
	"dashboard": "model.dashboard_def",
	"grid":      "model.grid_def",
}

// optionalTags is tags.Clean for an optional field: nil (not sent) stays nil
// so `tags = COALESCE($n, tags)` keeps what the row has.
func optionalTags(in *[]string) []string {
	if in == nil {
		return nil
	}
	return tags.Clean(*in)
}

// setTags replaces the tags on an existing metric, dimension, dashboard or
// grid — the tag editor the console has on each. An empty list clears them.
func (e *WriteExecutor) setTags(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Kind string    `json:"kind"`
		ID   string    `json:"id"`
		Tags *[]string `json:"tags"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	table, ok := tagTables[p.Kind]
	if !ok {
		return "", "", fmt.Errorf(`kind must be "metric", "dimension", "dashboard" or "grid"`)
	}
	if p.ID == "" {
		return "", "", fmt.Errorf("id is required (the %s's id or exact name)", p.Kind)
	}
	if p.Tags == nil {
		return "", "", fmt.Errorf("tags is required — send [] to clear them")
	}
	id, err := e.requireInModel(ctx, p.Kind, p.ID)
	if err != nil {
		return "", "", err
	}
	clean := tags.Clean(*p.Tags)
	if _, err := e.pool.Exec(ctx, `UPDATE `+table+` SET tags=$2 WHERE id=$1::uuid`, id, clean); err != nil {
		return "", "", fmt.Errorf("set tags: %w", err)
	}
	if len(clean) == 0 {
		return fmt.Sprintf("Tags cleared on %s %s", p.Kind, p.ID), "", nil
	}
	return fmt.Sprintf("Tags on %s %s set to %s", p.Kind, p.ID, strings.Join(clean, ", ")), "", nil
}

// ── create_dashboard ──────────────────────────────────────────────────────────

func (e *WriteExecutor) createDashboard(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Name       string   `json:"name"`
		Tags       []string `json:"tags"`
		RevisionID string   `json:"revision_id"`
		Folder     string   `json:"folder"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	p.Tags = tags.Clean(p.Tags)
	folderID, err := e.resolveFolder(ctx, p.Folder)
	if err != nil {
		return "", "", err
	}
	revID := e.effectiveRevision(p.RevisionID)
	if revID == "" {
		_ = e.pool.QueryRow(ctx, `SELECT COALESCE(active_revision_id::text,'') FROM core.model WHERE id=$1::uuid`, e.modelID).Scan(&revID)
	}

	var newID string
	err = e.pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_def (model_id, name, tags, revision_id, folder_id)
		VALUES ($1::uuid, $2, $3, NULLIF($4,'')::uuid, $5::uuid) RETURNING id::text
	`, e.modelID, p.Name, p.Tags, revID, folderID).Scan(&newID)
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
		// Title: the widget's header text, shown with it (show_title, on
		// by default when a title is given).
		Title     *string `json:"title"`
		ShowTitle *bool   `json:"show_title"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.DashboardID == "" || p.WidgetType == "" {
		return "", "", fmt.Errorf("dashboard_id and widget_type are required")
	}
	mappedDashID, err := e.requireInModel(ctx, "dashboard", p.DashboardID)
	if err != nil {
		return "", "", err
	}
	p.DashboardID = mappedDashID
	if p.WidgetType == "chart" {
		p.RefID, p.WidgetProps = hoistChartGrid(p.RefID, p.WidgetProps)
	}
	if p.WidgetProps, err = e.checkWidgetProps(ctx, p.WidgetProps); err != nil {
		return "", "", err
	}
	p.WidgetProps = widgetDefaults(p.WidgetType, p.WidgetProps)
	// The typed refs get the same in-model + cross-revision resolution as the
	// dashboard itself: a widget storing another revision's UUID is exactly
	// the "dashboard renders blank" failure the console shows when refs go
	// stale — the widget looks configured but resolves to nothing at render
	// time. Widget types whose ref is not a metric/grid (forms, automation
	// rules, integrations) pass through unchanged, as before.
	if err := modeledit.CheckWidgetType(p.WidgetType); err != nil {
		return "", "", err
	}
	if p.RefID, err = e.resolveWidgetRef(ctx, p.WidgetType, p.RefID); err != nil {
		return "", "", err
	}
	if err := validateWidgetContent(p.WidgetType, p.Content); err != nil {
		return "", "", err
	}
	// A chart's widget_props carry their own UUIDs (plotted dimension and
	// series metrics); remap them the same way or the chart body would query
	// the wrong revision even when ref_id is right.
	if p.WidgetType == "chart" && len(p.WidgetProps) > 0 && string(p.WidgetProps) != "null" {
		remapped, propErr := e.remapChartProps(ctx, p.WidgetProps)
		if propErr != nil {
			return "", "", propErr
		}
		p.WidgetProps = e.defaultChartDimension(ctx, p.RefID, remapped)
		gridRef := ""
		if p.RefID != nil {
			gridRef = *p.RefID
		}
		if err := modeledit.CheckChartMetrics(ctx, e.pool, gridRef, p.WidgetProps); err != nil {
			return "", "", err
		}
	}
	if p.WidgetType == "grid" && len(p.WidgetProps) > 0 && string(p.WidgetProps) != "null" {
		remapped, propErr := e.remapGridWidgetMetrics(ctx, p.WidgetProps)
		if propErr != nil {
			return "", "", propErr
		}
		p.WidgetProps = remapped
		gridRef := ""
		if p.RefID != nil {
			gridRef = *p.RefID
		}
		if err := modeledit.CheckGridWidgetMetrics(ctx, e.pool, gridRef, p.WidgetProps); err != nil {
			return "", "", err
		}
	}
	if p.SizeW < 20 {
		p.SizeW = 200
	}
	if p.SizeH < 20 {
		p.SizeH = 60
	}
	if err := e.checkWidgetPlacement(ctx, p.DashboardID, p.PosX, p.PosY, p.SizeW, p.SizeH); err != nil {
		return "", "", err
	}
	var propsStr *string
	if len(p.WidgetProps) > 0 && string(p.WidgetProps) != "null" {
		s := string(p.WidgetProps)
		propsStr = &s
	}

	var newID string
	err = e.pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_widget
		  (dashboard_id, widget_type, ref_id, content, pos_x, pos_y, size_w, size_h, widget_props, sort_order, col_start, col_span, title, show_title)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, 0, 1, 12, NULLIF(btrim($10::text),''), $11)
		RETURNING id::text
	`, p.DashboardID, p.WidgetType, p.RefID, p.Content,
		p.PosX, p.PosY, p.SizeW, p.SizeH, propsStr, p.Title, widgetShowTitle(p.Title, p.ShowTitle)).Scan(&newID)
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
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.Name == "" {
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
			INSERT INTO model.metric_def (model_id, name, label, formula, is_input, revision_id, format, format_decimals, format_currency, agg_rule, time_summary, tags, lineage_id, highlight_rules, picklist_allow_parents)
			SELECT model_id, name, label, formula, is_input, $2::uuid, format, format_decimals, format_currency, agg_rule, time_summary, tags, lineage_id, highlight_rules, picklist_allow_parents
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
				                                 dimension_type, time_granularity, fiscal_year_start_month, tags, lineage_id, business_maintained)
				SELECT model_id, name, agg_rule, properties, $2::uuid, source_property,
				       dimension_type, time_granularity, fiscal_year_start_month, tags, lineage_id, business_maintained
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
				INSERT INTO model.dimension_member (dimension_id, code, label, properties, sort_order, period_start, period_end, time_index, lineage_id, formula)
				SELECT dm.new_id, m.code, m.label, m.properties, m.sort_order, m.period_start, m.period_end, m.time_index, m.lineage_id, m.formula
				FROM model.dimension_member m
				JOIN dim_map dm ON dm.old_id = m.dimension_id
				RETURNING id
			),
			new_grids AS (
				INSERT INTO model.grid_def (model_id, name, revision_id, tags)
				SELECT model_id, name, $2::uuid, tags
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
		// A pick-list holds members of the copy's own dimension.
		if err := modeledit.RemapPicklistDimensions(ctx, tx, e.modelID, newID, srcID); err != nil {
			return "", "", err
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
			  (model_id, revision_name, metric_id, dim_members, value, entered_by, revision_id, source_ref, entered_at, text_value)
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
				fi.source_ref, fi.entered_at, fi.text_value
			FROM runtime.fact_input fi
			JOIN model.metric_def old_m ON old_m.id = fi.metric_id
			JOIN model.metric_def new_m ON new_m.model_id = old_m.model_id
			                           AND new_m.name = old_m.name
			                           AND new_m.revision_id = $2::uuid
			WHERE fi.revision_id = $3::uuid AND fi.model_id = $1::uuid
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("copy facts into new revision: %w", err)
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

		// Copy integrations — the same statement as the developer-console
		// duplicateRevision's Step G (internal/gateway/handler.go), which
		// says what it remaps and why: a connector names its target in the
		// target_id column and again in config.target_id (the copy its runs
		// read and write), and both are pointed at the new revision's copy of
		// the grid, form, dashboard, dimension or metric. It runs here, after
		// forms and dashboards are copied: run before them, as it used to, a
		// form or dashboard target found no copy and kept naming the source
		// revision's row. It runs before the automation rules, whose
		// source_integration_id is pointed at these copies, and before the
		// widget ref_id remap, which resolves integration_button widgets
		// against them.
		if _, err := tx.Exec(ctx, `
			WITH idmap(kind, old_id, new_id) AS (
				          SELECT 'grid', o.id, n.id FROM model.grid_def o
				            JOIN model.grid_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
				           WHERE o.model_id = $1::uuid AND o.revision_id <> $2::uuid
				UNION ALL SELECT 'form', o.id, n.id FROM model.form_def o
				            JOIN model.form_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
				           WHERE o.model_id = $1::uuid AND o.revision_id <> $2::uuid
				UNION ALL SELECT 'dashboard', o.id, n.id FROM model.dashboard_def o
				            JOIN model.dashboard_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
				           WHERE o.model_id = $1::uuid AND o.revision_id <> $2::uuid
				UNION ALL SELECT 'dimension', o.id, n.id FROM model.dimension_def o
				            JOIN model.dimension_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
				           WHERE o.model_id = $1::uuid AND o.revision_id <> $2::uuid
				UNION ALL SELECT 'metric', o.id, n.id FROM model.metric_def o
				            JOIN model.metric_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
				           WHERE o.model_id = $1::uuid AND o.revision_id <> $2::uuid
			)
			-- A model link keeps link_id, its source columns and its owner: its source is
			-- another model, which a revision copy does not remap (migration 126).
			INSERT INTO model.integration_def (model_id, revision_id, name, description, type, target_type, target_id, config,
			                                   status, tags, direction, enabled, connection_id, config_version,
			                                   last_tested_hash, last_tested_at,
			                                   link_id, source_model_id, source_enabled, source_switched_by, source_switched_at, link_owner)
			SELECT i.model_id, $2::uuid, i.name, i.description, i.type, i.target_type,
				COALESCE((SELECT m.new_id FROM idmap m WHERE m.old_id = i.target_id
				          ORDER BY m.kind = COALESCE(NULLIF(i.target_type,''), 'grid') DESC LIMIT 1), i.target_id),
				CASE WHEN jsonb_typeof(i.config) = 'object' THEN COALESCE((
					SELECT jsonb_object_agg(e.key, COALESCE((
						SELECT to_jsonb(m.new_id::text) FROM idmap m
						 WHERE e.key LIKE '%\_id' AND jsonb_typeof(e.value) = 'string'
						   AND replace(m.old_id::text, '-', '') = lower(regexp_replace(btrim(e.value #>> '{}'), '^urn:uuid:|[{}-]', '', 'gi'))
						 ORDER BY m.kind = CASE WHEN e.key = 'target_id'
						                        THEN COALESCE(NULLIF(i.config->>'target_type',''), NULLIF(i.target_type,''), 'grid')
						                        ELSE regexp_replace(e.key, '^(.*_)?([^_]+)_id$', '\2') END DESC
						 LIMIT 1), e.value))
					FROM jsonb_each(i.config) e), i.config)
				ELSE i.config END,
				i.status, i.tags, i.direction, i.enabled, i.connection_id, i.config_version,
				i.last_tested_hash, i.last_tested_at,
				i.link_id, i.source_model_id, i.source_enabled, i.source_switched_by, i.source_switched_at, i.link_owner
			FROM model.integration_def i
			WHERE i.model_id=$1::uuid AND i.revision_id=$3::uuid
		`, e.modelID, newID, srcID); err != nil {
			return "", "", fmt.Errorf("copy integrations into new revision: %w", err)
		}

		// Copy workflow definitions and automation rules — the same
		// statement as Step H of the developer-console duplicateRevision
		// (internal/gateway/handler.go), which says what it remaps and why:
		// a workflow's subject_config and context_schema, and a rule's
		// workflow, form, grid and connector refs, are pointed at this
		// revision's copies, and a schedule rule keeps its cron settings
		// (without them the copy failed automation_rule_schedule_cron_chk).
		// An AI-created workflow needs an automation rule to ever fire, and
		// update_workflow_def can only find a pre-session workflow if this
		// copy step exists.
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
				   status, created_by, updated_by, published_at, archived_at, context_schema, revision_id,
			   single_active_instance, approver_may_start)
				SELECT wd.application_id, wd.name, wd.description, wd.trigger_event, wd.subject_type,
					-- subject_config binds the workflow to a form ({"form_id"}) or a
					-- grid metric ({"grid_id","metric_id"}) of THIS revision; copied
					-- verbatim it kept pointing at the source revision's objects.
					COALESCE(wd.subject_config, '{}'::jsonb)
					|| COALESCE((SELECT jsonb_build_object('form_id', nfd.id::text) FROM model.form_def ofd
					             JOIN model.form_def nfd ON nfd.model_id = ofd.model_id AND nfd.name = ofd.name AND nfd.revision_id = $2::uuid
					             WHERE ofd.id::text = wd.subject_config->>'form_id'), '{}'::jsonb)
					|| COALESCE((SELECT jsonb_build_object('grid_id', ngd.id::text) FROM model.grid_def ogd
					             JOIN model.grid_def ngd ON ngd.model_id = ogd.model_id AND ngd.name = ogd.name AND ngd.revision_id = $2::uuid
					             WHERE ogd.id::text = wd.subject_config->>'grid_id'), '{}'::jsonb)
					|| COALESCE((SELECT jsonb_build_object('metric_id', nmd.id::text) FROM model.metric_def omd
					             JOIN model.metric_def nmd ON nmd.model_id = omd.model_id AND nmd.name = omd.name AND nmd.revision_id = $2::uuid
					             WHERE omd.id::text = wd.subject_config->>'metric_id'), '{}'::jsonb),
					wd.steps,
					wd.status, wd.created_by, wd.updated_by, wd.published_at, wd.archived_at,
					COALESCE((
						SELECT jsonb_agg(
							e.elem
							|| COALESCE((SELECT jsonb_build_object('dimension_id', dm.new_id::text) FROM dim_map dm WHERE dm.old_id::text = e.elem->>'dimension_id'), '{}'::jsonb)
							ORDER BY e.ord)
						FROM jsonb_array_elements(wd.context_schema) WITH ORDINALITY AS e(elem, ord)
					), '[]'::jsonb),
					$2::uuid,
					wd.single_active_instance, wd.approver_may_start
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
				   workflow_def_id, source_form_id, source_grid_id, source_integration_id,
				   cron_expr, timezone, misfire_policy, max_retries, retry_backoff_seconds, revision_id)
				SELECT ar.application_id, ar.name, ar.description, ar.trigger_type, ar.workflow_name, ar.enabled,
					wm.new_id,
					(SELECT nf.id FROM model.form_def ofd
					 JOIN model.form_def nf ON nf.model_id = ofd.model_id AND nf.name = ofd.name AND nf.revision_id = $2::uuid
					 WHERE ofd.id = ar.source_form_id),
					(SELECT ng.id FROM model.grid_def og
					 JOIN model.grid_def ng ON ng.model_id = og.model_id AND ng.name = og.name AND ng.revision_id = $2::uuid
					 WHERE og.id = ar.source_grid_id),
					COALESCE((SELECT min(ni.id::text)::uuid FROM model.integration_def oi
					          JOIN model.integration_def ni ON ni.model_id = oi.model_id AND ni.name = oi.name AND ni.revision_id = $2::uuid
					          WHERE oi.id = ar.source_integration_id AND oi.model_id = $1::uuid
					          HAVING count(*) = 1), ar.source_integration_id),
					ar.cron_expr, ar.timezone, ar.misfire_policy, ar.max_retries, ar.retry_backoff_seconds,
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
			-- The copied widget still holds the source revision's ref_id, so
			-- it is read off the copy itself. Matching old to new widgets by
			-- (type, sort_order) paired every KPI tile with every other when
			-- they shared a sort_order (0 for every widget added), and all of
			-- them came out pointing at one metric.
			widget_map AS (
				SELECT nw.id AS new_widget_id, nw.widget_type, nw.ref_id AS old_ref_id
				FROM model.dashboard_widget nw
				JOIN dash_map dm ON dm.new_id = nw.dashboard_id
				WHERE nw.ref_id IS NOT NULL AND nw.ref_id <> ''
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
	if err := decodeParams(raw, &p); err != nil {
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

	ws := workflow.NewStoreOn(e.pool)
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
	// ApproverMayStart: its business-admin approver may also start it (a
	// planning round); nil leaves it unchanged.
	ApproverMayStart *bool `json:"approver_may_start"`
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
	if err := decodeParams(raw, &p); err != nil {
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

	ws := workflow.NewStoreOn(e.pool)
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
	if p.ApproverMayStart != nil && *p.ApproverMayStart != def.ApproverMayStart {
		if err := ws.SetWorkflowDefApproverMayStart(ctx, p.WorkflowDefID, *p.ApproverMayStart); err != nil {
			return "", "", fmt.Errorf("set approver_may_start: %w", err)
		}
		def.ApproverMayStart = *p.ApproverMayStart
	}
	var stepCount int
	if arr, uErr := unmarshalArrayLen(def.Steps); uErr == nil {
		stepCount = arr
	}
	// The console saves an incomplete draft too; what it adds is the
	// Validate verdict next to it, so the model learns what a developer
	// would still have to fix rather than discovering it at publish.
	return fmt.Sprintf("Workflow '%s' updated (status: %s, %d step(s), single_active_instance: %v) — %s",
		def.Name, def.Status, stepCount, def.SingleActiveInstance, e.validationNote(ctx, def)), "", nil
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
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.Name == "" {
		return "", "", fmt.Errorf("name is required")
	}
	if p.Label == "" {
		p.Label = p.Name
	}
	revID := e.effectiveRevision(p.RevisionID)

	fs := crudapp.NewStoreOn(e.pool)
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
	if err := decodeParams(raw, &p); err != nil {
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

	fs := crudapp.NewStoreOn(e.pool)
	if err := fs.UpdateForm(ctx, p.FormID, name, label, fields); err != nil {
		return "", "", fmt.Errorf("update form: %w", err)
	}
	return fmt.Sprintf("Form '%s' updated (%d field(s))", name, len(fields)), "", nil
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

// setUserAccessRules replaces the target user's member rules in the active
// revision — the rules it can name — mirroring the business-admin console's
// PUT /access-rules for that slice (writeguard.ReplaceUserMemberRulesInRevision).
// Metric and button rules, rules on members gone from the active revision
// (which still restrict old revisions' copies) and rules on other models'
// members are kept: the tool cannot express them, so it must not wipe them.
// This is a business-admin capability deliberately extended to the
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
	if err := decodeParams(raw, &p); err != nil {
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
	inputs := make([]writeguard.RuleInput, len(rules))
	for i, r := range rules {
		inputs[i] = writeguard.RuleInput{Type: "dimension_member", RefID: r.memberID, Access: r.access}
	}
	if err := writeguard.ReplaceUserMemberRulesInRevision(ctx, tx, targetID, activeRev, inputs); err != nil {
		return "", "", fmt.Errorf("set rules: %w", err)
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
	return fmt.Sprintf("Member access rules for %s in the active revision replaced: %s (previous member rules there removed; unlisted members stay fully accessible; metric rules and rules on members not in the active revision are kept)",
		p.UserEmail, strings.Join(parts, ", ")), "", nil
}

// validationName is the metric name validation messages name: the new
// name when the update renames the metric, else the stored one.
func validationName(requested, stored string) string {
	if requested != "" {
		return requested
	}
	return stored
}

// availableNames lists what the working revision has of a kind, for a
// not-found message: the assistant invented a grid id live and was told only
// that it did not exist.
func (e *WriteExecutor) availableNames(ctx context.Context, kind string) string {
	table := map[string]string{"grid": "model.grid_def", "metric": "model.metric_def", "dimension": "model.dimension_def", "dashboard": "model.dashboard_def"}[kind]
	if table == "" || e.revID == "" {
		return ""
	}
	rows, err := e.pool.Query(ctx, `SELECT name, id::text FROM `+table+` WHERE model_id=$1::uuid AND revision_id=$2::uuid ORDER BY name LIMIT 40`, e.modelID, e.revID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var items []string
	for rows.Next() {
		var name, id string
		if rows.Scan(&name, &id) == nil {
			items = append(items, fmt.Sprintf("%s (%s)", name, id))
		}
	}
	if len(items) == 0 {
		return fmt.Sprintf(" (the working revision has no %s yet)", kind)
	}
	return fmt.Sprintf(" — the working revision's %ss: %s", kind, strings.Join(items, "; "))
}

// nearID returns the one id of the model's resources of a kind (any
// revision) within two edits of id, or "".
func (e *WriteExecutor) nearID(ctx context.Context, kind, id string) string {
	query := map[string]string{
		"grid":      `SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid`,
		"metric":    `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid`,
		"dimension": `SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid`,
		"dashboard": `SELECT id::text FROM model.dashboard_def WHERE model_id=$1::uuid`,
		"dashboard_widget": `SELECT w.id::text FROM model.dashboard_widget w
			JOIN model.dashboard_def d ON d.id = w.dashboard_id WHERE d.model_id=$1::uuid`,
	}[kind]
	if query == "" {
		return ""
	}
	rows, err := e.pool.Query(ctx, query, e.modelID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	match := ""
	for rows.Next() {
		var cand string
		if rows.Scan(&cand) != nil || editDistanceWithin(strings.ToLower(id), cand, 2) < 0 {
			continue
		}
		if match != "" {
			return "" // two near matches: not a slip, a guess
		}
		match = cand
	}
	return match
}

// editDistanceWithin is the edit distance of a and b when it is at most max,
// else -1.
func editDistanceWithin(a, b string, max int) int {
	if d := len(a) - len(b); d > max || -d > max {
		return -1
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			rowMin = min(rowMin, cur[j])
		}
		if rowMin > max {
			return -1
		}
		prev, cur = cur, prev
	}
	if prev[len(b)] > max {
		return -1
	}
	return prev[len(b)]
}

// dashboardCanvasWidth is the width, in pixels, of the canvas widgets are
// placed on (see "Dashboard geometry" in the prompt).
const dashboardCanvasWidth = 1200

// checkWidgetPlacement refuses a widget off the canvas or on top of one the
// dashboard already has. Live, the assistant put KPI tiles at x=1200 and
// three more at x=1, 2 and 3 on one row.
func (e *WriteExecutor) checkWidgetPlacement(ctx context.Context, dashboardID string, x, y, w, h int) error {
	if x < 0 || y < 0 || x+w > dashboardCanvasWidth {
		return fmt.Errorf("the widget at x=%d, width %d does not fit the %d px canvas: keep pos_x >= 0 and pos_x + size_w <= %d",
			x, w, dashboardCanvasWidth, dashboardCanvasWidth)
	}
	rows, err := e.pool.Query(ctx, `SELECT widget_type, pos_x, pos_y, size_w, size_h FROM model.dashboard_widget WHERE dashboard_id=$1::uuid`, dashboardID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	bottom := 0
	var clash string
	for rows.Next() {
		var typ string
		var ox, oy, ow, oh int
		if rows.Scan(&typ, &ox, &oy, &ow, &oh) != nil {
			continue
		}
		bottom = max(bottom, oy+oh)
		if clash == "" && x < ox+ow && ox < x+w && y < oy+oh && oy < y+h {
			clash = fmt.Sprintf("the %s widget at x=%d, y=%d (%d×%d)", typ, ox, oy, ow, oh)
		}
	}
	if clash != "" {
		return fmt.Errorf("the widget at x=%d, y=%d (%d×%d) overlaps %s — place it beside it, or below y=%d where the dashboard's widgets end",
			x, y, w, h, clash, bottom)
	}
	return nil
}

// hoistChartGrid moves a grid named inside a chart's props ("ref_id",
// "grid_id" or "grid" under "chart") up to the widget's ref_id when the
// widget names none: live, the assistant wrote {"chart": {"ref_id":
// "Dashboard Data", …}} and the chart was refused for having no grid.
func hoistChartGrid(refID *string, props json.RawMessage) (*string, json.RawMessage) {
	if (refID != nil && *refID != "") || len(props) == 0 {
		return refID, props
	}
	var all map[string]any
	if json.Unmarshal(props, &all) != nil {
		return refID, props
	}
	chart, ok := all["chart"].(map[string]any)
	if !ok {
		return refID, props
	}
	for _, k := range []string{"ref_id", "grid_id", "grid"} {
		if s, ok := chart[k].(string); ok && s != "" {
			delete(chart, k)
			out, err := json.Marshal(all)
			if err != nil {
				return refID, props
			}
			return &s, out
		}
	}
	return refID, props
}

// defaultChartDimension sets a chart's plotted dimension to its grid's only
// dimension when the props leave it out.
func (e *WriteExecutor) defaultChartDimension(ctx context.Context, refID *string, props json.RawMessage) json.RawMessage {
	if refID == nil || *refID == "" {
		return props
	}
	var all map[string]any
	if json.Unmarshal(props, &all) != nil {
		return props
	}
	chart, ok := all["chart"].(map[string]any)
	if !ok {
		return props
	}
	if d, _ := chart["dimension_id"].(string); d != "" {
		return props
	}
	var dims []string
	rows, err := e.pool.Query(ctx, `SELECT dimension_id::text FROM model.grid_dimension WHERE grid_id=$1::uuid`, *refID)
	if err != nil {
		return props
	}
	for rows.Next() {
		var d string
		if rows.Scan(&d) == nil {
			dims = append(dims, d)
		}
	}
	rows.Close()
	if len(dims) != 1 {
		return props
	}
	chart["dimension_id"] = dims[0]
	if out, err := json.Marshal(all); err == nil {
		return out
	}
	return props
}

// widgetDefaults fills what a widget the assistant adds leaves unsaid with
// what such a widget usually means: a KPI tile is the metric's total, and a
// chart leaves out total members (an FY point after twelve months rose to
// the year's sum, live). The console's editor starts from the other
// settings and has the developer pick; the assistant's plan rarely does.
func widgetDefaults(widgetType string, props json.RawMessage) json.RawMessage {
	if widgetType != "metric_kpi" && widgetType != "chart" {
		return props
	}
	all := map[string]any{}
	if len(props) > 0 && string(props) != "null" {
		if json.Unmarshal(props, &all) != nil {
			return props
		}
	}
	switch widgetType {
	case "metric_kpi":
		if _, set := all["kpi_context_mode"]; set {
			return props
		}
		if _, scoped := all["kpi_scope"]; scoped {
			return props
		}
		all["kpi_context_mode"] = "total"
	case "chart":
		chart, ok := all["chart"].(map[string]any)
		if !ok {
			return props
		}
		if _, set := chart["hide_rollup_members"]; set {
			return props
		}
		chart["hide_rollup_members"] = true
	}
	out, err := json.Marshal(all)
	if err != nil {
		return props
	}
	return out
}

// widgetShowTitle is a new widget's show_title: as sent, else on when it has
// a title.
func widgetShowTitle(title *string, show *bool) bool {
	if show != nil {
		return *show
	}
	return title != nil && strings.TrimSpace(*title) != ""
}
