package gateway

import (
	"net/http"
	"time"
)

// dormancyRow is one dedicated tenant's use as retiring unused workspaces
// sees it (internal/dormancy).
type dormancyRow struct {
	CustomerID string     `json:"customer_id"`
	LastUsedAt time.Time  `json:"last_used_at"`
	WarnedAt   *time.Time `json:"warned_at,omitempty"`
	// WarnOn is when its administrators will be told, while not yet told.
	WarnOn *time.Time `json:"warn_on,omitempty"`
	// DeleteOn is when it will be deleted, once they have been told.
	DeleteOn *time.Time `json:"delete_on,omitempty"`
}

// adminDormancy lists, in the hosted Playground, every dedicated tenant's last
// use and, where its edition retires unused workspaces, when it will be told
// or deleted — so the platform administrator sees it coming, and can move a
// tenant to an edition that keeps it. Elsewhere the list is empty.
func (h *handler) adminDormancy(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireRole(w, r, "platform_admin"); !ok {
		return
	}
	out := []dormancyRow{}
	if h.db.Router() == nil || !h.signupCfg.Playground {
		jsonOK(w, out)
		return
	}
	rows, err := h.db.Control().Query(r.Context(), `
		SELECT t.customer_id::text, COALESCE(t.last_used_at, t.created_at), t.dormancy_warned_at,
		       CASE WHEN p.dormant_warn_days > 0 AND t.dormancy_warned_at IS NULL
		            THEN COALESCE(t.last_used_at, t.created_at) + make_interval(days => p.dormant_warn_days) END,
		       CASE WHEN p.dormant_warn_days > 0 AND t.dormancy_warned_at IS NOT NULL
		            THEN GREATEST(COALESCE(t.last_used_at, t.created_at) + make_interval(days => p.dormant_delete_days),
		                          t.dormancy_warned_at + make_interval(days => p.dormant_delete_days - p.dormant_warn_days)) END
		FROM platform.tenant_database t LEFT JOIN platform.plan p ON p.key = t.plan
		WHERE t.status = 'ready'
		ORDER BY 2`)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var d dormancyRow
		if err := rows.Scan(&d.CustomerID, &d.LastUsedAt, &d.WarnedAt, &d.WarnOn, &d.DeleteOn); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		out = append(out, d)
	}
	jsonOK(w, out)
}
