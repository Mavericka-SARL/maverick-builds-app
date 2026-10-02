package gateway

// Generic grid reads beside /api/grid: a catalog of the grids a caller can
// read, and one grid's values resolved along a dimension. Both are what a
// business user may already read through /api/grid — anyone who opens the
// grid's model, with members and metrics filtered by their access rules —
// so they widen nothing; they exist so a reader that is not the grid screen
// (a report, the chat connector in mcp.go) need not reach for the developer
// grid list or a dashboard widget it may not have.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/query"
)

// validateChartConfig checks what the chart resolver needs of a config:
// shared by the dashboard chart-data route and gridSeries.
func validateChartConfig(cfg *query.ChartConfig) error {
	if cfg.DimensionID == "" {
		return fmt.Errorf("chart dimension_id missing")
	}
	switch cfg.ChartType {
	case query.ChartBar, query.ChartLine:
		if len(cfg.MetricIDs) < 1 || len(cfg.MetricIDs) > 5 {
			return fmt.Errorf("bar/line charts require 1–5 metrics")
		}
	case query.ChartPie, query.ChartHistogram:
		if len(cfg.MetricIDs) != 1 {
			return fmt.Errorf("pie/histogram charts require exactly 1 metric")
		}
	case query.ChartScatter:
		if cfg.XMetricID == "" || cfg.YMetricID == "" {
			return fmt.Errorf("scatter charts require x_metric_id and y_metric_id")
		}
		if cfg.XMetricID == cfg.YMetricID {
			return fmt.Errorf("scatter X and Y metrics must be different")
		}
	default:
		return fmt.Errorf("unsupported chart type: %s", cfg.ChartType)
	}
	if cfg.BinCount != 0 && (cfg.BinCount < 3 || cfg.BinCount > 30) {
		return fmt.Errorf("bin_count must be between 3 and 30")
	}
	return nil
}

// gridCatalogItem is one readable grid. MetricCount counts the metrics the
// caller may see.
type gridCatalogItem struct {
	ID                 string              `json:"id"`
	Name               string              `json:"name"`
	RevisionID         string              `json:"revision_id"`
	Dimensions         []gridCatalogDimRef `json:"dimensions"`
	MetricCount        int                 `json:"metric_count"`
	RollupSourceGridID *string             `json:"rollup_source_grid_id,omitempty"`
}

type gridCatalogDimRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// gridCatalog serves GET /api/grids: the grids of the request's model
// (X-App-Id / X-Model-Id, as every business read) in a revision
// (?revision_id, else the active one). A grid whose every metric is hidden
// from the caller is left out — its name says nothing they may read — and a
// rollup grid counts its source grid's metrics, which /api/grid lists for it.
func (h *handler) gridCatalog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	revisionID, _, revErr := h.businessRevisionCtx(ctx, r, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, revErr) {
		return
	}
	if revErr != nil {
		jsonErr(w, fmt.Errorf("no revision: %w", revErr), http.StatusInternalServerError)
		return
	}
	_, metricRules, err := loadUserAccessRules(ctx, h.db, act.UserID, revisionID)
	if err != nil {
		jsonErr(w, fmt.Errorf("load access rules: %w", err), http.StatusInternalServerError)
		return
	}

	rows, err := h.db.Query(ctx, `
		SELECT g.id::text, g.name, COALESCE(g.revision_id::text, ''), g.rollup_source_grid_id::text,
		       COALESCE((SELECT array_agg(rev.id::text ORDER BY gm.sort_order)
		                 FROM model.grid_metric gm
		                 JOIN model.metric_def orig ON orig.id = gm.metric_id
		                 JOIN model.metric_def rev ON rev.model_id = orig.model_id AND rev.name = orig.name
		                                          AND rev.revision_id = $2::uuid
		                 WHERE gm.grid_id = COALESCE(g.rollup_source_grid_id, g.id)), '{}'),
		       COALESCE((SELECT json_agg(json_build_object('id', d.id::text, 'name', d.name) ORDER BY d.name)
		                 FROM model.grid_dimension gd JOIN model.dimension_def d ON d.id = gd.dimension_id
		                 WHERE gd.grid_id = g.id), '[]')
		FROM model.grid_def g
		WHERE g.model_id = $1::uuid AND (g.revision_id IS NULL OR g.revision_id = $2::uuid)
		ORDER BY g.created_at, g.id`, modelID, revisionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	grids := []gridCatalogItem{}
	for rows.Next() {
		var g gridCatalogItem
		var metricIDs []string
		var dimsJSON []byte
		if err := rows.Scan(&g.ID, &g.Name, &g.RevisionID, &g.RollupSourceGridID, &metricIDs, &dimsJSON); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		for _, id := range metricIDs {
			if metricRules[id] != "hidden" {
				g.MetricCount++
			}
		}
		if g.MetricCount == 0 {
			continue
		}
		if err := json.Unmarshal(dimsJSON, &g.Dimensions); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if g.RevisionID == "" {
			g.RevisionID = revisionID
		}
		grids = append(grids, g)
	}
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, grids)
}

