package gateway

// Which revisions a caller works in.
//
// A model's active revision is what its users plan, report and approve in:
// it is open to anyone who opens the model. Any other revision — one being
// built, an older one — is its builders' (owner decision, 2026-10-02): a
// developer, tenant admin or platform admin with builder reach over the
// model. The business routes used to take any revision of the model by id
// (?revision_id, a body field, or an object that lives in one), so every
// business user of a model could read — and write into — a revision still
// being built once they learned its id. A revision closed to the caller is
// answered like one that does not exist.

import (
	"context"
	"errors"
	"net/http"
)

// errRevisionNotOpen is a revision of the model other than its active one,
// named by someone who does not build the model.
var errRevisionNotOpen = errors.New("revision not open to the caller")

// revisionOpen reports whether act may read or work in revisionID of
// modelID. "" is revision-global and open to everyone who opens the model.
// Fails closed.
func (h *handler) revisionOpen(ctx context.Context, act *actor, modelID, revisionID string) bool {
	if revisionID == "" {
		return true
	}
	// The revision resolveRevisionCtx serves when none is named: the active
	// one, else the newest.
	if open, _, err := h.resolveRevisionCtx(ctx, "", modelID); err == nil && open == revisionID {
		return true
	}
	if act == nil {
		return false
	}
	builder, err := h.actorCanAccessModel(withBuilderRoute(ctx), act, modelID)
	return err == nil && builder
}

// businessRevisionCtx is resolveRevisionCtx for a route anyone who opens the
// model may call: a named revision other than the open one answers
// errRevisionNotOpen unless the caller builds the model.
func (h *handler) businessRevisionCtx(ctx context.Context, r *http.Request, revisionID, modelID string) (revID, name string, err error) {
	revID, name, err = h.resolveRevisionCtx(ctx, revisionID, modelID)
	if err != nil || revisionID == "" {
		return revID, name, err
	}
	act, aerr := h.resolveActor(ctx, r)
	if aerr != nil || !h.revisionOpen(ctx, act, modelID, revID) {
		return "", "", errRevisionNotOpen
	}
	return revID, name, nil
}

// revisionModel is the model revisionID belongs to; "" when unknown.
func (h *handler) revisionModel(ctx context.Context, revisionID string) string {
	var modelID string
	_ = h.db.QueryRow(ctx, `SELECT model_id::text FROM model.revision WHERE id::text = $1`, revisionID).Scan(&modelID)
	return modelID
}
