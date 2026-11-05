package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/modeledit"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// ── PUT /api/developer/models/{id}/date-format ─────────────────────────────
// developerSetDateFormat sets how a model writes dates for its readers:
// {date_format} — one of modeledit.DateFormats, "" leaving it to the reader's
// browser. Developer only, in a model the caller may build in; a model
// outside the caller's scope answers 404. Audited as model.updated.
func (h *handler) developerSetDateFormat(w http.ResponseWriter, r *http.Request) {
	ctx := withBuilderRoute(r.Context())
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	modelID := strings.TrimSpace(r.PathValue("id"))
	var body struct {
		DateFormat *string `json:"date_format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DateFormat == nil {
		jsonErr(w, fmt.Errorf("date_format is required: one of %s, or \"\" for the reader's browser", strings.Join(modeledit.DateFormats[1:], ", ")), http.StatusBadRequest)
		return
	}
	allowed, err := h.actorCanAccessModel(ctx, act, modelID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !allowed {
		jsonErr(w, fmt.Errorf("model not found"), http.StatusNotFound)
		return
	}
	if err := modeledit.SetDateFormat(ctx, h.db, modelID, *body.DateFormat); err != nil {
		var fe *modeledit.DateFormatError
		if errors.As(err, &fe) {
			status := http.StatusBadRequest
			if fe.Msg == "model not found" {
				status = http.StatusNotFound
			}
			jsonErr(w, err, status)
			return
		}
		jsonErr(w, fmt.Errorf("set date format: %w", err), http.StatusInternalServerError)
		return
	}
	var appID string
	_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id = $1::uuid`, modelID).Scan(&appID)
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventModelUpdated,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: appID, ResourceType: "model", ResourceID: modelID,
		Metadata: map[string]string{"date_format": *body.DateFormat},
	})
	jsonOK(w, map[string]string{"status": "ok", "date_format": strings.TrimSpace(*body.DateFormat)})
}
