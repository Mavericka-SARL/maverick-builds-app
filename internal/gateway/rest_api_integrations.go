package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// REST API connector endpoints (spec §6). The gateway VALIDATES and
// PERSISTS; it never executes external requests — /test and /run enqueue a
// job (202 + run id) that cmd/integration's worker claims and executes.
// Legacy csv_import / google_sheets handlers are untouched; requests for
// type "rest_api" are delegated here from the shared routes.

func (h *handler) intStore(ctx context.Context) *integration.Store {
	return integration.NewStore(h.db.For(ctx))
}

// integrationAllowInsecure loosens URL rules for local fixtures in dev mode
// only — production never sets devMode.
func (h *handler) integrationAllowInsecure() bool { return h.devMode }

// resolveIntegrationApp returns (modelID, applicationID) for an integration
// and enforces caller access, the both-at-save-and-execution ownership rule.
func (h *handler) resolveIntegrationApp(w http.ResponseWriter, r *http.Request, integrationID string) (modelID, appID string, ok bool) {
	ctx := r.Context()
	if err := h.db.QueryRow(ctx, `
		SELECT i.model_id::text, m.application_id::text
		FROM model.integration_def i JOIN core.model m ON m.id = i.model_id
		WHERE i.id=$1::uuid
	`, integrationID).Scan(&modelID, &appID); err != nil {
		jsonErr(w, fmt.Errorf("integration not found"), http.StatusNotFound)
		return "", "", false
	}
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return "", "", false
	}
	if can, cerr := h.actorCanAccessApp(ctx, act, appID); cerr != nil || !can {
		jsonErr(w, fmt.Errorf("forbidden: integration is outside your access scope"), http.StatusForbidden)
		return "", "", false
	}
	return modelID, appID, true
}

// checkRestAPITargets validates that the config's target and connection
// belong to the same model/application — run at save and re-run by the
// worker at execution.
func (h *handler) checkRestAPITargets(r *http.Request, modelID, appID string, cfg *integration.Config, connectionID string) error {
	ctx := r.Context()
	var table string
	switch cfg.TargetType {
	case integration.TargetGrid:
		table = "model.grid_def"
	case integration.TargetForm:
		table = "model.form_def"
	case integration.TargetDimension:
		table = "model.dimension_def"
	default:
		return fmt.Errorf("unknown target type")
	}
	var owner string
	if err := h.db.QueryRow(ctx,
		"SELECT model_id::text FROM "+table+" WHERE id=$1::uuid", cfg.TargetID).Scan(&owner); err != nil {
		return fmt.Errorf("target %s not found", cfg.TargetType)
	}
	if owner != modelID {
		return fmt.Errorf("target belongs to a different model")
	}
	if connectionID != "" {
		var connApp string
		if err := h.db.QueryRow(ctx, `
			SELECT application_id::text FROM model.integration_connection WHERE id=$1::uuid
		`, connectionID).Scan(&connApp); err != nil {
			return fmt.Errorf("connection not found")
		}
		if connApp != appID {
			return fmt.Errorf("connection belongs to a different application")
		}
	}
	return nil
}

// restAPIDetail is the typed GET/PATCH response: definition + schedule.
type restAPIDetail struct {
	*integration.Definition
	Schedule *integration.Schedule `json:"schedule,omitempty"`
	Tested   bool                  `json:"tested"`
}

func (h *handler) restAPIRespond(w http.ResponseWriter, r *http.Request, modelID, id string) {
	def, err := h.intStore(r.Context()).GetDefinition(r.Context(), modelID, id)
	if err != nil {
		jsonErr(w, fmt.Errorf("integration not found"), http.StatusNotFound)
		return
	}
	sched, _ := h.intStore(r.Context()).GetSchedule(r.Context(), id)
	jsonOK(w, restAPIDetail{Definition: def, Schedule: sched, Tested: def.Tested()})
}

