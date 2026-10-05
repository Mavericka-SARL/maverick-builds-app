package aiassistant

// The change-and-remove half of the developer console. Until 2026-09-28 the
// assistant could build a model but not take any of it apart: it had no way
// to delete a dimension, rename a member's code, remove a metric from a grid,
// move a widget or file a dashboard in a folder, although the developer does
// all of these from the screens. Each tool below mirrors one developer
// endpoint, named in its comment, and shares that endpoint's validators
// (internal/metricformula, internal/timedim) and data edits
// (internal/modeledit), so the two cannot drift apart. Like every other
// write, these act on the session's draft revision — except the business
// role tools, which act on the tenant's live roles as the console does.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/imagedata"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/modeledit"
	"github.com/mavericks-engine/mavericks/internal/timedim"
	"github.com/mavericks-engine/mavericks/internal/workflow"
)

type writeTool func(context.Context, json.RawMessage) (string, string, error)

func (e *WriteExecutor) editTools() map[string]writeTool {
	return map[string]writeTool{
		"delete_dimension":          e.deleteDimension,
		"delete_dimension_member":   e.deleteDimensionMember,
		"generate_time_members":     e.generateTimeMembers,
		"reorder_dimension_members": e.reorderDimensionMembers,
		"reorder_grid_metrics":      e.reorderGridMetrics,
		"update_grid":               e.updateGrid,
		"delete_grid":               e.deleteGrid,
		"remove_grid_metric":        e.removeGridMetric,
		"remove_grid_dimension":     e.removeGridDimension,
		"update_grid_dimension":     e.updateGridDimension,
		"update_dashboard":          e.updateDashboard,
		"delete_dashboard":          e.deleteDashboard,
		"update_dashboard_widget":   e.updateDashboardWidget,
		"delete_dashboard_widget":   e.deleteDashboardWidget,
		"create_dashboard_folder":   e.createDashboardFolder,
		"update_dashboard_folder":   e.updateDashboardFolder,
		"delete_dashboard_folder":   e.deleteDashboardFolder,
		"archive_workflow_def":      e.archiveWorkflowDef,
		"restore_workflow_def":      e.restoreWorkflowDef,
		"duplicate_workflow_def":    e.duplicateWorkflowDef,
		"update_business_role":      e.updateBusinessRole,
		"delete_business_role":      e.deleteBusinessRole,
		"set_role_dashboards":       e.setRoleDashboards,
		"backfill_form_integration": e.backfillFormIntegration,
	}
}

// present reports which top-level keys a step's params carry, so a tool can
// tell "left out" from "set to empty".
func present(raw json.RawMessage) map[string]bool {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// ── dimensions and members ────────────────────────────────────────────────────

// deleteDimension is DELETE /api/developer/dimensions/{id}: refused while a
// formula names the dimension (DIMENSION_IN_USE) or a property grouping
// groups its members.
func (e *WriteExecutor) deleteDimension(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		DimensionID string `json:"dimension_id"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.DimensionID == "" {
		return "", "", fmt.Errorf("dimension_id is required")
	}
	dimID, err := e.requireInModel(ctx, "dimension", p.DimensionID)
	if err != nil {
		return "", "", err
	}
	var name string
	_ = e.pool.QueryRow(ctx, `SELECT name FROM model.dimension_def WHERE id=$1::uuid`, dimID).Scan(&name)
	if err := metricformula.CheckDimensionNotInUse(ctx, e.pool, dimID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", err
	}
	if err := metricformula.CheckDimensionNotGrouped(ctx, e.pool, dimID); err != nil {
		return "", "", err
	}
	if err := modeledit.CheckDimensionDeletable(ctx, e.pool, dimID); err != nil {
		return "", "", err
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM model.dimension_def WHERE id=$1::uuid`, dimID); err != nil {
		return "", "", fmt.Errorf("delete dimension: %w", err)
	}
	return fmt.Sprintf("Dimension '%s' deleted", name), "", nil
}

// deleteDimensionMember is DELETE /api/developer/dimensions/{id}/members/{m}:
// refused while a formula names the member's code (MEMBER_IN_USE); the
// member's input values move to history, its children become top-level, and
// a time dimension is re-indexed.
func (e *WriteExecutor) deleteDimensionMember(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		DimensionID string `json:"dimension_id"`
		Code        string `json:"code"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.DimensionID == "" || p.Code == "" {
		return "", "", fmt.Errorf("dimension_id and code are required")
	}
	dimID, err := e.requireInModel(ctx, "dimension", p.DimensionID)
	if err != nil {
		return "", "", err
	}
	var memberID string
	if err := e.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
		dimID, p.Code).Scan(&memberID); err != nil {
		return "", "", fmt.Errorf("member %q not found in dimension (call list_dimensions to see codes)", p.Code)
	}
	if err := metricformula.CheckMemberNotInUse(ctx, e.pool, dimID, memberID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", err
	}
	var children int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM model.dimension_member WHERE parent_member_id=$1::uuid`, memberID).Scan(&children)
	if err := modeledit.DeleteMember(ctx, e.pool, dimID, memberID); err != nil {
		return "", "", fmt.Errorf("delete member: %w", err)
	}
	msg := fmt.Sprintf("Dimension member '%s' deleted (its input values moved to history)", p.Code)
	if children > 0 {
		msg += fmt.Sprintf("; its %d child member(s) are now top-level", children)
	}
	return msg, "", nil
}

