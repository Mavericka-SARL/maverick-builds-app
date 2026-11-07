package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/modeledit"
	"github.com/mavericks-engine/mavericks/internal/timedim"
)

// A business admin extends the time window of the model they administer
// (owner decision, 2026-11-07): the Guide gives that to FP&A, and only a
// developer could add periods. Narrow on purpose: the time dimensions of the
// active revision, periods added after the last one only, placed as that
// one is (modeledit.ExtendCalendar). Nothing is renamed, moved or deleted.

// calendarRow is one time dimension as Business Admin › Calendar lists it.
type calendarRow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Granularity string `json:"granularity"`
	First       string `json:"first,omitempty"`
	Last        string `json:"last,omitempty"`
	LastLabel   string `json:"last_label,omitempty"`
	Periods     int    `json:"periods"`
}

// baCalendars handles GET /api/business-admin/calendars.
func (h *handler) baCalendars(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, modelID, err := h.baWorkspaceModel(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve workspace model")
		return
	}
	rows, err := h.db.Query(ctx, `
		SELECT d.id::text, d.name, COALESCE(d.time_granularity, ''),
		       COALESCE(min(m.period_start)::text, ''), COALESCE(max(m.period_end)::text, ''),
		       COALESCE((SELECT l.label FROM model.dimension_member l WHERE l.dimension_id = d.id
		                 ORDER BY l.period_end DESC NULLS LAST LIMIT 1), ''),
		       count(m.period_start)
		FROM core.model cm
		JOIN model.dimension_def d ON d.model_id = cm.id AND d.revision_id = cm.active_revision_id
		LEFT JOIN model.dimension_member m ON m.dimension_id = d.id
		WHERE cm.id = $1::uuid AND d.dimension_type::text = 'time'
		GROUP BY d.id, d.name, d.time_granularity
		ORDER BY d.name
	`, modelID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []calendarRow{}
	for rows.Next() {
		var c calendarRow
		if err := rows.Scan(&c.ID, &c.Name, &c.Granularity, &c.First, &c.Last, &c.LastLabel, &c.Periods); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, out)
}

// baCalendarExtend handles POST /api/business-admin/calendars/{dimId}/extend,
// body {"through": "YYYY-MM-DD"}.
func (h *handler) baCalendarExtend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dimID := r.PathValue("dimId")
	_, modelID, err := h.baWorkspaceModel(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve workspace model")
		return
	}
	var body struct {
		Through string `json:"through"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	through, err := timedim.ParseDate(body.Through)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if through.After(time.Now().AddDate(30, 0, 0)) {
		jsonErr(w, fmt.Errorf("a calendar runs at most 30 years ahead"), http.StatusBadRequest)
		return
	}
	// The dimension must be a time dimension of the active revision of the
	// model this admin has open: anything else answers as unknown.
	var ok bool
	if err := h.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM model.dimension_def d JOIN core.model cm ON cm.id = d.model_id
		               WHERE d.id::text = $1 AND cm.id = $2::uuid AND d.revision_id = cm.active_revision_id
		                 AND d.dimension_type::text = 'time')`, dimID, modelID).Scan(&ok); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !ok {
		jsonErr(w, fmt.Errorf("calendar not found"), http.StatusNotFound)
		return
	}
	tx, err := h.db.Begin(ctx)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	periods, aggregates, err := modeledit.ExtendCalendar(ctx, tx, dimID, through)
	var calErr *modeledit.CalendarError
	var timeErr *timedim.Error
	switch {
	case errors.As(err, &calErr), errors.As(err, &timeErr):
		jsonErr(w, err, http.StatusBadRequest)
		return
	case errors.Is(err, pgx.ErrNoRows):
		jsonErr(w, fmt.Errorf("calendar not found"), http.StatusNotFound)
		return
	case err != nil:
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if cid, _ := h.customerOfDimension(ctx, dimID); cid != "" && h.plans != nil {
		if err := h.plans.CheckMembers(ctx, h.db.For(ctx), cid, dimID, periods+aggregates); err != nil {
			h.jsonLimitErr(w, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	go h.recalcAllInputsAcrossRevisions(context.WithoutCancel(ctx), modelID) //nolint:contextcheck
	h.auditDimensionUpdated(ctx, r, dimID, "calendar_extended", map[string]string{
		"through": body.Through, "periods": strconv.Itoa(periods), "aggregates": strconv.Itoa(aggregates),
	})
	jsonOK(w, map[string]int{"periods": periods, "aggregates": aggregates})
}