// restAPICreateBody is the atomic create/update payload.
type restAPICreateBody struct {
	Name         string                `json:"name"`
	Description  *string               `json:"description,omitempty"`
	Tags         []string              `json:"tags,omitempty"`
	Status       string                `json:"status,omitempty"`
	Enabled      *bool                 `json:"enabled,omitempty"`
	ConnectionID *string               `json:"connection_id,omitempty"`
	Config       *integration.Config   `json:"config,omitempty"`
	Schedule     *integration.Schedule `json:"schedule,omitempty"`
}

// restAPICreate handles POST /api/developer/integrations with type=rest_api —
// metadata + typed config + schedule in one atomic call.
func (h *handler) restAPICreate(w http.ResponseWriter, r *http.Request, modelID, revisionID string, raw []byte) {
	ctx := r.Context()
	var body restAPICreateBody
	if err := json.Unmarshal(raw, &body); err != nil || body.Name == "" || body.Config == nil {
		jsonErr(w, fmt.Errorf("name and config are required"), http.StatusBadRequest)
		return
	}
	var appID string
	_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
	connID := ""
	if body.ConnectionID != nil {
		connID = *body.ConnectionID
	}
	status := body.Status
	if status == "" {
		status = "draft"
	}
	if status == "active" || body.Config != nil {
		if err := h.checkRestAPITargets(r, modelID, appID, body.Config, connID); err != nil && status == "active" {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
	}
	desc := ""
	if body.Description != nil {
		desc = *body.Description
	}
	def, err := h.intStore(ctx).CreateDefinition(ctx, modelID, revisionID, body.Name, desc, body.Tags, status, connID, body.Config, h.integrationAllowInsecure())
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if body.Schedule != nil {
		body.Schedule.IntegrationID = def.ID
		if act, aerr := h.resolveActor(ctx, r); aerr == nil && body.Schedule.Enabled {
			body.Schedule.EnabledBy = act.UserID
		}
		if err := h.intStore(ctx).UpsertSchedule(ctx, body.Schedule); err != nil {
			jsonErr(w, fmt.Errorf("schedule: %w", err), http.StatusBadRequest)
			return
		}
	}
	if a, e := h.resolveActor(ctx, r); e == nil {
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventIntegrationCreated,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: appID, ResourceType: "integration", ResourceID: def.ID, RevisionID: revisionID,
			Metadata: map[string]string{"name": def.Name, "type": "rest_api", "host": integration.SanitizedHost(def.Config.Request.URL)},
		})
	}
	h.restAPIRespond(w, r, modelID, def.ID)
}

// restAPIUpdate handles PATCH for a rest_api integration: atomic metadata +
// config + schedule.
func (h *handler) restAPIUpdate(w http.ResponseWriter, r *http.Request, modelID, id string) {
	ctx := r.Context()
	var body restAPICreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	var appID string
	_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
	if body.Config != nil {
		connID := ""
		if body.ConnectionID != nil {
			connID = *body.ConnectionID
		}
		if err := h.checkRestAPITargets(r, modelID, appID, body.Config, connID); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
	}
	var namePtr, statusPtr *string
	if body.Name != "" {
		namePtr = &body.Name
	}
	if body.Status != "" {
		statusPtr = &body.Status
	}
	if body.Enabled != nil {
		if _, err := h.db.Exec(ctx, `UPDATE model.integration_def SET enabled=$2 WHERE id=$1::uuid AND model_id=$3::uuid`, id, *body.Enabled, modelID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}
	if _, err := h.intStore(ctx).UpdateDefinition(ctx, modelID, id, namePtr, body.Description, body.Tags, statusPtr, body.ConnectionID, body.Config, h.integrationAllowInsecure()); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, pgx.ErrNoRows) {
			code = http.StatusNotFound
		}
		jsonErr(w, err, code)
		return
	}
	if body.Schedule != nil {
		body.Schedule.IntegrationID = id
		if act, aerr := h.resolveActor(ctx, r); aerr == nil && body.Schedule.Enabled {
			body.Schedule.EnabledBy = act.UserID
		}
		if err := h.intStore(ctx).UpsertSchedule(ctx, body.Schedule); err != nil {
			jsonErr(w, fmt.Errorf("schedule: %w", err), http.StatusBadRequest)
			return
		}
	}
	h.auditIntegrationEvent(r, appID, id, auditlog.EventIntegrationUpdated, map[string]string{"type": "rest_api"})
	h.restAPIRespond(w, r, modelID, id)
}

