package aiassistant

// Connectors, sign-in connections, tests, runs and rule triggers: what a
// developer does in the Integrations tab's wizard and with its Test and
// Run now buttons, and with a rule's manual trigger. The assistant does it
// through the same gateway code (Hooks, ReadHooks), as the developer the
// chat belongs to, so it is held to exactly what that developer is: the
// same validation, ownership and model-link checks, the same run queue, the
// same audit.
//
// A secret never passes through the assistant. A sign-in connection's
// public parts (a user name, a client id, a token URL) are in its proposal;
// its secret parts are typed by the developer into the confirmation card,
// which hands them to the gateway with the confirmation (SecretFields).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/internal/manuals"
)

// ConnectorSave is a create_api_integration or update_api_integration step,
// its references resolved, for Hooks.SaveConnector. IntegrationID "" creates
// (always as a draft: a connector is activated once a test passed).
type ConnectorSave struct {
	IntegrationID string
	Name          string
	Description   *string
	Tags          []string
	Status        string
	Enabled       *bool
	ConnectionID  *string
	Config        *integration.Config
	Schedule      *integration.Schedule
}

// ConnectionSave is a create_connection or update_connection step for
// Hooks.SaveConnection. Credential holds the public parts of the credential
// the proposal gave (PlainFields); the hook adds what the developer typed
// into the confirmation card. ReplaceCredential on an update asks the card
// for the whole credential again; without it the stored one is kept.
type ConnectionSave struct {
	ConnectionID      string
	Name              string
	AuthType          string
	Meta              map[string]string
	Credential        map[string]string
	ReplaceCredential bool
}

// SecretFields are each sign-in type's credential parts the developer types
// into the confirmation card — never the assistant. Optional ones may be
// left empty.
var SecretFields = map[string][]string{
	"api_key":                   {"value"},
	"bearer":                    {"token"},
	"basic":                     {"password"},
	"oauth2_client_credentials": {"client_secret"},
	"oauth2_authorization_code": {"client_secret"},
	"ssh_key":                   {"private_key", "passphrase"},
}

// OptionalSecretFields may be left empty in the card.
var OptionalSecretFields = map[string]bool{"passphrase": true}

// PlainFields are the public parts of a credential the proposal may carry.
var PlainFields = map[string][]string{
	"basic":                     {"username"},
	"oauth2_client_credentials": {"client_id"},
	"ssh_key":                   {"username"},
}

// metaFields are a connection's settings beside its credential.
var metaFields = map[string][]string{
	"oauth2_client_credentials": {"token_url", "scope"},
	"oauth2_authorization_code": {"authorization_url", "token_url", "client_id", "scope", "token_client_auth"},
}

func connectorToolDefs() []toolDef {
	return []toolDef{
		{
			Name: "get_integration",
			Description: "Returns one integration in full: a REST API connector's typed config (protocol, request or SFTP file or source model grid, auth placement, response, pagination, mapping, limits), its sign-in connection, schedule, status, whether its current config is tested, both switches of a model link, and its last runs with their errors. " +
				"Call it before update_api_integration (whose config replaces the stored one) and to explain why a run failed.",
			Parameters: `{"type":"object","properties":{"integration_id":{"type":"string","description":"id or exact name"}},"required":["integration_id"]}`,
		},
		{
			Name:        "list_integration_runs",
			Description: "Returns an integration's run history, newest first: trigger, status, error code and message, records read/written/skipped, duration and what the run read (host, file, source model). Read-only.",
			Parameters:  `{"type":"object","properties":{"integration_id":{"type":"string","description":"id or exact name"},"limit":{"type":"integer","description":"default 10, at most 50"}},"required":["integration_id"]}`,
		},
		{
			Name: "test_integration",
			Description: "Tests a SAVED REST API connector now, exactly as the developer's Test button does: the backend makes the GET request, reads the SFTP file or reads the source model's grid, writes nothing, and reports the status, the error if any, and the first records with their fields — what the mapping must name. A passing test is what activation needs. " +
				"Only for tests without side effects (a GET request, an SFTP file, a model link); a POST/PUT/PATCH/DELETE request's test may change the external system, so propose run_integration with mode \"test\" for the developer to confirm instead.",
			Parameters: `{"type":"object","properties":{"integration_id":{"type":"string","description":"id or exact name"}},"required":["integration_id"]}`,
		},
		{
			Name:        "list_model_link_sources",
			Description: "Returns the models a model link in this model may read: the other models of this tenant the developer is a developer of, each with its active revision's grids, their metrics and dimensions. With model_id and grid, that grid's dimensions with their member codes (for a link's filters).",
			Parameters:  `{"type":"object","properties":{"model_id":{"type":"string"},"grid":{"type":"string"}},"required":[]}`,
		},
		{
			Name:        "list_connections",
			Description: "Returns this application's sign-in connections for REST API and SFTP connectors: id, name, sign-in type, public settings, and whether a credential is stored. Secrets are never shown.",
			Parameters:  `{"type":"object","properties":{},"required":[]}`,
		},
		{
			Name: "read_manual",
			Description: "Searches the platform's Developer manual and Formulas manual and returns the best-matching sections as text. Use it to explain how the platform works — concepts, roles, revisions, grids, dashboards, workflows and triggers, integrations, formulas — from what the documentation says. " +
				"With no query, returns the manuals' contents.",
			Parameters: `{"type":"object","properties":{"query":{"type":"string","description":"what to look up, in a few words"},"manual":{"type":"string","enum":["developer","formulas"],"description":"omit to search both"}},"required":[]}`,
		},
	}
}

