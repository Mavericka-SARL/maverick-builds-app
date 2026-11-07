package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Integration templates (migration 158): a loader or an export saved once
// and made again in any model of the tenant, its target found by name.

// integrationTemplate is one template as the Integrations tab lists it.
type integrationTemplate struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	TargetType string          `json:"target_type"`
	TargetName string          `json:"target_name"`
	Config     json.RawMessage `json:"config"`
	CreatedAt  string          `json:"created_at"`
}

// templateTypes are the kinds of integration a template may hold: what
// needs no connection and reads no other model.
func templateType(t string) bool { return t != "rest_api" && t != "model" && t != "" }

// developerIntegrationTemplates handles GET and POST
// /api/developer/integration-templates: the templates of the open model's
// tenant, and saving one from an integration of the open model.
func (h *handler) developerIntegrationTemplates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	customerID := h.customerOfModel(ctx, modelID)
	if customerID == "" {
		jsonErr(w, fmt.Errorf("this model belongs to no tenant"), http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodGet {
		rows, err := h.db.Query(ctx, `
			SELECT id::text, name, type, target_type, target_name, config, created_at::text
			FROM core.integration_template WHERE customer_id = $1::uuid ORDER BY name`, customerID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []integrationTemplate{}
		for rows.Next() {
			var t integrationTemplate
			if err := rows.Scan(&t.ID, &t.Name, &t.Type, &t.TargetType, &t.TargetName, &t.Config, &t.CreatedAt); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			out = append(out, t)
		}
		jsonOK(w, out)
		return
	}

	var body struct {
		IntegrationID string `json:"integration_id"`
		Name          string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.IntegrationID == "" {
		jsonErr(w, fmt.Errorf("integration_id required"), http.StatusBadRequest)
		return
	}
	var t integrationTemplate
	err = h.db.QueryRow(ctx, `
		SELECT i.name, i.type, i.target_type, i.config,
		       COALESCE(CASE i.target_type
		           WHEN 'form' THEN (SELECT f.name FROM model.form_def f WHERE f.id = i.target_id)
		           ELSE (SELECT g.name FROM model.grid_def g WHERE g.id = i.target_id) END, '')
		FROM model.integration_def i WHERE i.id::text = $1 AND i.model_id = $2::uuid`,
		body.IntegrationID, modelID).Scan(&t.Name, &t.Type, &t.TargetType, &t.Config, &t.TargetName)
	if errors.Is(err, pgx.ErrNoRows) {
		jsonErr(w, fmt.Errorf("integration not found in this model"), http.StatusNotFound)
		return
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !templateType(t.Type) {
		jsonErr(w, fmt.Errorf("a %s integration holds a connection or another model and belongs to its model; it cannot be a template", t.Type), http.StatusBadRequest)
		return
	}
	if n := strings.TrimSpace(body.Name); n != "" {
		t.Name = n
	}
	t.Config = withoutTargetID(t.Config)
	var createdBy any
	if a, e := h.resolveActor(ctx, r); e == nil {
		createdBy = a.UserID
	}
	if err := h.db.QueryRow(ctx, `
		INSERT INTO core.integration_template (customer_id, name, type, target_type, target_name, config, created_by)
		VALUES ($1::uuid, $2, $3, $4, $5, $6::jsonb, $7::uuid) RETURNING id::text`,
		customerID, t.Name, t.Type, t.TargetType, t.TargetName, string(t.Config), createdBy).Scan(&t.ID); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			jsonErr(w, fmt.Errorf("a template named %q already exists", t.Name), http.StatusConflict)
			return
		}
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"id": t.ID})
}

// developerIntegrationTemplateUse handles POST
// /api/developer/integration-templates/{id}/use {"name"}: the template made
// in the open model's working revision, its target found by name there.
func (h *handler) developerIntegrationTemplateUse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	revisionID, _, revErr := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, revErr) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	var t integrationTemplate
	err = h.db.QueryRow(ctx, `
		SELECT name, type, target_type, target_name, config FROM core.integration_template
		WHERE id::text = $1 AND customer_id = $2::uuid`, r.PathValue("id"), h.customerOfModel(ctx, modelID)).
		Scan(&t.Name, &t.Type, &t.TargetType, &t.TargetName, &t.Config)
	if errors.Is(err, pgx.ErrNoRows) {
		jsonErr(w, fmt.Errorf("template not found"), http.StatusNotFound)
		return
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if n := strings.TrimSpace(body.Name); n != "" {
		t.Name = n
	}
	var targetID *string
	if t.TargetName != "" {
		table := "model.grid_def"
		if t.TargetType == "form" {
			table = "model.form_def"
		}
		var id string
		err := h.db.QueryRow(ctx, `SELECT id::text FROM `+table+`
			WHERE model_id = $1::uuid AND (revision_id IS NULL OR revision_id::text = $2) AND lower(name) = lower($3)
			ORDER BY revision_id NULLS LAST LIMIT 1`, modelID, revisionID, t.TargetName).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			jsonErr(w, fmt.Errorf("this model has no %s named %q for the template to load into", map[bool]string{true: "table", false: "grid"}[t.TargetType == "form"], t.TargetName), http.StatusBadRequest)
			return
		}
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		targetID = &id
	}
	var newID string
	if err := h.db.QueryRow(ctx, `
		INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, target_id, config, status, tags)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5, $6::uuid, $7::jsonb, 'active', '{}')
		RETURNING id::text`, modelID, revisionID, t.Name, t.Type, t.TargetType, targetID, string(t.Config)).Scan(&newID); err != nil {
		writeIntegrationErr(w, err, t.Name)
		return
	}
	jsonOK(w, map[string]string{"id": newID, "status": "created"})
}

// developerIntegrationTemplateDelete handles DELETE
// /api/developer/integration-templates/{id}.
func (h *handler) developerIntegrationTemplateDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	tag, err := h.db.Exec(ctx, `DELETE FROM core.integration_template WHERE id::text = $1 AND customer_id = $2::uuid`,
		r.PathValue("id"), h.customerOfModel(ctx, modelID))
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		jsonErr(w, fmt.Errorf("template not found"), http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]string{"status": "deleted"})
}

// withoutTargetID drops a config's own target_id: it names a row of the
// model the template came from, and the target is found by name instead.
func withoutTargetID(config json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(config, &m) != nil || m == nil {
		return json.RawMessage("{}")
	}
	delete(m, "target_id")
	b, _ := json.Marshal(m)
	return b
}