// reorderDimensionMembers is PUT /api/developer/dimensions/{id}/members/order
// by codes: codes are exactly one level's members — the children of
// parent_code, or the top-level members when it is left out — in the wanted
// order, and the dimension is renumbered in tree order
// (modeledit.ReorderMembers). A time dimension is refused: its periods keep
// calendar order.
func (e *WriteExecutor) reorderDimensionMembers(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		DimensionID string   `json:"dimension_id"`
		ParentCode  string   `json:"parent_code"`
		Codes       []string `json:"codes"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.DimensionID == "" || p.Codes == nil {
		return "", "", fmt.Errorf("dimension_id and codes (the level's member codes in the wanted order) are required")
	}
	dimID, err := e.requireInModel(ctx, "dimension", p.DimensionID)
	if err != nil {
		return "", "", err
	}
	var parentID *string
	if p.ParentCode != "" {
		// The parents ReorderMembers accepts, by code: a member that members
		// of the dimension hang under (the level exists), then a member of
		// the dimension, then one of its declared parent dimension. Which
		// side a parent sits on follows the members, not the declared parent
		// dimension, which can be set or cleared after they exist.
		var pid string
		if err := e.pool.QueryRow(ctx, `
			SELECT p.id::text FROM model.dimension_member p
			WHERE p.code=$2 AND (
			      p.dimension_id=$1::uuid
			   OR p.dimension_id=(SELECT parent_dimension_id FROM model.dimension_def WHERE id=$1::uuid)
			   OR EXISTS (SELECT 1 FROM model.dimension_member c WHERE c.dimension_id=$1::uuid AND c.parent_member_id=p.id))
			ORDER BY EXISTS (SELECT 1 FROM model.dimension_member c WHERE c.dimension_id=$1::uuid AND c.parent_member_id=p.id) DESC,
			         (p.dimension_id=$1::uuid) DESC, p.id
			LIMIT 1`, dimID, p.ParentCode).Scan(&pid); err != nil {
			return "", "", fmt.Errorf("parent member %q not found (call list_dimensions to see codes)", p.ParentCode)
		}
		parentID = &pid
	}
	ids := make([]string, 0, len(p.Codes))
	for _, code := range p.Codes {
		var id string
		if err := e.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
			dimID, code).Scan(&id); err != nil {
			return "", "", fmt.Errorf("member %q not found in dimension (call list_dimensions to see codes)", code)
		}
		ids = append(ids, id)
	}
	res, err := modeledit.ReorderMembers(ctx, e.pool, dimID, parentID, ids)
	if err != nil {
		var re *modeledit.ReorderError
		if errors.As(err, &re) {
			return "", "", err
		}
		return "", "", fmt.Errorf("reorder members: %w", err)
	}
	level := "Top-level members"
	if res.ParentCode != "" {
		level = fmt.Sprintf("Members under '%s'", res.ParentCode)
	}
	return fmt.Sprintf("%s reordered: %s", level, strings.Join(res.Codes, ", ")), "", nil
}

// reorderGridMetrics is PUT /api/developer/grids/{id}/metrics/order: the
// order a grid shows its metrics in, given as every metric of the grid (by
// name or id), once each.
func (e *WriteExecutor) reorderGridMetrics(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		GridID  string   `json:"grid_id"`
		Metrics []string `json:"metrics"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.GridID == "" || len(p.Metrics) == 0 {
		return "", "", fmt.Errorf("grid_id and metrics (every metric of the grid, in the wanted order) are required")
	}
	gridID, err := e.requireInModel(ctx, "grid", p.GridID)
	if err != nil {
		return "", "", err
	}
	ids := make([]string, 0, len(p.Metrics))
	for _, ref := range p.Metrics {
		id, err := e.requireInModel(ctx, "metric", ref)
		if err != nil {
			return "", "", err
		}
		ids = append(ids, id)
	}
	if err := modeledit.ReorderGridMetrics(ctx, e.pool, gridID, ids); err != nil {
		var oe *modeledit.GridOrderError
		if errors.As(err, &oe) {
			return "", "", fmt.Errorf("%w (list_grids shows the grid's metrics)", err)
		}
		return "", "", fmt.Errorf("reorder grid metrics: %w", err)
	}
	return fmt.Sprintf("Grid metrics reordered: %s", strings.Join(p.Metrics, ", ")), "", nil
}

// generateTimeMembers is POST /api/developer/dimensions/{id}/members/generate:
// one period per step of the dimension's granularity from start to end, under
// an optional aggregate period, validated with the dimension's existing
// periods. Codes the dimension already has are skipped.
func (e *WriteExecutor) generateTimeMembers(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		DimensionID string `json:"dimension_id"`
		Start       string `json:"start"`
		End         string `json:"end"`
		ParentCode  string `json:"parent_code"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.DimensionID == "" || p.Start == "" || p.End == "" {
		return "", "", fmt.Errorf("dimension_id, start and end (YYYY-MM-DD) are required")
	}
	dimID, err := e.requireInModel(ctx, "dimension", p.DimensionID)
	if err != nil {
		return "", "", err
	}
	cfg, err := timedim.LoadConfig(ctx, e.pool, dimID)
	if err != nil {
		return "", "", fmt.Errorf("dimension not found")
	}
	if cfg.Type != timedim.TypeTime {
		return "", "", fmt.Errorf("periods can only be generated for a time dimension")
	}
	var parentID *string
	if p.ParentCode != "" {
		var pid string
		if err := e.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
			dimID, p.ParentCode).Scan(&pid); err != nil {
			return "", "", fmt.Errorf("parent period %q not found in this dimension", p.ParentCode)
		}
		parentID = &pid
	}
	start, err := timedim.ParseDate(p.Start)
	if err != nil {
		return "", "", err
	}
	end, err := timedim.ParseDate(p.End)
	if err != nil {
		return "", "", err
	}
	periods, err := timedim.GeneratePeriods(cfg, start, end)
	if err != nil {
		return "", "", err
	}
	if err := e.checkMembers(ctx, dimID, len(periods)); err != nil {
		return "", "", err
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	created, err := modeledit.InsertPeriods(ctx, tx, dimID, cfg.Granularity, periods, parentID)
	if err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return fmt.Sprintf("Generated %d %s period(s) from %s to %s (%d already existed)",
		created, cfg.Granularity, p.Start, p.End, len(periods)-created), "", nil
}

// ── grids ─────────────────────────────────────────────────────────────────────

// gridChangeTx applies one grid membership change and runs validate on the
// grid in the same transaction, as the developer endpoint's gridChangeTx
// does; a validation error rolls the change back.
func (e *WriteExecutor) gridChangeTx(ctx context.Context, gridID string,
	validate func(ctx context.Context, q metricformula.Querier, modelID, revisionID, gridID string) error,
	sql string, args ...any) (int64, error) {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	var modelID, revisionID string
	if err := tx.QueryRow(ctx,
		`SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.grid_def WHERE id=$1::uuid`, gridID,
	).Scan(&modelID, &revisionID); err != nil {
		return 0, err
	}
	if err := validate(ctx, tx, modelID, revisionID, gridID); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

// updateGrid is PATCH /api/developer/grids/{id}: a rename.
func (e *WriteExecutor) updateGrid(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		GridID string `json:"grid_id"`
		Name   string `json:"name"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.GridID == "" || strings.TrimSpace(p.Name) == "" {
		return "", "", fmt.Errorf("grid_id and name are required")
	}
	gridID, err := e.requireInModel(ctx, "grid", p.GridID)
	if err != nil {
		return "", "", err
	}
	var old string
	_ = e.pool.QueryRow(ctx, `SELECT name FROM model.grid_def WHERE id=$1::uuid`, gridID).Scan(&old)
	if _, err := e.pool.Exec(ctx, `UPDATE model.grid_def SET name=$2 WHERE id=$1::uuid`, gridID, p.Name); err != nil {
		return "", "", fmt.Errorf("rename grid: %w", err)
	}
	return fmt.Sprintf("Grid '%s' renamed to '%s'", old, p.Name), gridID, nil
}