// ── Write tools ───────────────────────────────────────────────────────────────

type apiIntegrationParams struct {
	IntegrationID string                `json:"integration_id"`
	Name          *string               `json:"name"`
	Description   *string               `json:"description"`
	Tags          *[]string             `json:"tags"`
	Status        string                `json:"status"`
	Enabled       *bool                 `json:"enabled"`
	ConnectionID  *string               `json:"connection_id"`
	Config        json.RawMessage       `json:"config"`
	Schedule      *integration.Schedule `json:"schedule"`
}

// createAPIIntegration is create_api_integration: a REST API connector — an
// HTTPS API, an SFTP file or a model link — saved as a draft.
func (e *WriteExecutor) createAPIIntegration(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p apiIntegrationParams
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.IntegrationID != "" || p.Status != "" || p.Enabled != nil {
		return "", "", fmt.Errorf("create_api_integration saves a new draft; activate or switch it with update_api_integration once a test passed")
	}
	if p.Name == nil || strings.TrimSpace(*p.Name) == "" {
		return "", "", fmt.Errorf("name is required")
	}
	name := strings.TrimSpace(*p.Name)
	if err := e.integrationNameFree(ctx, name, ""); err != nil {
		return "", "", err
	}
	if len(p.Config) == 0 {
		return "", "", fmt.Errorf("config is required (see the REST API connectors section of your instructions)")
	}
	cfg, err := e.connectorConfig(ctx, p.Config)
	if err != nil {
		return "", "", err
	}
	req := ConnectorSave{Name: name, Description: p.Description, Config: cfg, Schedule: p.Schedule}
	if p.Tags != nil {
		req.Tags = *p.Tags
	}
	if p.ConnectionID != nil {
		id, err := e.resolveConnection(ctx, *p.ConnectionID)
		if err != nil {
			return "", "", err
		}
		req.ConnectionID = &id
	}
	if e.hooks.SaveConnector == nil {
		return "", "", fmt.Errorf("REST API connectors are saved through the gateway, which is not available here")
	}
	id, err := e.hooks.SaveConnector(ctx, req)
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("REST API connector '%s' saved as a draft (id: %s): %s into %s. Test it with test_integration, then activate it with update_api_integration.",
		name, id, describeSource(cfg), cfg.TargetType), id, nil
}

