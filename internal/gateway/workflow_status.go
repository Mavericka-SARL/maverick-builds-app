package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ── GET /api/workflow/status?name=<workflow> ───────────────────────────────

// workflowStatus serves a dashboard's workflow-status widget: where the
// latest run of one published workflow of the caller's model stands — each
// human step (task, approval) in the definition's order, done, current (since
// when, due when, for which roles), sent back, skipped or still to come.
// Anyone who opens the model may read it, as they may read the workflow
// itself (workflowDefinition): step names, roles and times, never a person,
// a comment or the run's context (owner decision, 2026-10-10). Test runs are
// left out. The widget names the workflow by name, so a revision copy or a
// model package keeps it pointing at the same process.
func (h *handler) workflowStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := h.resolveActor(ctx, r); err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		jsonErr(w, fmt.Errorf("name is required: the workflow's name"), http.StatusBadRequest)
		return
	}
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	var appID, activeRev string
	if err := h.db.QueryRow(ctx, `SELECT application_id::text, COALESCE(active_revision_id::text,'') FROM core.model WHERE id=$1::uuid`,
		modelID).Scan(&appID, &activeRev); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	type stepOut struct {
		ID          string     `json:"id"`
		Name        string     `json:"name"`
		Type        string     `json:"type"`
		Roles       []string   `json:"roles"`
		State       string     `json:"state"` // done | current | sent_back | skipped | pending
		Since       *time.Time `json:"since,omitempty"`
		DueAt       *time.Time `json:"due_at,omitempty"`
		CompletedAt *time.Time `json:"completed_at,omitempty"`
	}
	type instanceOut struct {
		Status      string     `json:"status"`
		StartedAt   time.Time  `json:"started_at"`
		CompletedAt *time.Time `json:"completed_at,omitempty"`
	}
	out := struct {
		Workflow string       `json:"workflow"`
		Found    bool         `json:"found"`
		Instance *instanceOut `json:"instance"`
		Steps    []stepOut    `json:"steps"`
	}{Workflow: name, Steps: []stepOut{}}

	var defID string
	var defSteps json.RawMessage
	err = h.db.QueryRow(ctx, `
		SELECT id::text, name, COALESCE(steps, '[]'::jsonb) FROM workflow.workflow_def
		WHERE application_id=$1::uuid AND status='published' AND lower(name)=lower($2)
		  AND (revision_id IS NULL OR revision_id::text=$3)
		ORDER BY (revision_id IS NOT NULL) DESC LIMIT 1`, appID, name, activeRev).Scan(&defID, &out.Workflow, &defSteps)
	if err != nil {
		jsonOK(w, out) // no such published workflow: the widget says so
		return
	}
	out.Found = true

	var instanceID string
	var inst instanceOut
	steps := defSteps
	if err := h.db.QueryRow(ctx, `
		SELECT id::text, status::text, started_at, completed_at, COALESCE(steps_snapshot, $2::jsonb)
		FROM workflow.workflow_instance
		WHERE workflow_def_id=$1::uuid AND NOT COALESCE(test_run, false)
		ORDER BY started_at DESC LIMIT 1`, defID, defSteps).Scan(&instanceID, &inst.Status, &inst.StartedAt, &inst.CompletedAt, &steps); err == nil {
		out.Instance = &inst
	}

	type stepRow struct {
		status, decision   string
		created            time.Time
		dueAt, completedAt *time.Time
	}
	latest := map[string]stepRow{}
	if instanceID != "" {
		rows, err := h.db.Query(ctx, `
			SELECT DISTINCT ON (step_def_id) step_def_id, status::text, COALESCE(decision,''), created_at, due_at, completed_at
			FROM workflow.workflow_step WHERE instance_id=$1::uuid
			ORDER BY step_def_id, created_at DESC`, instanceID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var id string
			var s stepRow
			if err := rows.Scan(&id, &s.status, &s.decision, &s.created, &s.dueAt, &s.completedAt); err != nil {
				rows.Close()
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			latest[id] = s
		}
		rows.Close()
	}

	defs, err := workflowStepsOut(steps)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	for _, d := range defs {
		if d.Type != "task" && d.Type != "approval" {
			continue // a condition, a notification: the engine's, not a person's
		}
		s := stepOut{ID: d.ID, Name: d.Name, Type: d.Type, Roles: d.AssigneeRoles, State: "pending"}
		if row, ok := latest[d.ID]; ok {
			switch {
			case row.status == "pending" || row.status == "in_progress":
				s.State, s.Since, s.DueAt = "current", &row.created, row.dueAt
			case row.status == "skipped":
				s.State = "skipped"
			case row.status == "rejected" || strings.EqualFold(row.decision, "reject"):
				s.State, s.CompletedAt = "sent_back", row.completedAt
			default:
				s.State, s.CompletedAt = "done", row.completedAt
			}
		}
		out.Steps = append(out.Steps, s)
	}
	jsonOK(w, out)
}