// deleteGrid is DELETE /api/developer/grids/{id}: the grid, and the widgets
// that show it.
func (e *WriteExecutor) deleteGrid(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		GridID string `json:"grid_id"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.GridID == "" {
		return "", "", fmt.Errorf("grid_id is required")
	}
	gridID, err := e.requireInModel(ctx, "grid", p.GridID)
	if err != nil {
		return "", "", err
	}
	var name string
	_ = e.pool.QueryRow(ctx, `SELECT name FROM model.grid_def WHERE id=$1::uuid`, gridID).Scan(&name)
	if err := modeledit.DropWidgetsReferencing(ctx, e.pool, gridID); err != nil {
		return "", "", err
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM model.grid_def WHERE id=$1::uuid`, gridID); err != nil {
		return "", "", fmt.Errorf("delete grid: %w", err)
	}
	return fmt.Sprintf("Grid '%s' deleted, with the dashboard widgets that showed it", name), "", nil
}

// removeGridMetric is DELETE /api/developer/grids/{id}/metrics/{metricId}.
// The metric keeps existing; it is then free to join another grid.
func (e *WriteExecutor) removeGridMetric(ctx context.Context, raw json.RawMessage) (string, string, error) {
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
	gridID, err := e.requireInModel(ctx, "grid", p.GridID)
	if err != nil {
		return "", "", err
	}
	metricID, err := e.requireInModel(ctx, "metric", p.MetricID)
	if err != nil {
		return "", "", err
	}
	tag, err := e.pool.Exec(ctx, `DELETE FROM model.grid_metric WHERE grid_id=$1::uuid AND metric_id=$2::uuid`, gridID, metricID)
	if err != nil {
		return "", "", fmt.Errorf("remove metric from grid: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", "", fmt.Errorf("that metric is not on this grid (call list_grids)")
	}
	return "Metric removed from the grid", "", nil
}

// removeGridDimension is DELETE /api/developer/grids/{id}/dimensions/{dimId}:
// refused while a metric on the grid reads that dimension in a formula
// (metricformula.ValidateGridDimensional).
func (e *WriteExecutor) removeGridDimension(ctx context.Context, raw json.RawMessage) (string, string, error) {
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
	gridID, err := e.requireInModel(ctx, "grid", p.GridID)
	if err != nil {
		return "", "", err
	}
	dimID, err := e.requireInModel(ctx, "dimension", p.DimensionID)
	if err != nil {
		return "", "", err
	}
	n, err := e.gridChangeTx(ctx, gridID, metricformula.ValidateGridDimensional,
		`DELETE FROM model.grid_dimension WHERE grid_id=$1::uuid AND dimension_id=$2::uuid`, gridID, dimID)
	if err != nil {
		return "", "", err
	}
	if n == 0 {
		return "", "", fmt.Errorf("that dimension is not on this grid (call list_grids)")
	}
	return "Dimension removed from the grid", "", nil
}

// updateGridDimension is PATCH /api/developer/grids/{id}/dimensions/{dimId}:
// the hierarchy level the grid opens the dimension at (null = the default).
func (e *WriteExecutor) updateGridDimension(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		GridID       string `json:"grid_id"`
		DimensionID  string `json:"dimension_id"`
		DisplayLevel *int   `json:"display_level"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.GridID == "" || p.DimensionID == "" {
		return "", "", fmt.Errorf("grid_id and dimension_id are required")
	}
	if !present(raw)["display_level"] {
		return "", "", fmt.Errorf("display_level is required (a level number, or null for the default)")
	}
	gridID, err := e.requireInModel(ctx, "grid", p.GridID)
	if err != nil {
		return "", "", err
	}
	dimID, err := e.requireInModel(ctx, "dimension", p.DimensionID)
	if err != nil {
		return "", "", err
	}
	tag, err := e.pool.Exec(ctx, `UPDATE model.grid_dimension SET display_level=$3 WHERE grid_id=$1::uuid AND dimension_id=$2::uuid`,
		gridID, dimID, p.DisplayLevel)
	if err != nil {
		return "", "", fmt.Errorf("set display level: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "", "", fmt.Errorf("that dimension is not on this grid (call list_grids)")
	}
	if p.DisplayLevel == nil {
		return "Grid dimension display level reset to the default", "", nil
	}
	return fmt.Sprintf("Grid dimension display level set to %d", *p.DisplayLevel), "", nil
}

// ── dashboards, widgets, folders ──────────────────────────────────────────────

// resolveFolder resolves a folder reference (id or exact name; "" = none)
// into the working revision, refusing another model's folder — the check the
// developer endpoint's validateDashboardFolder makes.
func (e *WriteExecutor) resolveFolder(ctx context.Context, ref string) (*string, error) {
	if ref == "" {
		return nil, nil
	}
	id, err := e.requireInModel(ctx, "dashboard_folder", ref)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// updateDashboard is PATCH /api/developer/dashboards/{id}: rename and/or move
// to a folder ("folder": "" or null = the top level). Tags change through
// set_tags.
func (e *WriteExecutor) updateDashboard(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		DashboardID string  `json:"dashboard_id"`
		Name        string  `json:"name"`
		Folder      *string `json:"folder"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.DashboardID == "" {
		return "", "", fmt.Errorf("dashboard_id is required")
	}
	has := present(raw)
	if p.Name == "" && !has["folder"] {
		return "", "", fmt.Errorf("nothing to change: provide name and/or folder")
	}
	dashID, err := e.requireInModel(ctx, "dashboard", p.DashboardID)
	if err != nil {
		return "", "", err
	}
	var changed []string
	if p.Name != "" {
		if _, err := e.pool.Exec(ctx, `UPDATE model.dashboard_def SET name=$2 WHERE id=$1::uuid`, dashID, p.Name); err != nil {
			return "", "", fmt.Errorf("rename dashboard: %w", err)
		}
		changed = append(changed, fmt.Sprintf("renamed to '%s'", p.Name))
	}
	if has["folder"] {
		ref := ""
		if p.Folder != nil {
			ref = *p.Folder
		}
		folderID, err := e.resolveFolder(ctx, ref)
		if err != nil {
			return "", "", err
		}
		if _, err := e.pool.Exec(ctx, `UPDATE model.dashboard_def SET folder_id=$2::uuid WHERE id=$1::uuid`, dashID, folderID); err != nil {
			return "", "", fmt.Errorf("move dashboard: %w", err)
		}
		if folderID == nil {
			changed = append(changed, "moved to the top level")
		} else {
			changed = append(changed, "moved to folder "+ref)
		}
	}
	return "Dashboard " + strings.Join(changed, "; "), dashID, nil
}

// deleteDashboard is DELETE /api/developer/dashboards/{id}.
func (e *WriteExecutor) deleteDashboard(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		DashboardID string `json:"dashboard_id"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.DashboardID == "" {
		return "", "", fmt.Errorf("dashboard_id is required")
	}
	dashID, err := e.requireInModel(ctx, "dashboard", p.DashboardID)
	if err != nil {
		return "", "", err
	}
	var name string
	_ = e.pool.QueryRow(ctx, `SELECT name FROM model.dashboard_def WHERE id=$1::uuid`, dashID).Scan(&name)
	if _, err := e.pool.Exec(ctx, `DELETE FROM model.dashboard_def WHERE id=$1::uuid`, dashID); err != nil {
		return "", "", fmt.Errorf("delete dashboard: %w", err)
	}
	return fmt.Sprintf("Dashboard '%s' deleted", name), "", nil
}

// resolveWidgetRef checks and resolves a widget's ref_id the way the
// developer endpoint's validateWidgetRef does for every widget type that
// references something: the thing must be in this model (and is resolved
// into the working revision); an automation button's rule must belong to
// this application.
func (e *WriteExecutor) resolveWidgetRef(ctx context.Context, widgetType string, ref *string) (*string, error) {
	if ref == nil || *ref == "" {
		return ref, nil
	}
	if widgetType == "automation_button" {
		appID, err := e.applicationID(ctx)
		if err != nil {
			return nil, err
		}
		var ruleAppID string
		if err := e.pool.QueryRow(ctx, `SELECT application_id::text FROM workflow.automation_rule WHERE id=$1::uuid`, *ref).Scan(&ruleAppID); err != nil {
			return nil, fmt.Errorf("automation rule %s not found (call list_automation_rules)", *ref)
		}
		if ruleAppID != appID {
			return nil, fmt.Errorf("automation rule %s belongs to a different application", *ref)
		}
		return ref, nil
	}
	kind, ok := widgetRefKind[widgetType]
	if !ok {
		return ref, nil
	}
	id, err := e.requireInModel(ctx, kind, *ref)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// widgetRefKind is what each widget type's ref_id points at — the types the
// developer endpoint's widgetRefOwnerSQL checks.
var widgetRefKind = map[string]string{
	"metric_kpi":         "metric",
	"chart":              "grid",
	"grid":               "grid",
	"import":             "grid",
	"form":               "form",
	"integration_button": "integration",
}

// validateWidgetContent is the developer endpoint's check on an image
// widget's content: a data URL of a real image within the size limit.
func validateWidgetContent(widgetType string, content *string) error {
	if widgetType != "image" || content == nil {
		return nil
	}
	return imagedata.Validate("the image", *content, imagedata.MaxWidgetBytes)
}

// updateDashboardWidget is PATCH /api/developer/dashboards/{id}/widgets/{w}:
// only the fields the step carries change.
func (e *WriteExecutor) updateDashboardWidget(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		WidgetID    string          `json:"widget_id"`
		RefID       *string         `json:"ref_id"`
		Content     *string         `json:"content"`
		Title       *string         `json:"title"`
		ShowTitle   *bool           `json:"show_title"`
		WidgetProps json.RawMessage `json:"widget_props"`
		PosX        *int            `json:"pos_x"`
		PosY        *int            `json:"pos_y"`
		SizeW       *int            `json:"size_w"`
		SizeH       *int            `json:"size_h"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.WidgetID == "" {
		return "", "", fmt.Errorf("widget_id is required (list_dashboards shows each widget's id)")
	}
	widgetID, err := e.requireInModel(ctx, "dashboard_widget", p.WidgetID)
	if err != nil {
		return "", "", err
	}
	var widgetType string
	_ = e.pool.QueryRow(ctx, `SELECT widget_type FROM model.dashboard_widget WHERE id=$1::uuid`, widgetID).Scan(&widgetType)
	if p.RefID, err = e.resolveWidgetRef(ctx, widgetType, p.RefID); err != nil {
		return "", "", err
	}
	if err := validateWidgetContent(widgetType, p.Content); err != nil {
		return "", "", err
	}
	var props *string
	if len(p.WidgetProps) > 0 && string(p.WidgetProps) != "null" {
		// Merged into what the widget has, not put in its place: live, the
		// assistant sent {"hide_rollup_members": true} to two charts and
		// replacing the props whole would have wiped both charts.
		var stored []byte
		_ = e.pool.QueryRow(ctx, `SELECT COALESCE(widget_props,'{}'::jsonb) FROM model.dashboard_widget WHERE id=$1::uuid`, widgetID).Scan(&stored)
		// A scope given without a mode pins the tile, whatever mode it had.
		var given map[string]any
		if json.Unmarshal(p.WidgetProps, &given) == nil && given["kpi_scope"] != nil && given["kpi_context_mode"] == nil {
			given["kpi_context_mode"] = "pin"
			p.WidgetProps, _ = json.Marshal(given)
		}
		merged, mErr := mergeWidgetProps(widgetType, stored, p.WidgetProps)
		if mErr != nil {
			return "", "", mErr
		}
		if merged, mErr = e.checkWidgetProps(ctx, merged); mErr != nil {
			return "", "", mErr
		}
		p.WidgetProps = merged
		if widgetType == "chart" {
			remapped, err := e.remapChartProps(ctx, p.WidgetProps)
			if err != nil {
				return "", "", err
			}
			p.WidgetProps = remapped
		}
		if widgetType == "grid" {
			remapped, err := e.remapGridWidgetMetrics(ctx, p.WidgetProps)
			if err != nil {
				return "", "", err
			}
			p.WidgetProps = remapped
		}
		s := string(p.WidgetProps)
		props = &s
	}
	if (widgetType == "chart" || widgetType == "grid") && (p.RefID != nil || props != nil) {
		// The chart as it will be: the new grid or props, else the stored ones.
		var refID string
		var stored []byte
		_ = e.pool.QueryRow(ctx, `SELECT COALESCE(ref_id::text,''), COALESCE(widget_props,'null'::jsonb) FROM model.dashboard_widget WHERE id=$1::uuid`, widgetID).Scan(&refID, &stored)
		if p.RefID != nil && *p.RefID != "" {
			refID = *p.RefID
		}
		if props != nil {
			stored = []byte(*props)
		}
		if err := modeledit.CheckWidgetProps(ctx, e.pool, widgetType, refID, stored); err != nil {
			return "", "", err
		}
	}
	for _, v := range []*int{p.SizeW, p.SizeH} {
		if v != nil && *v < 20 {
			*v = 20
		}
	}
	if _, err := e.pool.Exec(ctx, `
		UPDATE model.dashboard_widget
		SET ref_id=COALESCE($2, ref_id), content=COALESCE($3, content),
		    pos_x=COALESCE($4, pos_x), pos_y=COALESCE($5, pos_y), size_w=COALESCE($6, size_w), size_h=COALESCE($7, size_h),
		    title=COALESCE($8, title), show_title=COALESCE($9, show_title),
		    widget_props=COALESCE($10::jsonb, widget_props)
		WHERE id=$1::uuid`,
		widgetID, p.RefID, p.Content, p.PosX, p.PosY, p.SizeW, p.SizeH, p.Title, p.ShowTitle, props); err != nil {
		return "", "", fmt.Errorf("update widget: %w", err)
	}
	return fmt.Sprintf("Widget '%s' updated", widgetType), widgetID, nil
}

// deleteDashboardWidget is DELETE /api/developer/dashboards/{id}/widgets/{w}.
func (e *WriteExecutor) deleteDashboardWidget(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		WidgetID string `json:"widget_id"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.WidgetID == "" {
		return "", "", fmt.Errorf("widget_id is required (list_dashboards shows each widget's id)")
	}
	widgetID, err := e.requireInModel(ctx, "dashboard_widget", p.WidgetID)
	if err != nil {
		return "", "", err
	}
	var widgetType string
	_ = e.pool.QueryRow(ctx, `SELECT widget_type FROM model.dashboard_widget WHERE id=$1::uuid`, widgetID).Scan(&widgetType)
	if _, err := e.pool.Exec(ctx, `DELETE FROM model.dashboard_widget WHERE id=$1::uuid`, widgetID); err != nil {
		return "", "", fmt.Errorf("delete widget: %w", err)
	}
	return fmt.Sprintf("Widget '%s' deleted", widgetType), "", nil
}

// folderCycle reports whether making parentID the parent of folderID would
// put the folder inside itself.
func (e *WriteExecutor) folderCycle(ctx context.Context, folderID, parentID string) bool {
	cur := parentID
	for i := 0; i < 50 && cur != ""; i++ {
		if cur == folderID {
			return true
		}
		var up *string
		if err := e.pool.QueryRow(ctx, `SELECT parent_id::text FROM model.dashboard_folder WHERE id=$1::uuid`, cur).Scan(&up); err != nil || up == nil {
			return false
		}
		cur = *up
	}
	return false
}

// createDashboardFolder is POST /api/developer/folders.
func (e *WriteExecutor) createDashboardFolder(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Name   string `json:"name"`
		Parent string `json:"parent"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || strings.TrimSpace(p.Name) == "" {
		return "", "", fmt.Errorf("name is required")
	}
	parentID, err := e.resolveFolder(ctx, p.Parent)
	if err != nil {
		return "", "", err
	}
	var id string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_folder (model_id, name, parent_id, revision_id)
		VALUES ($1::uuid, $2, $3::uuid, NULLIF($4,'')::uuid) RETURNING id::text`,
		e.modelID, p.Name, parentID, e.revID).Scan(&id); err != nil {
		return "", "", fmt.Errorf("create folder: %w", err)
	}
	return fmt.Sprintf("Dashboard folder '%s' created (id: %s)", p.Name, id), id, nil
}

// updateDashboardFolder is PATCH /api/developer/folders/{id}: rename and/or
// move under another folder ("parent": "" or null = the top level).
func (e *WriteExecutor) updateDashboardFolder(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Folder string  `json:"folder"`
		Name   string  `json:"name"`
		Parent *string `json:"parent"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.Folder == "" {
		return "", "", fmt.Errorf("folder (its id or exact name) is required")
	}
	has := present(raw)
	if p.Name == "" && !has["parent"] {
		return "", "", fmt.Errorf("nothing to change: provide name and/or parent")
	}
	folderID, err := e.requireInModel(ctx, "dashboard_folder", p.Folder)
	if err != nil {
		return "", "", err
	}
	var changed []string
	if p.Name != "" {
		if _, err := e.pool.Exec(ctx, `UPDATE model.dashboard_folder SET name=$2 WHERE id=$1::uuid`, folderID, p.Name); err != nil {
			return "", "", fmt.Errorf("rename folder: %w", err)
		}
		changed = append(changed, fmt.Sprintf("renamed to '%s'", p.Name))
	}
	if has["parent"] {
		ref := ""
		if p.Parent != nil {
			ref = *p.Parent
		}
		parentID, err := e.resolveFolder(ctx, ref)
		if err != nil {
			return "", "", err
		}
		if parentID != nil && e.folderCycle(ctx, folderID, *parentID) {
			return "", "", fmt.Errorf("a folder cannot be moved inside itself")
		}
		if _, err := e.pool.Exec(ctx, `UPDATE model.dashboard_folder SET parent_id=$2::uuid WHERE id=$1::uuid`, folderID, parentID); err != nil {
			return "", "", fmt.Errorf("move folder: %w", err)
		}
		if parentID == nil {
			changed = append(changed, "moved to the top level")
		} else {
			changed = append(changed, "moved under "+ref)
		}
	}
	return "Dashboard folder " + strings.Join(changed, "; "), folderID, nil
}

// deleteDashboardFolder is DELETE /api/developer/folders/{id}: its subfolders
// go with it; its dashboards stay and move to the top level.
func (e *WriteExecutor) deleteDashboardFolder(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Folder string `json:"folder"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.Folder == "" {
		return "", "", fmt.Errorf("folder (its id or exact name) is required")
	}
	folderID, err := e.requireInModel(ctx, "dashboard_folder", p.Folder)
	if err != nil {
		return "", "", err
	}
	var name string
	_ = e.pool.QueryRow(ctx, `SELECT name FROM model.dashboard_folder WHERE id=$1::uuid`, folderID).Scan(&name)
	if _, err := e.pool.Exec(ctx, `DELETE FROM model.dashboard_folder WHERE id=$1::uuid`, folderID); err != nil {
		return "", "", fmt.Errorf("delete folder: %w", err)
	}
	return fmt.Sprintf("Dashboard folder '%s' deleted (its subfolders too; its dashboards moved to the top level)", name), "", nil
}

// ── workflow lifecycle ────────────────────────────────────────────────────────

func (e *WriteExecutor) workflowLifecycleRef(ctx context.Context, raw json.RawMessage) (id, name string, err error) {
	var p struct {
		WorkflowDefID string `json:"workflow_def_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.WorkflowDefID == "" {
		return "", "", fmt.Errorf("workflow_def_id (its id or exact name) is required")
	}
	if e.userID == "" {
		return "", "", fmt.Errorf("workflow lifecycle changes need the acting developer")
	}
	if id, err = resolveWorkflowDefRef(ctx, e.pool, e.modelID, e.revID, p.WorkflowDefID); err != nil {
		return "", "", err
	}
	_ = e.pool.QueryRow(ctx, `SELECT name FROM workflow.workflow_def WHERE id=$1::uuid`, id).Scan(&name)
	return id, name, nil
}