// updateAPIIntegration is update_api_integration.
func (e *WriteExecutor) updateAPIIntegration(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p apiIntegrationParams
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	row, err := e.loadIntegrationDef(ctx, p.IntegrationID)
	if err != nil {
		return "", "", err
	}
	if row.Type != "rest_api" {
		return "", "", fmt.Errorf("%q is a %s integration — update_integration changes it", row.Name, row.Type)
	}
	switch p.Status {
	case "", "draft", "active":
	default:
		return "", "", fmt.Errorf("status is draft or active")
	}
	req := ConnectorSave{IntegrationID: row.ID, Description: p.Description, Status: p.Status, Enabled: p.Enabled, Schedule: p.Schedule}
	if p.Name != nil {
		req.Name = strings.TrimSpace(*p.Name)
		if req.Name == "" {
			return "", "", fmt.Errorf("name cannot be empty")
		}
		if err := e.integrationNameFree(ctx, req.Name, row.ID); err != nil {
			return "", "", err
		}
	}
	if p.Tags != nil {
		req.Tags = *p.Tags
	}
	if len(p.Config) > 0 {
		if req.Config, err = e.connectorConfig(ctx, p.Config); err != nil {
			return "", "", err
		}
		// Values get_integration hid come back as the placeholder: keep
		// the stored ones.
		var stored integration.Config
		if json.Unmarshal(row.Config, &stored) == nil {
			restoreHidden(req.Config, &stored)
		}
	}
	if p.ConnectionID != nil {
		id := ""
		if strings.TrimSpace(*p.ConnectionID) != "" {
			if id, err = e.resolveConnection(ctx, *p.ConnectionID); err != nil {
				return "", "", err
			}
		}
		req.ConnectionID = &id
	}
	if e.hooks.SaveConnector == nil {
		return "", "", fmt.Errorf("REST API connectors are saved through the gateway, which is not available here")
	}
	if _, err := e.hooks.SaveConnector(ctx, req); err != nil {
		return "", "", err
	}
	var changed []string
	if req.Config != nil {
		changed = append(changed, "config")
	}
	if p.Status != "" {
		changed = append(changed, "status "+p.Status)
	}
	if p.Enabled != nil {
		changed = append(changed, fmt.Sprintf("switched %s", map[bool]string{true: "on", false: "off"}[*p.Enabled]))
	}
	if p.Schedule != nil {
		changed = append(changed, "schedule")
	}
	if p.Name != nil || p.Tags != nil || p.Description != nil || p.ConnectionID != nil {
		changed = append(changed, "details")
	}
	return fmt.Sprintf("REST API connector '%s' updated: %s", row.Name, strings.Join(changed, ", ")), row.ID, nil
}

// connectorConfig decodes a connector's config, strictly, and resolves the
// names in it: its target, and a model link's source model.
func (e *WriteExecutor) connectorConfig(ctx context.Context, raw json.RawMessage) (*integration.Config, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("config must be an object: %w", err)
	}
	if _, ok := doc["kind"]; !ok {
		doc["kind"] = json.RawMessage(`"rest_api/v1"`)
	}
	if v, ok := doc["protocol"]; ok && (string(v) == `"https"` || string(v) == `"http"`) {
		doc["protocol"] = json.RawMessage(`""`)
	}
	norm, _ := json.Marshal(doc)
	var cfg integration.Config
	if err := decodeParams(norm, &cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if cfg.TargetType == "" {
		cfg.TargetType = integration.TargetGrid
	}
	if cfg.TargetID != "" {
		id, err := e.requireInModel(ctx, string(cfg.TargetType), cfg.TargetID)
		if err != nil {
			return nil, fmt.Errorf("config.target_id: %w", err)
		}
		cfg.TargetID = id
	}
	if cfg.Protocol == integration.ProtocolModel && cfg.Model != nil && cfg.Model.ModelID != "" && !uuidShaped(cfg.Model.ModelID) {
		id, err := e.resolveTenantModel(ctx, cfg.Model.ModelID)
		if err != nil {
			return nil, fmt.Errorf("config.model.model_id: %w", err)
		}
		cfg.Model.ModelID = id
	}
	return &cfg, nil
}

// resolveTenantModel finds a model of this model's tenant by name, or by
// "Application › Model".
func (e *WriteExecutor) resolveTenantModel(ctx context.Context, ref string) (string, error) {
	appName, modelName := "", strings.TrimSpace(ref)
	if i := strings.Index(ref, "›"); i >= 0 {
		appName, modelName = strings.TrimSpace(ref[:i]), strings.TrimSpace(ref[i+len("›"):])
	}
	rows, err := e.pool.Query(ctx, `
		SELECT sm.id::text, sapp.name || ' › ' || sm.name
		FROM core.model tm
		JOIN core.application tapp ON tapp.id = tm.application_id
		JOIN core.application sapp ON sapp.customer_id = tapp.customer_id
		JOIN core.model sm ON sm.application_id = sapp.id
		WHERE tm.id = $1::uuid AND lower(sm.name) = lower($2) AND ($3 = '' OR lower(sapp.name) = lower($3))`,
		e.modelID, modelName, appName)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids, names []string
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return "", err
		}
		ids, names = append(ids, id), append(names, name)
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("no model of this tenant is named %q — list_model_link_sources lists the ones a link may read", ref)
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("%d models are named %q (%s) — pass its id or \"Application › Model\"", len(ids), ref, strings.Join(names, "; "))
}