func (h *handler) auditIntegrationEvent(r *http.Request, appID, id string, event auditlog.EventType, meta map[string]string) {
	ctx := r.Context()
	if a, e := h.resolveActor(ctx, r); e == nil {
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: event,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: appID, ResourceType: "integration", ResourceID: id, Metadata: meta,
		})
	}
}

// restAPIDuplicate copies the definition (name " (copy)", test state cleared,
// schedule copied DISABLED) — never run history.
func (h *handler) restAPIDuplicate(w http.ResponseWriter, r *http.Request, modelID, id string) {
	ctx := r.Context()
	st := h.intStore(ctx)
	src, err := st.GetDefinition(ctx, modelID, id)
	if err != nil {
		jsonErr(w, fmt.Errorf("integration not found"), http.StatusNotFound)
		return
	}
	dup, err := st.CreateDefinition(ctx, modelID, src.RevisionID, src.Name+" (copy)", src.Description, src.Tags, "draft", src.ConnectionID, src.Config, h.integrationAllowInsecure())
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if sched, serr := st.GetSchedule(ctx, id); serr == nil && sched.Kind != "manual" {
		sched.IntegrationID = dup.ID
		sched.Enabled = false
		sched.EnabledBy = ""
		_ = st.UpsertSchedule(ctx, sched)
	}
	h.restAPIRespond(w, r, modelID, dup.ID)
}

// restAPIValidate runs config validation + ownership checks without saving.
func (h *handler) restAPIValidate(w http.ResponseWriter, r *http.Request, modelID, id string) {
	ctx := r.Context()
	def, err := h.intStore(ctx).GetDefinition(ctx, modelID, id)
	if err != nil {
		jsonErr(w, fmt.Errorf("integration not found"), http.StatusNotFound)
		return
	}
	if def.Config == nil {
		jsonOK(w, map[string]any{"valid": false, "errors": []string{"no configuration saved"}})
		return
	}
	var errs []string
	if verr := def.Config.Validate(h.integrationAllowInsecure()); verr != nil {
		errs = append(errs, verr.Error())
	}
	var appID string
	_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
	if terr := h.checkRestAPITargets(r, modelID, appID, def.Config, def.ConnectionID); terr != nil {
		errs = append(errs, terr.Error())
	}
	if len(errs) == 0 {
		jsonOK(w, map[string]any{"valid": true, "config_hash": integration.ConfigHash(def.Config), "errors": []string{}})
		return
	}
	jsonOK(w, map[string]any{"valid": false, "errors": errs})
}

// restAPIEnqueue is shared by /test, /run and dry-run: 202 + run id.
func (h *handler) restAPIEnqueue(w http.ResponseWriter, r *http.Request, modelID, id, trigger string, dryRun bool) {
	ctx := r.Context()
	def, err := h.intStore(ctx).GetDefinition(ctx, modelID, id)
	if err != nil {
		jsonErr(w, fmt.Errorf("integration not found"), http.StatusNotFound)
		return
	}
	if def.Config == nil {
		jsonErr(w, fmt.Errorf("no configuration saved"), http.StatusBadRequest)
		return
	}
	if verr := def.Config.Validate(h.integrationAllowInsecure()); verr != nil {
		jsonErr(w, verr, http.StatusBadRequest)
		return
	}
	// Mutation-method tests require the explicit acknowledgement (spec §4).
	if trigger == "test" && def.Config.Request.Method != "GET" {
		if r.URL.Query().Get("acknowledge_side_effects") != "1" {
			jsonErr(w, fmt.Errorf("testing a %s request may change external data — retry with acknowledge_side_effects=1", def.Config.Request.Method), http.StatusBadRequest)
			return
		}
	}
	if trigger == "manual" && !dryRun {
		if def.Status != "active" {
			jsonErr(w, fmt.Errorf("integration is a draft — activate it first"), http.StatusBadRequest)
			return
		}
		if !def.Enabled {
			jsonErr(w, fmt.Errorf("integration is disabled"), http.StatusBadRequest)
			return
		}
	}
	act, _ := h.resolveActor(ctx, r)
	runBy := ""
	if act != nil {
		runBy = act.UserID
	}
	runID, err := h.intStore(ctx).Enqueue(ctx, id, trigger, runBy, dryRun, nil)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"run_id": runID, "status": "queued"})
}