// archiveWorkflowDef is POST /api/developer/workflows/{id}/archive: a
// workflow that has run is archived rather than deleted; it stops starting.
func (e *WriteExecutor) archiveWorkflowDef(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var keys struct {
		WorkflowDefID string `json:"workflow_def_id"`
	}
	if err := decodeParams(raw, &keys); isUnknownParam(err) {
		return "", "", err
	}
	id, name, err := e.workflowLifecycleRef(ctx, raw)
	if err != nil {
		return "", "", err
	}
	if _, err := workflow.NewStoreOn(e.pool).ArchiveWorkflowDef(ctx, id, e.userID); err != nil {
		return "", "", err
	}
	return fmt.Sprintf("Workflow '%s' archived", name), id, nil
}

// restoreWorkflowDef is POST /api/developer/workflows/{id}/restore: an
// archived workflow goes back to draft, to be published again by a developer.
func (e *WriteExecutor) restoreWorkflowDef(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var keys struct {
		WorkflowDefID string `json:"workflow_def_id"`
	}
	if err := decodeParams(raw, &keys); isUnknownParam(err) {
		return "", "", err
	}
	id, name, err := e.workflowLifecycleRef(ctx, raw)
	if err != nil {
		return "", "", err
	}
	if _, err := workflow.NewStoreOn(e.pool).RestoreWorkflowDef(ctx, id, e.userID); err != nil {
		return "", "", err
	}
	return fmt.Sprintf("Workflow '%s' restored as a draft — a developer publishes it from the Workflows tab", name), id, nil
}