// resolveConnection finds a sign-in connection of this model's application
// by id or exact name.
func (e *WriteExecutor) resolveConnection(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	var id string
	err := e.pool.QueryRow(ctx, `
		SELECT c.id::text FROM model.integration_connection c
		JOIN core.model m ON m.application_id = c.application_id
		WHERE m.id = $1::uuid AND (c.id::text = $2 OR lower(c.name) = lower($2))
		ORDER BY (c.id::text = $2) DESC LIMIT 1`, e.modelID, ref).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("no sign-in connection %q in this application — list_connections lists them, or propose create_connection", ref)
	}
	return id, nil
}

// describeSource is what a connector reads, in a few words.
func describeSource(cfg *integration.Config) string {
	switch cfg.Protocol {
	case integration.ProtocolSFTP:
		if cfg.SFTP != nil {
			return "an SFTP file on " + cfg.SFTP.Host
		}
		return "an SFTP file"
	case integration.ProtocolModel:
		if cfg.Model != nil {
			return fmt.Sprintf("grid %q of another model", cfg.Model.Grid)
		}
		return "another model's grid"
	case integration.ProtocolHTTPS:
	}
	if cfg.Direction == integration.DirectionPush {
		return "a push to " + integration.SanitizedHost(cfg.Request.URL)
	}
	return cfg.Request.Method + " " + integration.SanitizedHost(cfg.Request.URL)
}

type connectionParams struct {
	ConnectionID      string            `json:"connection_id"`
	Name              string            `json:"name"`
	AuthType          string            `json:"auth_type"`
	Meta              map[string]string `json:"meta"`
	Credential        map[string]string `json:"credential"`
	ReplaceCredential bool              `json:"replace_credential"`
}

// checkConnectionParams refuses a secret in a proposal and settings a
// sign-in type does not have.
func checkConnectionParams(authType string, p connectionParams) error {
	for k := range p.Credential {
		for _, s := range SecretFields[authType] {
			if k == s {
				return fmt.Errorf("never put a secret into a proposal: leave credential.%s out — the developer types it into the confirmation card, where it goes to the server and not into this chat", k)
			}
		}
		if !containsString(PlainFields[authType], k) {
			return fmt.Errorf("credential.%s is not a public part of a %s sign-in (those are: %s)", k, authType, orNone(PlainFields[authType]))
		}
	}
	for k := range p.Meta {
		if !containsString(metaFields[authType], k) {
			return fmt.Errorf("meta.%s is not a setting of a %s sign-in (those are: %s)", k, authType, orNone(metaFields[authType]))
		}
	}
	return nil
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}

// createConnection is create_connection.
func (e *WriteExecutor) createConnection(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p connectionParams
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if p.ConnectionID != "" || p.ReplaceCredential {
		return "", "", fmt.Errorf("create_connection makes a new connection; update_connection changes one")
	}
	if strings.TrimSpace(p.Name) == "" {
		return "", "", fmt.Errorf("name is required")
	}
	if _, ok := SecretFields[p.AuthType]; !ok && p.AuthType != "none" {
		return "", "", fmt.Errorf("auth_type must be none, api_key, bearer, basic, oauth2_client_credentials, oauth2_authorization_code or ssh_key")
	}
	if err := checkConnectionParams(p.AuthType, p); err != nil {
		return "", "", err
	}
	if e.hooks.SaveConnection == nil {
		return "", "", fmt.Errorf("sign-in connections are saved through the gateway, which is not available here")
	}
	id, err := e.hooks.SaveConnection(ctx, ConnectionSave{Name: strings.TrimSpace(p.Name), AuthType: p.AuthType, Meta: p.Meta, Credential: p.Credential})
	if err != nil {
		return "", "", err
	}
	next := ""
	if p.AuthType == "oauth2_authorization_code" {
		next = " The developer still has to click Connect on it (Integrations › its connection) to grant access in the provider's consent page."
	}
	return fmt.Sprintf("Sign-in connection '%s' (%s) created (id: %s).%s", p.Name, p.AuthType, id, next), id, nil
}

