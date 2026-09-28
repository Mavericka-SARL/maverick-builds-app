package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// Property-grouping dimensions over the developer API: a dimension whose
// members group another dimension's members by a declared property
// (dimension_def.source_dimension_id + source_property). The rules live in
// metricformula.ValidateGrouping and metricformula.PlanGroupingPatch, shared
// with the AI Developer's create_dimension and update_dimension.

// groupingErr answers a grouping rejection with 400 and anything else with
// 500.
func groupingErr(w http.ResponseWriter, err error) {
	if metricformula.IsValidationError(err) {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	jsonErr(w, err, http.StatusInternalServerError)
}

// groupingDeleteErr answers a delete refused because a grouping uses the
// dimension with 409, anything else with 500.
func groupingDeleteErr(w http.ResponseWriter, err error) {
	if metricformula.IsValidationError(err) {
		jsonErr(w, err, http.StatusConflict)
		return
	}
	jsonErr(w, err, http.StatusInternalServerError)
}

// errPlanLimit marks a derive refused by the tenant's member limit; the
// caller has already answered the request.
var errPlanLimit = errors.New("plan limit")

// deriveGroupingMembers adds to grouping dimension dimID a member for every
// value of the source's property it has no member for yet, within the
// tenant's member limit. It answers the request itself when the limit
// refuses (and returns errPlanLimit).
func (h *handler) deriveGroupingMembers(ctx context.Context, w http.ResponseWriter, dimID, sourceID, prop string) ([]string, error) {
	missing, err := metricformula.MissingGroupingMembers(ctx, h.db.For(ctx), dimID, sourceID, prop)
	if err != nil || len(missing) == 0 {
		return nil, err
	}
	if cid, _ := h.customerOfDimension(ctx, dimID); cid != "" && h.plans != nil {
		if err := h.plans.CheckMembers(ctx, h.db.For(ctx), cid, dimID, len(missing)); err != nil {
			h.jsonLimitErr(w, err)
			return nil, errPlanLimit
		}
	}
	return metricformula.DeriveGroupingMembers(ctx, h.db.For(ctx), dimID, missing)
}

// planGroupingPatch reads a dimension PATCH's grouping fields —
// source_dimension_id (null clears the grouping), source_property,
// derive_members — and plans them with metricformula.PlanGroupingPatch, the
// rules the AI Developer's update_dimension shares. On a rejection it
// answers the request (400 INVALID_GROUPING, 409 DIMENSION_IN_USE) and
// returns ok=false.
func (h *handler) planGroupingPatch(ctx context.Context, w http.ResponseWriter, dimID string, raw []byte,
	sent map[string]json.RawMessage, parentSent bool, newParent *string) (metricformula.GroupingPatch, bool) {
	var body struct {
		SourceDimensionID *string `json:"source_dimension_id"`
		SourceProperty    *string `json:"source_property"`
		DeriveMembers     bool    `json:"derive_members"`
	}
	_ = json.Unmarshal(raw, &body)
	_, sourceSent := sent["source_dimension_id"]
	_, propSent := sent["source_property"]
	p, err := metricformula.PlanGroupingPatch(ctx, h.db.For(ctx), dimID, metricformula.GroupingPatchRequest{
		SourceSent: sourceSent, Source: body.SourceDimensionID,
		PropertySent: propSent, Property: body.SourceProperty,
		Derive:     body.DeriveMembers,
		ParentSent: parentSent, NewParent: newParent,
	})
	if err != nil {
		switch metricformula.ValidationCode(err) {
		case metricformula.CodeDimensionInUse:
			jsonErr(w, err, http.StatusConflict)
		case "":
			jsonErr(w, err, http.StatusInternalServerError)
		default:
			jsonErr(w, err, http.StatusBadRequest)
		}
		return p, false
	}
	return p, true
}
