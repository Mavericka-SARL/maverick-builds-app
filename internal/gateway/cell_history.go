package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/mavericks-engine/mavericks/ee/cellhistory"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// Per-cell change history (enterprise). Read-only, for anyone who could see
// the cell in the grid: the same model, revision, metric and hidden-member
// rules the cell write applies, so history never shows a value the grid
// would not. The query lives in ee/cellhistory.
func (h *handler) cellHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	modelID, revisionID, metricID := q.Get("model_id"), q.Get("revision_id"), q.Get("metric_id")
	if modelID == "" || metricID == "" {
		jsonErr(w, fmt.Errorf("model_id and metric_id are required"), http.StatusBadRequest)
		return
	}
	if canAccess, caErr := h.actorCanAccessModel(ctx, a, modelID); caErr != nil {
		jsonErr(w, caErr, http.StatusInternalServerError)
		return
	} else if !canAccess {
		jsonErr(w, fmt.Errorf("forbidden: model is outside your access scope"), http.StatusForbidden)
		return
	}
	if revisionID == "" {
		_ = h.db.QueryRow(ctx,
			`SELECT COALESCE(active_revision_id::text, (SELECT id::text FROM model.revision WHERE model_id=$1::uuid ORDER BY created_at DESC LIMIT 1)) FROM core.model WHERE id=$1::uuid`,
			modelID).Scan(&revisionID)
	}
	var revisionBelongs bool
	if err := h.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model.revision WHERE id=$1::uuid AND model_id=$2::uuid)`, revisionID, modelID).Scan(&revisionBelongs); err != nil || !revisionBelongs {
		jsonErr(w, fmt.Errorf("revision is outside this model"), http.StatusForbidden)
		return
	}
	var isInput bool
	if err := h.db.QueryRow(ctx, `SELECT is_input FROM model.metric_def WHERE id=$1::uuid AND model_id=$2::uuid`, metricID, modelID).Scan(&isInput); err != nil {
		jsonErr(w, fmt.Errorf("metric not found"), http.StatusNotFound)
		return
	} else if !isInput {
		jsonErr(w, fmt.Errorf("only input metrics have a change history; a calculated value has no entries of its own"), http.StatusBadRequest)
		return
	}
	if access, err := writeguard.MetricAccess(ctx, h.db.For(ctx), a.UserID, metricID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	} else if access == "hidden" {
		jsonErr(w, fmt.Errorf("access denied"), http.StatusForbidden)
		return
	}

	dimCodes := map[string]string{}
	if raw := q.Get("dim_codes"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &dimCodes); err != nil {
			jsonErr(w, fmt.Errorf("dim_codes must be a JSON object of dimension id to member code"), http.StatusBadRequest)
			return
		}
	}
	for dimID, code := range dimCodes {
		var memberID string
		if err := h.db.QueryRow(ctx, `
			SELECT m.id::text FROM model.dimension_member m
			JOIN model.dimension_def d ON d.id = m.dimension_id
			WHERE d.id = $1::uuid AND d.model_id = $2::uuid AND m.code = $3`, dimID, modelID, code).Scan(&memberID); err != nil {
			jsonErr(w, fmt.Errorf("member %s of dimension %s not found", code, dimID), http.StatusNotFound)
			return
		}
		if hidden, err := writeguard.HiddenInChain(ctx, h.db.For(ctx), a.UserID, memberID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		} else if hidden {
			jsonErr(w, fmt.Errorf("access denied"), http.StatusForbidden)
			return
		}
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	entries, err := cellhistory.History(ctx, h.db.For(ctx), modelID, revisionID, metricID, dimCodes, limit)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, entries)
}