// updateConnection is update_connection.
func (e *WriteExecutor) updateConnection(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p connectionParams
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	id, err := e.resolveConnection(ctx, p.ConnectionID)
	if err != nil {
		return "", "", err
	}
	authType := p.AuthType
	if authType == "" {
		_ = e.pool.QueryRow(ctx, `SELECT auth_type FROM model.integration_connection WHERE id=$1::uuid`, id).Scan(&authType)
	}
	if len(p.Credential) > 0 && !p.ReplaceCredential {
		return "", "", fmt.Errorf("credential is only sent with replace_credential: true, which asks the developer for the whole credential again")
	}
	if err := checkConnectionParams(authType, p); err != nil {
		return "", "", err
	}
	if e.hooks.SaveConnection == nil {
		return "", "", fmt.Errorf("sign-in connections are saved through the gateway, which is not available here")
	}
	if _, err := e.hooks.SaveConnection(ctx, ConnectionSave{ConnectionID: id, Name: strings.TrimSpace(p.Name), AuthType: p.AuthType,
		Meta: p.Meta, Credential: p.Credential, ReplaceCredential: p.ReplaceCredential}); err != nil {
		return "", "", err
	}
	what := "settings"
	if p.ReplaceCredential {
		what = "settings and credential"
	}
	return fmt.Sprintf("Sign-in connection %s: %s updated.", id, what), id, nil
}

// runIntegration is run_integration: Run now (mode run), a test that may
// change the external system (mode test), or a dry run that maps without
// writing (mode dry_run) — done as the developer's buttons do it, after
// the developer confirms.
func (e *WriteExecutor) runIntegration(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		IntegrationID string `json:"integration_id"`
		Mode          string `json:"mode"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	row, err := e.loadIntegrationDef(ctx, p.IntegrationID)
	if err != nil {
		return "", "", err
	}
	switch p.Mode {
	case "":
		p.Mode = "run"
	case "run", "test", "dry_run":
	default:
		return "", "", fmt.Errorf("mode is run, test or dry_run")
	}
	if p.Mode != "run" && row.Type != "rest_api" {
		return "", "", fmt.Errorf("only a REST API connector has a test or dry run")
	}
	switch row.Type {
	case "rest_api", "google_sheets":
	case "csv_import":
		return "", "", fmt.Errorf("an Excel/CSV integration runs on a file: import an attached file with import_file_data")
	default:
		return "", "", fmt.Errorf("a %s integration is not run", row.Type)
	}
	if e.hooks.RunIntegration == nil {
		return "", "", fmt.Errorf("integrations are run through the gateway, which is not available here")
	}
	out, err := e.hooks.RunIntegration(ctx, row.ID, p.Mode)
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("%s of '%s': %s", map[string]string{"run": "Run", "test": "Test", "dry_run": "Dry run"}[p.Mode], row.Name, out), row.ID, nil
}

// triggerAutomationRule is trigger_automation_rule: the rule's manual
// trigger, as the developer fires it.
func (e *WriteExecutor) triggerAutomationRule(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		RuleID  string         `json:"rule_id"`
		Payload map[string]any `json:"payload"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if strings.TrimSpace(p.RuleID) == "" {
		return "", "", fmt.Errorf("rule_id is required (list_automation_rules)")
	}
	if e.hooks.TriggerRule == nil {
		return "", "", fmt.Errorf("rules are triggered through the gateway, which is not available here")
	}
	out, err := e.hooks.TriggerRule(ctx, strings.TrimSpace(p.RuleID), p.Payload)
	if err != nil {
		return "", "", err
	}
	return out, p.RuleID, nil
}

// ── Read tools ────────────────────────────────────────────────────────────────

// readIntegrationRef resolves an integration of this model by id or exact
// name in the working revision.
func (e *ToolExecutor) readIntegrationRef(ctx context.Context, ref string) (id, name, typ string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", "", fmt.Errorf("integration_id is required")
	}
	err = e.pool.QueryRow(ctx, `
		SELECT id::text, name, type FROM model.integration_def
		WHERE model_id = $1::uuid AND (id::text = $2 OR (lower(name) = lower($2) AND revision_id IS NOT DISTINCT FROM NULLIF($3,'')::uuid))
		ORDER BY (id::text = $2) DESC LIMIT 1`, e.modelID, ref, e.revID).Scan(&id, &name, &typ)
	if err != nil {
		return "", "", "", fmt.Errorf("integration %q not found in this model — list_integrations lists them", ref)
	}
	return id, name, typ, nil
}