// errGridNotFound answers a grid that does not exist and one outside the
// caller's reach alike.
var errGridNotFound = errors.New("grid not found")

// gridSeries serves GET /api/grid/series: one grid's metrics resolved along
// one of its dimensions — the chart resolver a dashboard chart uses
// (internal/query), without a dashboard. Values are the engine's own: inputs
// rolled up by their aggregation and time-summary rules, formulas evaluated
// at each member, withheld where they depend on what the caller may not see.
//
// Query: grid_def_id; revision_id (else the model's active revision);
// chart_type (bar — the default —, line, pie, scatter, histogram);
// dimension_id, the dimension resolved along; metric_ids, comma-separated
// (x_metric_id and y_metric_id for scatter); bin_count; hide_rollup_members=1
// to keep only leaf members; context, a JSON object of dimension id → member
// code fixing the grid's other dimensions.
//
// A context member is taken exactly as given: one that does not exist or is
// hidden from the caller is refused with the same answer, where a saved
// dashboard chart substitutes a visible member — a requested filter that
// silently became another would report the wrong slice.
func (h *handler) gridSeries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	gridDefID := q.Get("grid_def_id")
	if _, err := uuid.Parse(gridDefID); err != nil {
		jsonErr(w, errGridNotFound, http.StatusNotFound)
		return
	}
	var modelID, gridRevision string
	err = h.db.QueryRow(ctx,
		`SELECT model_id::text, COALESCE(revision_id::text, '') FROM model.grid_def WHERE id = $1::uuid`, gridDefID,
	).Scan(&modelID, &gridRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		jsonErr(w, errGridNotFound, http.StatusNotFound)
		return
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if ok, err := h.actorCanAccessModel(ctx, act, modelID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	} else if !ok {
		jsonErr(w, errGridNotFound, http.StatusNotFound)
		return
	}
	revisionID, _, revErr := h.businessRevisionCtx(ctx, r, q.Get("revision_id"), modelID)
	if rejectForeignRevision(w, revErr) {
		return
	}
	if revErr != nil {
		jsonErr(w, fmt.Errorf("no revision: %w", revErr), http.StatusInternalServerError)
		return
	}
	if gridRevision != "" && gridRevision != revisionID {
		jsonErr(w, fmt.Errorf("grid is not part of this revision"), http.StatusBadRequest)
		return
	}

	cfg := query.ChartConfig{
		ChartType:         query.ChartType(q.Get("chart_type")),
		DimensionID:       q.Get("dimension_id"),
		XMetricID:         q.Get("x_metric_id"),
		YMetricID:         q.Get("y_metric_id"),
		HideRollupMembers: q.Get("hide_rollup_members") == "1",
	}
	if cfg.ChartType == "" {
		cfg.ChartType = query.ChartBar
	}
	for _, id := range strings.Split(q.Get("metric_ids"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.MetricIDs = append(cfg.MetricIDs, id)
		}
	}
	if v := q.Get("bin_count"); v != "" {
		if cfg.BinCount, err = strconv.Atoi(v); err != nil {
			jsonErr(w, fmt.Errorf("bin_count must be a number"), http.StatusBadRequest)
			return
		}
	}
	if err := validateChartConfig(&cfg); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	requested := map[string]string{}
	if raw := q.Get("context"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &requested); err != nil {
			jsonErr(w, fmt.Errorf("context must be a JSON object of dimension id to member code"), http.StatusBadRequest)
			return
		}
	}

	result, err := query.NewChartResolver(h.db.For(ctx)).Resolve(ctx, &cfg, requested, modelID, revisionID, gridDefID, act.UserID)
	if err != nil {
		if msg := err.Error(); strings.Contains(msg, "context member") {
			jsonErr(w, errContextMemberUnavailable, http.StatusBadRequest)
			return
		}
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	// The resolver substitutes a visible member for a hidden one; here that
	// is a refusal, worded as for a member that does not exist.
	var resolved struct {
		Context map[string]string `json:"context"`
	}
	raw, err := json.Marshal(result)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	_ = json.Unmarshal(raw, &resolved)
	for dimID, code := range requested {
		if resolved.Context[dimID] != code {
			jsonErr(w, errContextMemberUnavailable, http.StatusBadRequest)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

var errContextMemberUnavailable = errors.New("a context member is not available: it does not exist in this grid's dimension or is not visible to you")
