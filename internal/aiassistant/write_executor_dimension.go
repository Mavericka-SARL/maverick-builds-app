package aiassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/tags"
)

// firstNonEmpty returns the first of its arguments that is not blank.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// resolveDimensionRef resolves a dimension named by id or name into the
// working revision (requireInModel: another model's dimension is refused,
// another revision's is remapped to its counterpart or refused). An
// unscoped executor (revID from the params) resolves a name within revID.
func (e *WriteExecutor) resolveDimensionRef(ctx context.Context, ref, revID string) (string, error) {
	if e.revID == "" && !uuidShaped(ref) {
		var id string
		if err := e.pool.QueryRow(ctx, `
			SELECT id::text FROM model.dimension_def
			WHERE model_id=$1::uuid AND revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid AND name=$3
		`, e.modelID, revID, ref).Scan(&id); err != nil {
			return "", fmt.Errorf("dimension %q not found in the working revision — pass its exact name or id", ref)
		}
		return id, nil
	}
	return e.requireInModel(ctx, "dimension", ref)
}

// ── update_dimension ──────────────────────────────────────────────────────────

// optionalRef reads a nullable reference field of update_dimension: absent
// or "" = not given, JSON null = clear, a string = set to it.
func optionalRef(sent map[string]json.RawMessage, key string) (given bool, value *string, err error) {
	raw, ok := sent[key]
	if !ok {
		return false, nil, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return true, nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false, nil, fmt.Errorf("%s must be a string or null", key)
	}
	if strings.TrimSpace(s) == "" {
		return false, nil, nil
	}
	s = strings.TrimSpace(s)
	return true, &s, nil
}