func (e *ToolExecutor) getIntegration(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		IntegrationID string `json:"integration_id"`
	}
	_ = json.Unmarshal(args, &p)
	id, _, typ, err := e.readIntegrationRef(ctx, p.IntegrationID)
	if err != nil {
		return "", err
	}
	if typ != "rest_api" {
		return "", fmt.Errorf("a %s integration: list_integrations shows its whole configuration", typ)
	}
	var modelID string
	_ = e.pool.QueryRow(ctx, `SELECT model_id::text FROM model.integration_def WHERE id=$1::uuid`, id).Scan(&modelID)
	st := integration.NewStore(e.pool)
	def, err := st.GetDefinition(ctx, modelID, id)
	if err != nil {
		return "", err
	}
	sched, _ := st.GetSchedule(ctx, id)
	var conn string
	if def.ConnectionID != "" {
		_ = e.pool.QueryRow(ctx, `SELECT name || ' (' || auth_type || CASE WHEN secret_enc IS NULL OR secret_enc = '' THEN ', no credential stored' ELSE '' END || ')' FROM model.integration_connection WHERE id=$1::uuid`, def.ConnectionID).Scan(&conn)
	}
	if def.Config != nil {
		cfg := *def.Config
		hideSensitive(&cfg)
		def.Config = &cfg
	}
	out := map[string]any{
		"id": def.ID, "name": def.Name, "description": def.Description, "status": def.Status, "enabled": def.Enabled,
		"tags": def.Tags, "config": def.Config, "tested": def.Tested(), "connection_id": def.ConnectionID, "connection": conn,
		"schedule": sched,
	}
	if def.Config != nil && def.Config.Protocol == integration.ProtocolModel {
		out["source_side_enabled"] = def.SourceEnabled
	}
	runs, _ := st.ListRuns(ctx, id, 5)
	out["last_runs"] = summarizeRuns(runs)
	b, _ := json.MarshalIndent(out, "", " ")
	return string(b), nil
}

func (e *ToolExecutor) listIntegrationRuns(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		IntegrationID string `json:"integration_id"`
		Limit         int    `json:"limit"`
	}
	_ = json.Unmarshal(args, &p)
	id, name, _, err := e.readIntegrationRef(ctx, p.IntegrationID)
	if err != nil {
		return "", err
	}
	if p.Limit <= 0 || p.Limit > 50 {
		p.Limit = 10
	}
	runs, err := integration.NewStore(e.pool).ListRuns(ctx, id, p.Limit)
	if err != nil {
		return "", err
	}
	if len(runs) == 0 {
		return fmt.Sprintf("'%s' has not run yet.", name), nil
	}
	b, _ := json.MarshalIndent(summarizeRuns(runs), "", " ")
	return string(b), nil
}

// summarizeRuns is what a run says, without its raw preview body.
func summarizeRuns(runs []integration.Run) []map[string]any {
	out := make([]map[string]any, 0, len(runs))
	for _, r := range runs {
		m := map[string]any{"trigger": r.TriggerType, "status": r.Status, "created_at": r.CreatedAt,
			"records_read": r.RecordsRead, "records_written": r.RecordsWritten, "records_skipped": r.RecordsSkipped,
			"duration_ms": r.DurationMS}
		if r.DryRun {
			m["dry_run"] = true
		}
		if r.ErrorCode != "" {
			m["error_code"] = r.ErrorCode
		}
		if r.Message != "" {
			m["message"] = r.Message
		}
		var meta map[string]string
		if json.Unmarshal(r.Meta, &meta) == nil {
			for _, k := range []string{"host", "file", "source_model", "source_revision", "source_grid", "unchanged", "stopped"} {
				if meta[k] != "" {
					m[k] = meta[k]
				}
			}
		}
		out = append(out, m)
	}
	return out
}