// ── Route handlers ───────────────────────────────────────────────────────────

// restAPIIntegrationSubAction handles POST /api/developer/integrations/{id}/
// {duplicate|validate|test}.
func (h *handler) restAPIIntegrationSubAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/developer/integrations/"), "/")
	if len(parts) != 2 {
		jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
		return
	}
	id, action := parts[0], parts[1]
	modelID, _, ok := h.resolveIntegrationApp(w, r, id)
	if !ok {
		return
	}
	switch action {
	case "duplicate":
		h.restAPIDuplicate(w, r, modelID, id)
	case "validate":
		h.restAPIValidate(w, r, modelID, id)
	case "test":
		dry := r.URL.Query().Get("dry_run") == "1"
		trigger := "test"
		if dry {
			trigger = "dry_run"
		}
		h.restAPIEnqueue(w, r, modelID, id, trigger, dry)
	default:
		jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
	}
}

// integrationRunDetail handles GET /api/developer/integration-runs/{runId}
// and POST .../cancel.
func (h *handler) integrationRunDetail(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/developer/integration-runs/")
	runID := strings.TrimSuffix(tail, "/cancel")
	run, err := h.intStore(r.Context()).GetRun(r.Context(), runID)
	if err != nil {
		jsonErr(w, fmt.Errorf("run not found"), http.StatusNotFound)
		return
	}
	if _, _, ok := h.resolveIntegrationApp(w, r, run.IntegrationID); !ok {
		return
	}
	if strings.HasSuffix(tail, "/cancel") && r.Method == http.MethodPost {
		status, cerr := h.intStore(r.Context()).Cancel(r.Context(), runID)
		if cerr != nil {
			jsonErr(w, fmt.Errorf("run is not cancellable"), http.StatusConflict)
			return
		}
		jsonOK(w, map[string]string{"status": status})
		return
	}
	jsonOK(w, run)
}