// duplicateWorkflowDef is POST /api/developer/workflows/{id}/duplicate: a
// draft copy under a new name.
func (e *WriteExecutor) duplicateWorkflowDef(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		WorkflowDefID string `json:"workflow_def_id"`
		Name          string `json:"name"`
	}
	if err := decodeParams(raw, &p); isUnknownParam(err) {
		return "", "", err
	}
	id, name, err := e.workflowLifecycleRef(ctx, raw)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(p.Name) == "" {
		return "", "", fmt.Errorf("name (for the copy) is required")
	}
	dup, err := workflow.NewStoreOn(e.pool).DuplicateWorkflowDef(ctx, id, p.Name, e.userID)
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("Workflow '%s' duplicated as draft '%s' (id: %s)", name, p.Name, dup.ID), dup.ID, nil
}

// ── business roles (live, like the console) ───────────────────────────────────

// resolveBusinessRole finds a business role of this application's workspace
// by id or name (case-insensitive), as the console's role routes are scoped
// to the caller's workspace.
func (e *WriteExecutor) resolveBusinessRole(ctx context.Context, ref string) (id, name, wsID string, err error) {
	if wsID, err = e.workspaceID(ctx); err != nil {
		return "", "", "", err
	}
	q := `SELECT id::text, name FROM identity.business_role WHERE workspace_id=$1::uuid AND lower(name)=lower($2)`
	if uuidShaped(ref) {
		q = `SELECT id::text, name FROM identity.business_role WHERE workspace_id=$1::uuid AND id=$2::uuid`
	}
	if err := e.pool.QueryRow(ctx, q, wsID, ref).Scan(&id, &name); err != nil {
		return "", "", "", fmt.Errorf("business role %q not found in this workspace (call list_workflow_roles)", ref)
	}
	return id, name, wsID, nil
}