// updateDimension is the AI Developer's twin of the developer console's
// PATCH /api/developer/dimensions/{id}: a partial update (a field left out
// keeps its value) of the name, the rollup rule, the parent dimension and
// the property grouping, checked by the same shared rules —
// metricformula.ValidateParentDimension, metricformula.PlanGroupingPatch
// (ValidateGrouping, the DIMENSION_IN_USE refusal on clearing or replacing
// a grouping's source) and the DIMENSION_NAME_TAKEN refusal. The parent and
// the source are named by id or name and resolve within the working
// revision. dimension_type and the time settings are immutable, as there.
//
// The developer path recalculates the metrics reading the dimension when
// the grouping changes; here the write lands in the AI's draft, and
// promoting the draft recomputes every calculated metric of the revision
// (activateRevision).
func (e *WriteExecutor) updateDimension(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		DimensionID   string    `json:"dimension_id"`
		Name          string    `json:"name"`
		AggRule       string    `json:"agg_rule"`
		Tags          *[]string `json:"tags"`
		DeriveMembers bool      `json:"derive_members"`
		// Read from sent below: a string, or null to clear.
		ParentDimensionID   json.RawMessage `json:"parent_dimension_id"`
		ParentDimensionName json.RawMessage `json:"parent_dimension_name"`
		SourceDimensionID   json.RawMessage `json:"source_dimension_id"`
		SourceDimensionName json.RawMessage `json:"source_dimension_name"`
		SourceProperty      json.RawMessage `json:"source_property"`
	}
	var sent map[string]json.RawMessage
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if err := json.Unmarshal(raw, &sent); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if strings.TrimSpace(p.DimensionID) == "" {
		return "", "", fmt.Errorf("dimension_id is required (the dimension's id or exact name)")
	}
	dimID, err := e.requireInModel(ctx, "dimension", strings.TrimSpace(p.DimensionID))
	if err != nil {
		return "", "", err
	}
	var modelID, revisionID, dimType, curName string
	if err := e.pool.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text,''), dimension_type, name
		FROM model.dimension_def WHERE id=$1::uuid
	`, dimID).Scan(&modelID, &revisionID, &dimType, &curName); err != nil {
		return "", "", fmt.Errorf("load dimension: %w", err)
	}

	// The parent dimension: parent_dimension_id or parent_dimension_name,
	// null clears (detaches).
	parentSent, parentRef, err := optionalRef(sent, "parent_dimension_id")
	if err != nil {
		return "", "", err
	}
	if !parentSent {
		if parentSent, parentRef, err = optionalRef(sent, "parent_dimension_name"); err != nil {
			return "", "", err
		}
	}
	var newParent *string
	if parentRef != nil {
		pid, err := e.resolveDimensionRef(ctx, *parentRef, revisionID)
		if err != nil {
			return "", "", fmt.Errorf("%s: parent dimension: %w", metricformula.CodeInvalidParentDimension, err)
		}
		if err := metricformula.ValidateParentDimension(ctx, e.pool, metricformula.ParentDimension{
			ModelID: modelID, RevisionID: revisionID, DimensionID: dimID, DimensionType: dimType,
			ParentDimensionID: pid,
		}); err != nil {
			return "", "", err
		}
		newParent = &pid
	}

	// The property grouping: source_dimension_id or source_dimension_name
	// (null clears the grouping), source_property, derive_members.
	sourceSent, sourceRef, err := optionalRef(sent, "source_dimension_id")
	if err != nil {
		return "", "", err
	}
	if !sourceSent {
		if sourceSent, sourceRef, err = optionalRef(sent, "source_dimension_name"); err != nil {
			return "", "", err
		}
	}
	var source *string
	if sourceRef != nil {
		sid, err := e.resolveDimensionRef(ctx, *sourceRef, revisionID)
		if err != nil {
			return "", "", fmt.Errorf("%s: source dimension: %w", metricformula.CodeInvalidGrouping, err)
		}
		source = &sid
	}
	propSent, prop, err := optionalRef(sent, "source_property")
	if err != nil {
		return "", "", err
	}
	if raw, ok := sent["source_property"]; ok && !propSent && strings.TrimSpace(string(raw)) != "null" {
		// "" clears the property, as the PATCH treats it.
		propSent, prop = true, nil
	}

	name, aggRule := strings.TrimSpace(p.Name), strings.TrimSpace(p.AggRule)
	if err := metricformula.CheckRenameFreeOfOtherKind(ctx, e.pool, dimID, name, "dimension"); err != nil {
		return "", "", err
	}
	if name == "" && aggRule == "" && p.Tags == nil && !parentSent && !sourceSent && !propSent && !p.DeriveMembers {
		return "", "", fmt.Errorf("nothing to change: provide name, agg_rule, tags, parent_dimension_id, " +
			"source_dimension_id, source_property or derive_members")
	}

	grouping, err := metricformula.PlanGroupingPatch(ctx, e.pool, dimID, metricformula.GroupingPatchRequest{
		SourceSent: sourceSent, Source: source,
		PropertySent: propSent, Property: prop,
		Derive:     p.DeriveMembers,
		ParentSent: parentSent, NewParent: newParent,
	})
	if err != nil {
		return "", "", err
	}

	var newTags []string
	if p.Tags != nil {
		newTags = tags.Clean(*p.Tags)
	}
	if _, err := e.pool.Exec(ctx, `
		UPDATE model.dimension_def
		SET name=COALESCE(NULLIF($2,''), name), agg_rule=COALESCE(NULLIF($3,''), agg_rule),
		    parent_dimension_id=CASE WHEN $5::boolean THEN $4::uuid ELSE parent_dimension_id END,
		    tags=COALESCE($6, tags),
		    source_dimension_id=CASE WHEN $7::boolean THEN $8::uuid ELSE source_dimension_id END,
		    source_property=CASE WHEN $7::boolean THEN $9 ELSE source_property END
		WHERE id=$1::uuid`,
		dimID, name, aggRule, newParent, parentSent, newTags,
		grouping.Write, grouping.Source, grouping.Property); err != nil {
		if metricformula.IsUniqueViolation(err) {
			return "", "", metricformula.DimensionNameTaken(err, name)
		}
		return "", "", fmt.Errorf("update dimension: %w", err)
	}

	display := curName
	if name != "" {
		display = name
	}
	var changes []string
	if name != "" && name != curName {
		changes = append(changes, fmt.Sprintf("renamed from '%s'", curName))
	}
	if aggRule != "" {
		changes = append(changes, "rollup rule "+aggRule)
	}
	if p.Tags != nil {
		changes = append(changes, "tags set")
	}
	if parentSent {
		if parentRef == nil {
			changes = append(changes, "detached from its parent dimension")
		} else {
			changes = append(changes, "parent dimension "+*parentRef)
		}
	}
	if grouping.Write {
		if grouping.Source == nil {
			changes = append(changes, "property grouping cleared")
		} else {
			changes = append(changes, "groups the source dimension's members by "+*grouping.Property)
		}
	}
	if grouping.Derive {
		missing, err := metricformula.MissingGroupingMembers(ctx, e.pool, dimID, *grouping.Source, *grouping.Property)
		if err != nil {
			return "", "", fmt.Errorf("derive grouping members: %w", err)
		}
		var added []string
		if err := e.checkMembers(ctx, dimID, len(missing)); err != nil {
			return "", "", err
		}
		if len(missing) > 0 {
			if added, err = metricformula.DeriveGroupingMembers(ctx, e.pool, dimID, missing); err != nil {
				return "", "", fmt.Errorf("derive grouping members: %w", err)
			}
		}
		if len(added) > 0 {
			changes = append(changes, fmt.Sprintf("derived %d member(s): %s", len(added), strings.Join(added, ", ")))
		} else {
			changes = append(changes, "no new members to derive")
		}
	}
	msg := fmt.Sprintf("Dimension '%s' updated (id: %s)", display, dimID)
	if len(changes) > 0 {
		msg += ": " + strings.Join(changes, "; ")
	}
	return msg, "", nil
}
