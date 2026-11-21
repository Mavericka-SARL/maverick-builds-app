package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
)

// workflowDefinition serves GET /api/workflow/definitions/{id}: one
// published workflow of the caller's model, with its steps as anyone who
// opens the model may read them — the process, not its machinery. Each step
// gives its name, type, written instructions, the roles that act on it
// (role names, as the designer wrote them), its deadline in hours, its
// completion label and which step follows; never a condition, a
// notification's recipients or anything naming a person (owner decision,
// 2026-10-10). The chat connector reads it to explain a process; the list
// (workflowDefinitions) stays what the start dialog needs.
func (h *handler) workflowDefinition(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := h.resolveActor(ctx, r); err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	defID := r.PathValue("id")
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	var appID, activeRev string
	if err := h.db.QueryRow(ctx, `
		SELECT application_id::text, COALESCE(active_revision_id::text,'')
		FROM core.model WHERE id=$1::uuid
	`, modelID).Scan(&appID, &activeRev); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	// The list's own filter: published, of this model's application, in
	// its active revision (or revision-less).
	var out struct {
		ID            string            `json:"id"`
		Name          string            `json:"name"`
		Description   string            `json:"description"`
		TriggerEvent  string            `json:"trigger_event"`
		ContextSchema json.RawMessage   `json:"context_schema"`
		Steps         []workflowStepOut `json:"steps"`
	}
	var steps json.RawMessage
	if err := h.db.QueryRow(ctx, `
		SELECT id::text, name, COALESCE(description,''), trigger_event, COALESCE(context_schema, '[]'::jsonb), COALESCE(steps, '[]'::jsonb)
		FROM workflow.workflow_def
		WHERE id::text=$1 AND application_id=$2::uuid AND status='published'
		  AND (revision_id IS NULL OR revision_id::text=$3)
	`, defID, appID, activeRev).Scan(&out.ID, &out.Name, &out.Description, &out.TriggerEvent, &out.ContextSchema, &steps); err != nil {
		jsonErr(w, fmt.Errorf("workflow not found"), http.StatusNotFound)
		return
	}
	if out.Steps, err = workflowStepsOut(steps); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	out.ContextSchema = publicContextSchema(out.ContextSchema)
	jsonOK(w, out)
}

// publicContextSchema is a context schema as the people who start and
// follow a workflow read it: a variable's default formula is the
// designer's machinery, so it reads has_default: true instead (the start
// dialog asks start-defaults for the values). A schema without defaults is
// returned as it is.
func publicContextSchema(raw json.RawMessage) json.RawMessage {
	var vars []map[string]any
	if json.Unmarshal(raw, &vars) != nil {
		return raw
	}
	changed := false
	for _, v := range vars {
		if d, ok := v["default"]; ok {
			delete(v, "default")
			if s, _ := d.(string); strings.TrimSpace(s) != "" {
				v["has_default"] = true
			}
			changed = true
		}
	}
	if !changed {
		return raw
	}
	b, err := json.Marshal(vars)
	if err != nil {
		return raw
	}
	return b
}

// workflowStartDefaults serves GET /api/workflow/definitions/{id}/start-defaults:
// the values the workflow's variables default to, as a start with nothing
// given would take them, for the start dialog to show filled in (people may
// change them). Only someone who may start it asks: a default reads the
// model as the workflow does, and the run it starts would hold the same
// values. values maps a variable to its value; errors a variable whose
// default cannot be worked out to the reason.
func (h *handler) workflowStartDefaults(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	defID := r.PathValue("id")
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	var published bool
	if err := h.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM workflow.workflow_def wd JOIN core.model m ON m.application_id = wd.application_id
		               WHERE wd.id::text = $1 AND m.id = $2::uuid AND wd.status = 'published'
		                 AND (wd.revision_id IS NULL OR wd.revision_id = m.active_revision_id))`,
		defID, modelID).Scan(&published); err != nil || !published {
		jsonErr(w, fmt.Errorf("workflow not found"), http.StatusNotFound)
		return
	}
	if onlyApprover(a) && !h.workflowStore(ctx).ApproverMayStart(ctx, defID) {
		jsonErr(w, errApproverStarts, http.StatusForbidden)
		return
	}
	if h.refuseNonStarter(ctx, w, a, defID) {
		return
	}
	values, errs, err := h.workflowStore(ctx).StartDefaults(ctx, defID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"values": values, "errors": errs})
}

// workflowStepOut is a step as workflowDefinition shows it.
type workflowStepOut struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Type            string            `json:"type"`
	Instructions    string            `json:"instructions,omitempty"`
	AssigneeRoles   []string          `json:"assignee_roles"`
	SLAHours        int32             `json:"sla_hours,omitempty"`
	CompletionLabel string            `json:"completion_label,omitempty"`
	RequiredComment bool              `json:"required_comment,omitempty"`
	Routes          map[string]string `json:"routes,omitempty"`
	NextStepIDs     []string          `json:"next_step_ids,omitempty"`
}

// workflowStepsOut keeps the fields workflowStepOut names and drops every
// other one a step holds.
func workflowStepsOut(raw json.RawMessage) ([]workflowStepOut, error) {
	var in []struct {
		ID              string            `json:"id"`
		Name            string            `json:"name"`
		Type            json.RawMessage   `json:"type"`
		Instructions    string            `json:"instructions"`
		AssigneeRoles   []string          `json:"assignee_roles"`
		SLAHours        int32             `json:"sla_hours"`
		CompletionLabel string            `json:"completion_label"`
		RequiredComment bool              `json:"required_comment"`
		Routes          map[string]string `json:"routes"`
		NextStepIDs     []string          `json:"next_step_ids"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("read steps: %w", err)
	}
	out := make([]workflowStepOut, 0, len(in))
	for _, s := range in {
		roles := s.AssigneeRoles
		if roles == nil {
			roles = []string{}
		}
		out = append(out, workflowStepOut{ID: s.ID, Name: s.Name, Type: stepTypeWord(s.Type), Instructions: s.Instructions,
			AssigneeRoles: roles, SLAHours: s.SLAHours, CompletionLabel: s.CompletionLabel, RequiredComment: s.RequiredComment,
			Routes: s.Routes, NextStepIDs: s.NextStepIDs})
	}
	return out, nil
}

// stepTypeWord is a step's type as the designer writes it ("approval"),
// whether stored that way or as the proto's number or name.
func stepTypeWord(raw json.RawMessage) string {
	var word string
	if json.Unmarshal(raw, &word) == nil {
		return strings.ToLower(strings.TrimPrefix(word, "STEP_TYPE_"))
	}
	var n int32
	if json.Unmarshal(raw, &n) == nil {
		if name, ok := workflowv1.StepType_name[n]; ok {
			return strings.ToLower(strings.TrimPrefix(name, "STEP_TYPE_"))
		}
	}
	return "unspecified"
}