// integrationConnections handles GET (list) / POST (create).
func (h *handler) integrationConnections(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	appID, err := h.resolveDemoAppID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve application")
		return
	}
	st := h.intStore(ctx)
	if r.Method == http.MethodPost {
		var body struct {
			Name     string          `json:"name"`
			AuthType string          `json:"auth_type"`
			Meta     json.RawMessage `json:"meta"`
			// Secret is write-only: absent → no credential yet.
			Secret *json.RawMessage `json:"secret,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		var secret []byte
		if body.Secret != nil {
			secret = []byte(*body.Secret)
		}
		act, _ := h.resolveActor(ctx, r)
		createdBy := ""
		if act != nil {
			createdBy = act.UserID
		}
		conn, cerr := st.CreateConnection(ctx, appID, body.Name, body.AuthType, body.Meta, secret, createdBy)
		if cerr != nil {
			jsonErr(w, cerr, http.StatusBadRequest)
			return
		}
		h.auditIntegrationEvent(r, appID, conn.ID, auditlog.EventIntegrationCreated, map[string]string{"kind": "connection", "auth_type": conn.AuthType})
		jsonOK(w, conn)
		return
	}
	list, err := st.ListConnections(ctx, appID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, list)
}

// integrationConnectionAction handles PATCH/DELETE /{id} and POST /{id}/test.
func (h *handler) integrationConnectionAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	appID, err := h.resolveDemoAppID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve application")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/developer/integration-connections/"), "/")
	id := parts[0]
	st := h.intStore(ctx)

	if len(parts) == 2 && parts[1] == "test" && r.Method == http.MethodPost {
		// A connection test verifies the credential is present, decryptable
		// and shaped for its auth type. It deliberately makes NO network
		// call — the gateway never executes external requests; a full
		// round-trip test happens through an integration's own /test run.
		authType, _, secret, oerr := st.OpenCredential(ctx, appID, id)
		if oerr != nil {
			jsonOK(w, map[string]any{"ok": false, "error": "credential cannot be opened (missing key or corrupt payload)"})
			return
		}
		if authType != "none" && len(secret) == 0 {
			jsonOK(w, map[string]any{"ok": false, "error": "no credential stored"})
			return
		}
		var doc map[string]any
		if len(secret) > 0 && json.Unmarshal(secret, &doc) != nil {
			jsonOK(w, map[string]any{"ok": false, "error": "credential payload is not a JSON object"})
			return
		}
		need := map[string][]string{
			"api_key": {"value"}, "bearer": {"token"}, "basic": {"username", "password"},
			"oauth2_client_credentials": {"client_id", "client_secret", "token_url"},
		}
		for _, k := range need[authType] {
			if k == "token_url" {
				continue // lives in public meta
			}
			if v, ok := doc[k].(string); !ok || v == "" {
				jsonOK(w, map[string]any{"ok": false, "error": fmt.Sprintf("credential is missing %q", k)})
				return
			}
		}
		jsonOK(w, map[string]any{"ok": true})
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Name     string          `json:"name"`
			AuthType string          `json:"auth_type"`
			Meta     json.RawMessage `json:"meta"`
			// Secret: absent=Keep, null=Remove, object=Replace.
			Secret *json.RawMessage `json:"secret,omitempty"`
		}
		raw, _ := readAll(r)
		if err := json.Unmarshal(raw, &body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		var secret []byte // nil = keep
		if hasKey(raw, "secret") {
			if body.Secret == nil || string(*body.Secret) == "null" {
				secret = []byte{} // remove
			} else {
				secret = []byte(*body.Secret)
			}
		}
		conn, uerr := st.UpdateConnection(ctx, appID, id, body.Name, body.AuthType, body.Meta, secret)
		if uerr != nil {
			code := http.StatusBadRequest
			if errors.Is(uerr, pgx.ErrNoRows) {
				code = http.StatusNotFound
			}
			jsonErr(w, uerr, code)
			return
		}
		h.auditIntegrationEvent(r, appID, id, auditlog.EventIntegrationUpdated, map[string]string{"kind": "connection"})
		jsonOK(w, conn)
	case http.MethodDelete:
		if derr := st.DeleteConnection(ctx, appID, id); derr != nil {
			code := http.StatusBadRequest
			if errors.Is(derr, pgx.ErrNoRows) {
				code = http.StatusNotFound
			}
			jsonErr(w, derr, code)
			return
		}
		h.auditIntegrationEvent(r, appID, id, auditlog.EventIntegrationDeleted, map[string]string{"kind": "connection"})
		jsonOK(w, map[string]string{"status": "deleted"})
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close() //nolint:errcheck
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if len(buf) > 1<<20 {
				return nil, fmt.Errorf("body too large")
			}
			return buf, nil
		}
		if len(buf) > 1<<20 {
			return nil, fmt.Errorf("body too large")
		}
	}
}

func hasKey(raw []byte, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// restAPIListRuns serves GET {id}/runs for rest_api integrations with the
// full extended run shape.
func (h *handler) restAPIListRuns(w http.ResponseWriter, r *http.Request, id string) {
	runs, err := h.intStore(r.Context()).ListRuns(r.Context(), id, 50)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, runs)
}

// restAPIRunEnqueue is the rest_api branch of POST /api/integrations/{id}/run:
// 202 + queued run instead of synchronous execution. Business users reach it
// only through a role-granted dashboard integration_button; the shared access
// check in integrationRun already ran.
func (h *handler) restAPIRunEnqueue(w http.ResponseWriter, r *http.Request, id string) {
	var modelID string
	if err := h.db.QueryRow(r.Context(), `SELECT model_id::text FROM model.integration_def WHERE id=$1::uuid`, id).Scan(&modelID); err != nil {
		jsonErr(w, fmt.Errorf("integration not found"), http.StatusNotFound)
		return
	}
	h.restAPIEnqueue(w, r, modelID, id, "manual", false)
}