// roleUseNote names the workflows of this application whose steps name the
// role, since assignee_roles and recipient_role match roles by name.
func (e *WriteExecutor) roleUseNote(ctx context.Context, roleName, verb string) string {
	rows, err := e.pool.Query(ctx, `
		SELECT DISTINCT wd.name FROM workflow.workflow_def wd
		JOIN core.model m ON m.application_id = wd.application_id
		WHERE m.id=$1::uuid AND wd.steps::text LIKE '%' || to_json($2::text)::text || '%'
		ORDER BY wd.name`, e.modelID, roleName)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf(" — steps of workflow(s) %s name '%s' and no longer match a role now that it is %s; update them with update_workflow_def",
		strings.Join(names, ", "), roleName, verb)
}

// updateBusinessRole is PATCH /api/business-admin/roles/{id}: a rename.
func (e *WriteExecutor) updateBusinessRole(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Role string `json:"role"`
		Name string `json:"name"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.Role == "" || strings.TrimSpace(p.Name) == "" {
		return "", "", fmt.Errorf("role (its name or id) and name are required")
	}
	id, old, _, err := e.resolveBusinessRole(ctx, p.Role)
	if err != nil {
		return "", "", err
	}
	name := strings.TrimSpace(p.Name)
	if _, err := e.pool.Exec(ctx, `UPDATE identity.business_role SET name=$2 WHERE id=$1::uuid`, id, name); err != nil {
		if metricformula.IsUniqueViolation(err) {
			return "", "", fmt.Errorf("a business role named '%s' already exists", name)
		}
		return "", "", fmt.Errorf("rename business role: %w", err)
	}
	return fmt.Sprintf("Business role '%s' renamed to '%s'%s", old, name, e.roleUseNote(ctx, old, "renamed")), id, nil
}

// deleteBusinessRole is DELETE /api/business-admin/roles/{id}: the role, its
// members and its dashboard grants.
func (e *WriteExecutor) deleteBusinessRole(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Role string `json:"role"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.Role == "" {
		return "", "", fmt.Errorf("role (its name or id) is required")
	}
	id, name, _, err := e.resolveBusinessRole(ctx, p.Role)
	if err != nil {
		return "", "", err
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM identity.business_role WHERE id=$1::uuid`, id); err != nil {
		return "", "", fmt.Errorf("delete business role: %w", err)
	}
	return fmt.Sprintf("Business role '%s' deleted%s", name, e.roleUseNote(ctx, name, "deleted")), "", nil
}

// setRoleDashboards is PUT /api/business-admin/roles/{id}/dashboards, for the
// dashboards of the working revision: it replaces which of them the role's
// members may open. Like every model change of a session, the grants on the
// draft's dashboards take effect when the draft is promoted; grants on other
// revisions' dashboards are left alone.
func (e *WriteExecutor) setRoleDashboards(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Role       string   `json:"role"`
		Dashboards []string `json:"dashboards"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.Role == "" || p.Dashboards == nil {
		return "", "", fmt.Errorf("role (its name or id) and dashboards (ids or exact names; [] for none) are required")
	}
	roleID, roleName, _, err := e.resolveBusinessRole(ctx, p.Role)
	if err != nil {
		return "", "", err
	}
	ids := make([]string, 0, len(p.Dashboards))
	for _, ref := range p.Dashboards {
		id, err := e.requireInModel(ctx, "dashboard", ref)
		if err != nil {
			return "", "", err
		}
		ids = append(ids, id)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err := tx.Exec(ctx, `
		DELETE FROM identity.business_role_dashboard brd
		USING model.dashboard_def d
		WHERE brd.role_id=$1::uuid AND d.id = brd.dashboard_id
		  AND d.model_id=$2::uuid AND d.revision_id IS NOT DISTINCT FROM NULLIF($3,'')::uuid`,
		roleID, e.modelID, e.revID); err != nil {
		return "", "", fmt.Errorf("clear dashboard grants: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `INSERT INTO identity.business_role_dashboard (role_id, dashboard_id) VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`,
			roleID, id); err != nil {
			return "", "", fmt.Errorf("grant dashboard: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return fmt.Sprintf("Business role '%s' may open %d dashboard(s) of this revision", roleName, len(ids)), roleID, nil
}

// ── form integrations ─────────────────────────────────────────────────────────

// backfillFormIntegration is POST /api/developer/form-integrations/{id}/backfill:
// every record of the form in a posting status is posted into the metric,
// through the gateway's own posting code.
func (e *WriteExecutor) backfillFormIntegration(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		FormIntegrationID string `json:"form_integration_id"`
	}
	perr := decodeParams(raw, &p)
	if isUnknownParam(perr) {
		return "", "", perr
	}
	if perr != nil || p.FormIntegrationID == "" {
		return "", "", fmt.Errorf("form_integration_id is required")
	}
	if _, _, err := e.loadIntegration(ctx, p.FormIntegrationID); err != nil {
		return "", "", err
	}
	if e.hooks.PostFormIntegration == nil {
		return "", "", fmt.Errorf("posting form records is not available here")
	}
	n, err := e.hooks.PostFormIntegration(ctx, p.FormIntegrationID)
	if err != nil {
		return "", "", fmt.Errorf("backfill: %w", err)
	}
	return fmt.Sprintf("Posted %d form record(s) into the integration's metric", n), "", nil
}

// mergeWidgetProps lays the given props over the stored ones: a key given
// replaces the stored value, a null removes it, anything not given stays;
// "chart" merges the same way one level down, and a chart setting given at
// the top level of a chart widget's props goes under "chart"; a grid
// widget's "metrics" is its metric_ids.
func mergeWidgetProps(widgetType string, stored, given json.RawMessage) (json.RawMessage, error) {
	base := map[string]any{}
	if len(stored) > 0 && string(stored) != "null" {
		_ = json.Unmarshal(stored, &base)
	}
	var patch map[string]any
	if err := json.Unmarshal(given, &patch); err != nil {
		return nil, fmt.Errorf("widget_props is not a JSON object: %w", err)
	}
	if v, ok := patch["metrics"]; ok && widgetType == "grid" {
		// The list a grid widget shows goes by metric_ids; "metrics" (the
		// name create_grid reads it by) would otherwise sit beside a stored
		// metric_ids and lose to it.
		if _, both := patch["metric_ids"]; !both {
			patch["metric_ids"] = v
		}
		delete(patch, "metrics")
	}
	if widgetType == "chart" {
		for k, v := range patch {
			if modeledit.ChartSettingKeys[k] {
				c, _ := patch["chart"].(map[string]any)
				if c == nil {
					c = map[string]any{}
				}
				c[k] = v
				patch["chart"] = c
				delete(patch, k)
			}
		}
	}
	for k, v := range patch {
		switch {
		case v == nil:
			delete(base, k)
		case k == "chart":
			pc, ok := v.(map[string]any)
			bc, okBase := base["chart"].(map[string]any)
			if !ok || !okBase {
				base[k] = v
				continue
			}
			for ck, cv := range pc {
				if cv == nil {
					delete(bc, ck)
				} else {
					bc[ck] = cv
				}
			}
		default:
			base[k] = v
		}
	}
	return json.Marshal(base)
}

// checkWidgetProps refuses widget_props the console would not read — an
// unknown key, at the top or under "chart"; a kpi_scope that is not one
// member of one dimension; a scope with a mode other than "pin" — and
// resolves the scope's dimension by id or name.
func (e *WriteExecutor) checkWidgetProps(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return raw, nil
	}
	var props map[string]any
	if err := json.Unmarshal(raw, &props); err != nil {
		return nil, fmt.Errorf("widget_props is not a JSON object: %w", err)
	}
	sortedKeys := func(m map[string]bool) string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return strings.Join(keys, ", ")
	}
	if v, ok := props["metrics"]; ok {
		// A grid widget's list, under the name create_grid reads it by.
		if _, both := props["metric_ids"]; !both {
			props["metric_ids"] = v
		}
		delete(props, "metrics")
	}
	for k := range props {
		if !modeledit.WidgetPropKeys[k] {
			return nil, fmt.Errorf("widget_props has no key %q: its keys are %s", k, sortedKeys(modeledit.WidgetPropKeys))
		}
	}
	if chart, ok := props["chart"].(map[string]any); ok {
		for k := range chart {
			if !modeledit.ChartSettingKeys[k] {
				return nil, fmt.Errorf("a chart's settings have no key %q: they are %s", k, sortedKeys(modeledit.ChartSettingKeys))
			}
		}
	}
	mode, _ := props["kpi_context_mode"].(string)
	if v, ok := props["kpi_context_mode"]; ok && v != nil && mode != "total" && mode != "sync" && mode != "pin" {
		return nil, fmt.Errorf(`kpi_context_mode is "total" (the whole model), "sync" (the dashboard's selectors) or "pin" (with kpi_scope)`)
	}
	if v, ok := props["kpi_scope"]; ok && v != nil {
		const shape = `kpi_scope is {"dimension_id": "<dimension id or name>", "member_code": "<member code>"} — one member of one dimension — with "kpi_context_mode": "pin"`
		scope, ok := v.(map[string]any)
		dim, _ := scope["dimension_id"].(string)
		code, _ := scope["member_code"].(string)
		if !ok || len(scope) != 2 || dim == "" || code == "" {
			return nil, fmt.Errorf("%s", shape)
		}
		if mode == "" {
			props["kpi_context_mode"] = "pin"
		} else if mode != "pin" {
			return nil, fmt.Errorf("kpi_context_mode %q contradicts kpi_scope: %s", mode, shape)
		}
		id, err := e.requireInModel(ctx, "dimension", dim)
		if err != nil {
			return nil, fmt.Errorf("kpi_scope: %w", err)
		}
		scope["dimension_id"] = id
	}
	if err := e.resolveWidgetLayout(ctx, props); err != nil {
		return nil, err
	}
	return json.Marshal(props)
}

// resolveWidgetLayout resolves the dimensions a grid widget's default_view
// and a chart's context_defaults name — by name or id, as the console stores
// them by id — and refuses a default_view key the grid does not read.
// Without it a layout naming "Region" saved and the grid opened in its
// default layout.
func (e *WriteExecutor) resolveWidgetLayout(ctx context.Context, props map[string]any) error {
	dimID := func(where, ref string) (string, error) {
		id, err := e.requireInModel(ctx, "dimension", ref)
		if err != nil {
			return "", fmt.Errorf("%s: %w", where, err)
		}
		return id, nil
	}
	if v, ok := props["default_view"]; ok && v != nil {
		dv, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf(`default_view is {"rows": [...], "cols": [...], "context": [...], "filter_sel": {"<dimension>": "<member code>"}}`)
		}
		for k := range dv {
			switch k {
			case "rows", "cols", "context", "filter_sel":
			default:
				return fmt.Errorf("default_view has no key %q: its keys are rows, cols, context and filter_sel", k)
			}
		}
		for _, zone := range []string{"rows", "cols", "context"} {
			list, _ := dv[zone].([]any)
			for i, item := range list {
				ref, _ := item.(string)
				if ref == "" || ref == "__metrics__" {
					continue
				}
				if strings.EqualFold(ref, "metrics") {
					list[i] = "__metrics__"
					continue
				}
				id, err := dimID("default_view."+zone, ref)
				if err != nil {
					return err
				}
				list[i] = id
			}
		}
		if fs, ok := dv["filter_sel"].(map[string]any); ok {
			out := make(map[string]any, len(fs))
			for k, code := range fs {
				if k == "__metrics__" {
					out[k] = code
					continue
				}
				id, err := dimID("default_view.filter_sel", k)
				if err != nil {
					return err
				}
				out[id] = code
			}
			dv["filter_sel"] = out
		}
	}
	if v, ok := props["show_members"]; ok && v != nil {
		const shape = `show_members is {"<dimension>": ["<member code>", ...]} — the members a grid widget's rows or columns show, in that order`
		sm, ok := v.(map[string]any)
		if !ok {
			return errors.New(shape)
		}
		out := make(map[string]any, len(sm))
		for k, raw := range sm {
			id, err := dimID("show_members", k)
			if err != nil {
				return err
			}
			list, ok := raw.([]any)
			if !ok || len(list) == 0 {
				return errors.New(shape)
			}
			codes := make([]string, 0, len(list))
			for _, item := range list {
				code, _ := item.(string)
				var found bool
				_ = e.pool.QueryRow(ctx, `SELECT true FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, id, code).Scan(&found)
				if !found {
					return fmt.Errorf("show_members: %q is not a member code of %s (call list_dimensions to see codes)", code, k)
				}
				codes = append(codes, code)
			}
			out[id] = codes
		}
		props["show_members"] = out
	}
	if chart, ok := props["chart"].(map[string]any); ok {
		if cd, ok := chart["context_defaults"].(map[string]any); ok {
			out := make(map[string]any, len(cd))
			for k, code := range cd {
				id, err := dimID("chart.context_defaults", k)
				if err != nil {
					return err
				}
				out[id] = code
			}
			chart["context_defaults"] = out
		}
	}
	return nil
}
