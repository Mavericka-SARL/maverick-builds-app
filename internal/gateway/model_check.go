package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/modelcheck"
)

// Developer Models › Model check (internal/modelcheck): GET reads a revision's
// findings; PUT …/ignores keeps one with a reason, DELETE …/ignores brings it
// back. The revision is one of the developer's current model.

// modelCheck serves GET /api/developer/model-check?revision_id=.
func (h *handler) modelCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	revID, _, err := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if err != nil {
		jsonErr(w, fmt.Errorf("revision not found"), http.StatusNotFound)
		return
	}
	if !h.requireRevisionInModel(w, r, revID, modelID) {
		return
	}
	rep, err := modelcheck.Run(ctx, h.db.For(ctx), modelID, revID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, rep)
}

// modelCheckIgnores serves PUT and DELETE /api/developer/model-check/ignores.
// PUT {revision_id, key, reason} ignores a finding the revision has now;
// DELETE ?revision_id=&key= un-ignores it.
func (h *handler) modelCheckIgnores(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	var body struct {
		RevisionID string `json:"revision_id"`
		Key        string `json:"key"`
		Reason     string `json:"reason"`
	}
	if r.Method == http.MethodDelete {
		body.RevisionID, body.Key = r.URL.Query().Get("revision_id"), r.URL.Query().Get("key")
	} else if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body: %w", err), http.StatusBadRequest)
		return
	}
	if body.RevisionID == "" || strings.TrimSpace(body.Key) == "" {
		jsonErr(w, errors.New("revision_id and key are required"), http.StatusBadRequest)
		return
	}
	if !h.requireRevisionInModel(w, r, body.RevisionID, modelID) {
		return
	}
	db := h.db.For(ctx)
	if r.Method == http.MethodDelete {
		if _, err := db.Exec(ctx, `DELETE FROM model.check_ignore WHERE revision_id = $1::uuid AND key = $2`, body.RevisionID, body.Key); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]any{"ignored": false})
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		jsonErr(w, errors.New("say why the finding is kept: a reason is required"), http.StatusBadRequest)
		return
	}
	rep, err := modelcheck.Run(ctx, db, modelID, body.RevisionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	var rule string
	for _, f := range rep.Findings {
		if f.Key == body.Key {
			rule = f.Rule
		}
	}
	if rule == "" {
		jsonErr(w, errors.New("the revision has no such finding now"), http.StatusNotFound)
		return
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO model.check_ignore (revision_id, key, rule, reason, ignored_by)
		VALUES ($1::uuid, $2, $3, $4, NULLIF($5, '')::uuid)
		ON CONFLICT (revision_id, key) DO UPDATE SET reason = EXCLUDED.reason, ignored_by = EXCLUDED.ignored_by, ignored_at = now()`,
		body.RevisionID, body.Key, rule, strings.TrimSpace(body.Reason), a.UserID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"ignored": true})
}

// saveWarnings is the model check's open findings about the object just
// saved (kind, id), for its save to answer with: the developer learns of a
// total worked out from blanks when saving the metric, not on a later
// review. Bounded: a check that cannot finish quickly answers none.
func (h *handler) saveWarnings(ctx context.Context, modelID, revisionID, kind, id string) []modelcheck.Finding {
	if modelID == "" || revisionID == "" || id == "" {
		return []modelcheck.Finding{}
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rep, err := modelcheck.Run(cctx, h.db.For(ctx), modelID, revisionID)
	if err != nil {
		return []modelcheck.Finding{}
	}
	return rep.For(kind, id)
}

// planModelCheckWarnings is the model check's findings a plan would add:
// the revision checked as the plan leaves it (in its dry run, tx) against
// the revision as it is, one line each, with the fix. The AI Developer is
// told what a developer saving the same objects would be.
func (h *handler) planModelCheckWarnings(ctx context.Context, tx pgx.Tx, modelID, revisionID string) []string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	after, err := modelcheck.Run(cctx, tx, modelID, revisionID)
	if err != nil {
		return nil
	}
	before := map[string]bool{}
	if rep, err := modelcheck.Run(cctx, h.db.For(ctx), modelID, revisionID); err == nil {
		for _, f := range rep.Findings {
			before[f.Key] = true
		}
	}
	var out []string
	for _, f := range after.Open() {
		// Information is for the developer's review, not a reason to send
		// a plan back.
		if before[f.Key] || f.Severity == modelcheck.SeverityInfo {
			continue
		}
		out = append(out, fmt.Sprintf("Model check (%s): %s Fix: %s", f.Title, f.Message, f.Fix))
	}
	return out
}