func (e *ToolExecutor) testIntegration(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		IntegrationID string `json:"integration_id"`
	}
	_ = json.Unmarshal(args, &p)
	id, _, typ, err := e.readIntegrationRef(ctx, p.IntegrationID)
	if err != nil {
		return "", err
	}
	if typ != "rest_api" {
		return "", fmt.Errorf("only a REST API connector has a test; a file integration is previewed with preview_file_import")
	}
	if e.hooks.TestIntegration == nil {
		return "", fmt.Errorf("tests run through the gateway, which is not available here")
	}
	return e.hooks.TestIntegration(ctx, id)
}

func (e *ToolExecutor) listModelLinkSources(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		ModelID string `json:"model_id"`
		Grid    string `json:"grid"`
	}
	_ = json.Unmarshal(args, &p)
	if e.hooks.ModelLinkSources == nil {
		return "", fmt.Errorf("model link sources are read through the gateway, which is not available here")
	}
	return e.hooks.ModelLinkSources(ctx, p.ModelID, p.Grid)
}

func (e *ToolExecutor) listConnections(ctx context.Context) (string, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT c.id::text, c.name, c.auth_type, c.meta::text, (c.secret_enc IS NOT NULL AND c.secret_enc <> '')
		FROM model.integration_connection c JOIN core.model m ON m.application_id = c.application_id
		WHERE m.id = $1::uuid ORDER BY c.name`, e.modelID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, name, authType, meta string
		var has bool
		if err := rows.Scan(&id, &name, &authType, &meta, &has); err != nil {
			return "", err
		}
		state := "credential stored"
		if !has {
			state = "NO credential stored"
		}
		fmt.Fprintf(&b, "- %s (id %s): %s, %s, settings %s\n", name, id, authType, state, meta)
	}
	if b.Len() == 0 {
		return "No sign-in connections in this application yet (create_connection makes one).", nil
	}
	return b.String(), nil
}

func (e *ToolExecutor) readManual(_ context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Query  string `json:"query"`
		Manual string `json:"manual"`
	}
	_ = json.Unmarshal(args, &p)
	if strings.TrimSpace(p.Query) == "" {
		return manuals.Contents(p.Manual), nil
	}
	hits := manuals.Search(p.Query, p.Manual, 9000)
	if len(hits) == 0 {
		return fmt.Sprintf("Nothing in the manuals matches %q. Their contents:\n%s", p.Query, manuals.Contents(p.Manual)), nil
	}
	var b strings.Builder
	for _, s := range hits {
		title := s.Chapter
		if s.Title != s.Chapter {
			title += " › " + s.Title
		}
		fmt.Fprintf(&b, "### %s manual — %s\n%s\n\n", s.Manual, title, s.Text)
	}
	return strings.TrimSpace(b.String()), nil
}

// hiddenValue stands for a request value get_integration does not show.
const hiddenValue = "[hidden]"

// sensitiveKey is a header or parameter name that usually carries a secret.
func sensitiveKey(k string) bool {
	k = strings.ToLower(k)
	for _, w := range []string{"auth", "token", "key", "secret", "password", "passwd", "cookie", "session", "signature"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// hideSensitive replaces the values of a request's secret-looking headers,
// query parameters and form fields with hiddenValue: a credential typed
// into the request instead of a connection must not reach the assistant's
// provider. The copy's slices are its own.
func hideSensitive(c *integration.Config) {
	hide := func(kvs []integration.KV) []integration.KV {
		out := make([]integration.KV, len(kvs))
		for i, kv := range kvs {
			if sensitiveKey(kv.Key) && kv.Value != "" {
				kv.Value = hiddenValue
			}
			out[i] = kv
		}
		return out
	}
	c.Request.Headers = hide(c.Request.Headers)
	c.Request.Query = hide(c.Request.Query)
	c.Request.BodyForm = hide(c.Request.BodyForm)
}

// restoreHidden puts back the stored value of every request value next
// still holds as hiddenValue.
func restoreHidden(next, stored *integration.Config) {
	restore := func(kvs, was []integration.KV) {
		for i := range kvs {
			if kvs[i].Value != hiddenValue {
				continue
			}
			for _, w := range was {
				if strings.EqualFold(w.Key, kvs[i].Key) {
					kvs[i].Value = w.Value
					break
				}
			}
		}
	}
	restore(next.Request.Headers, stored.Request.Headers)
	restore(next.Request.Query, stored.Request.Query)
	restore(next.Request.BodyForm, stored.Request.BodyForm)
}
