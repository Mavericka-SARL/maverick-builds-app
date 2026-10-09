package gateway

// The AI Developer's integrations: connectors, sign-in connections, tests,
// runs and rule triggers (internal/aiassistant/tools_connector.go). Saving a
// connector is restAPISave, the wizard's own save; a test, a run and a
// trigger are the developer's own routes, called in-process as the developer
// behind the chat request (callAsCaller) — the same guard, the same checks
// and the same run queue as their click. A secret reaches a connection from
// the confirmation card only, never from the assistant.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// callAsCaller runs one of the gateway's own routes in-process as the
// person behind r: r's identity headers, tenant routing and application go
// with it, and the route's guard and checks run as for that person's own
// request. It answers the status and body.
func (h *handler) callAsCaller(ctx context.Context, r *http.Request, method, path string, query url.Values, body any) (int, []byte) {
	req := r.Clone(ctx)
	req.Method = method
	req.URL = &url.URL{Path: path}
	if query != nil {
		req.URL.RawQuery = query.Encode()
	}
	req.RequestURI = ""
	req.Body, req.ContentLength = http.NoBody, 0
	if body != nil {
		b, _ := json.Marshal(body)
		req.Body = noCloseReader{bytes.NewReader(b)}
		req.ContentLength = int64(len(b))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

type noCloseReader struct{ *bytes.Reader }

func (noCloseReader) Close() error { return nil }

// routeError is a route's refusal as an error.
func routeError(status int, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Error == "" {
		e.Error = http.StatusText(status)
	}
	return errors.New(e.Error)
}

// awaitRun waits up to limit for a queued run to finish.
func (h *handler) awaitRun(ctx context.Context, runID string, limit time.Duration) (*integration.Run, error) {
	deadline := time.Now().Add(limit)
	for {
		run, err := h.intStore(ctx).GetRun(ctx, runID)
		if err != nil {
			return nil, err
		}
		if run.Status != "queued" && run.Status != "running" {
			return run, nil
		}
		if time.Now().After(deadline) {
			return run, nil
		}
		select {
		case <-ctx.Done():
			return run, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// describeRun is a run's outcome for the assistant: what happened, what
// was read and written, and — for a test — the first records and their
// fields, which a mapping names.
func describeRun(run *integration.Run, withPreview bool) string {
	var b strings.Builder
	if run.Status == "queued" || run.Status == "running" {
		fmt.Fprintf(&b, "still %s (run %s): no runner has finished it yet — list_integration_runs shows it once it has", run.Status, run.ID)
		return b.String()
	}
	fmt.Fprintf(&b, "%s", run.Status)
	if run.ErrorCode != "" {
		fmt.Fprintf(&b, " (%s)", run.ErrorCode)
	}
	if run.Message != "" {
		fmt.Fprintf(&b, ": %s", run.Message)
	}
	fmt.Fprintf(&b, "; %d record(s) read, %d written, %d skipped", run.RecordsRead, run.RecordsWritten, run.RecordsSkipped)
	var meta map[string]string
	_ = json.Unmarshal(run.Meta, &meta)
	for _, k := range []string{"host", "file", "source_model", "source_revision", "source_grid", "source_warnings", "unchanged", "stopped", "preview_status"} {
		if meta[k] != "" {
			fmt.Fprintf(&b, "; %s: %s", k, meta[k])
		}
	}
	if meta["host_key"] != "" {
		fmt.Fprintf(&b, "; the SFTP server presented host key %s (fingerprint %s), which is not trusted: show the developer the fingerprint to compare with the server's, and only if they confirm it is theirs propose config.sftp.host_key = that key line",
			meta["host_key"], meta["host_key_fingerprint"])
	}
	if withPreview && meta["preview_body"] != "" {
		preview := meta["preview_body"]
		if len(preview) > 4000 {
			preview = preview[:4000] + " …"
		}
		fmt.Fprintf(&b, "\nFirst records (what the mapping's sources name, as $.field):\n%s", preview)
	}
	return b.String()
}

// aiTestIntegration is the test_integration read tool: the developer's own
// Test route, for a test without side effects only.
func (h *handler) aiTestIntegration(ctx context.Context, r *http.Request, modelID, id string) (string, error) {
	def, err := h.intStore(ctx).GetDefinition(ctx, modelID, id)
	if err != nil {
		return "", fmt.Errorf("REST API connector not found in this model")
	}
	if def.Config != nil && def.Config.Protocol == integration.ProtocolHTTPS && def.Config.Request.Method != http.MethodGet {
		return "", fmt.Errorf("a test of this %s request may change the external system — propose run_integration with mode \"test\" for the developer to confirm", def.Config.Request.Method)
	}
	status, body := h.callAsCaller(ctx, r, http.MethodPost, "/api/developer/integrations/"+id+"/test", nil, nil)
	if status != http.StatusAccepted {
		return "", routeError(status, body)
	}
	var q struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(body, &q)
	run, err := h.awaitRun(ctx, q.RunID, 2*time.Minute)
	if err != nil {
		return "", err
	}
	return "Test " + describeRun(run, true), nil
}

// aiRunIntegration is run_integration: the developer's own Run now route,
// or their Test route acknowledging side effects, or a dry run.
func (h *handler) aiRunIntegration(ctx context.Context, r *http.Request, id, mode string) (string, error) {
	var status int
	var body []byte
	limit := 10 * time.Minute
	switch mode {
	case "test":
		status, body = h.callAsCaller(ctx, r, http.MethodPost, "/api/developer/integrations/"+id+"/test", url.Values{"acknowledge_side_effects": {"1"}}, nil)
		limit = 2 * time.Minute
	case "dry_run":
		status, body = h.callAsCaller(ctx, r, http.MethodPost, "/api/developer/integrations/"+id+"/test", url.Values{"dry_run": {"1"}}, nil)
	default:
		status, body = h.callAsCaller(ctx, r, http.MethodPost, "/api/integrations/"+id+"/run", nil, nil)
	}
	if status >= 300 {
		return "", routeError(status, body)
	}
	var q struct {
		RunID string `json:"run_id"`
	}
	if json.Unmarshal(body, &q) != nil || q.RunID == "" {
		// A synchronous run (a Google Sheets pull) answers its result.
		return clippedJSON(body), nil
	}
	run, err := h.awaitRun(ctx, q.RunID, limit)
	if err != nil {
		return "", err
	}
	if run.Status == "failed" {
		return "", errors.New(describeRun(run, false))
	}
	return describeRun(run, mode != "run"), nil
}

// clippedJSON is a route's answer, compacted and cut for the assistant.
func clippedJSON(b []byte) string {
	s := compactJSON(b)
	if len(s) > 4000 {
		s = s[:4000] + " …"
	}
	return s
}

// aiTriggerRule is trigger_automation_rule: the rule's manual trigger
// route, as the developer.
func (h *handler) aiTriggerRule(ctx context.Context, r *http.Request, ruleID string, payload map[string]any) (string, error) {
	p := map[string]string{}
	for k, v := range payload {
		p[k] = fmt.Sprint(v)
	}
	status, body := h.callAsCaller(ctx, r, http.MethodPost, "/api/automation/trigger/"+url.PathEscape(ruleID), nil, map[string]any{"payload": p})
	if status >= 300 {
		return "", routeError(status, body)
	}
	return "Rule triggered: " + clippedJSON(body), nil
}

// aiSaveConnector is SaveConnector: restAPISave on db as a.
func (h *handler) aiSaveConnector(ctx context.Context, db dbx.DB, a *actor, modelID, revID string, req aiassistant.ConnectorSave) (string, error) {
	body := &restAPICreateBody{Name: req.Name, Description: req.Description, Tags: req.Tags, Status: req.Status,
		Enabled: req.Enabled, ConnectionID: req.ConnectionID, Config: req.Config, Schedule: req.Schedule}
	if req.IntegrationID == "" {
		body.Status = "draft"
		return h.restAPISave(ctx, db, a, modelID, revID, "", body)
	}
	return h.restAPISave(ctx, db, a, modelID, "", req.IntegrationID, body)
}

// aiSaveConnection is SaveConnection on db as a. card holds what the
// developer typed into the confirmation card for this step; nil in a
// proposal check, which saves the connection without a credential.
func (h *handler) aiSaveConnection(ctx context.Context, db dbx.DB, a *actor, modelID string, req aiassistant.ConnectionSave, card map[string]string, checking bool) (string, error) {
	var appID string
	if err := db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID); err != nil {
		return "", err
	}
	st := integration.NewStoreOn(db)
	authType := req.AuthType
	if req.ConnectionID != "" && authType == "" {
		conn, err := st.GetConnection(ctx, appID, req.ConnectionID)
		if err != nil {
			return "", fmt.Errorf("sign-in connection not found in this application")
		}
		authType = conn.AuthType
	}
	var meta json.RawMessage
	if len(req.Meta) > 0 {
		meta, _ = json.Marshal(req.Meta)
	}
	// The credential: its public parts from the card (as corrected there)
	// or the proposal, its secret parts from the card alone.
	var secret []byte
	if req.ConnectionID == "" || req.ReplaceCredential {
		cred := map[string]string{}
		for _, k := range aiassistant.PlainFields[authType] {
			if v := strings.TrimSpace(card[k]); v != "" {
				cred[k] = v
			} else if v := strings.TrimSpace(req.Credential[k]); v != "" {
				cred[k] = v
			}
		}
		for _, k := range aiassistant.SecretFields[authType] {
			if v := card[k]; v != "" {
				cred[k] = v
			} else if !checking && !aiassistant.OptionalSecretFields[k] {
				return "", fmt.Errorf("the confirmation card left %s empty — confirm again and type it there", strings.ReplaceAll(k, "_", " "))
			}
		}
		if !checking && len(cred) > 0 {
			secret, _ = json.Marshal(cred)
		}
	}
	var id string
	event := auditlog.EventIntegrationCreated
	if req.ConnectionID == "" {
		conn, err := st.CreateConnection(ctx, appID, req.Name, authType, meta, secret, a.UserID)
		if err != nil {
			return "", err
		}
		id = conn.ID
	} else {
		if _, err := st.UpdateConnection(ctx, appID, req.ConnectionID, req.Name, req.AuthType, meta, secret); err != nil {
			return "", err
		}
		id, event = req.ConnectionID, auditlog.EventIntegrationUpdated
	}
	auditlog.Log(ctx, db, h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: event,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ApplicationID: appID, ResourceType: "integration", ResourceID: id,
		Metadata: map[string]string{"kind": "connection", "auth_type": authType, "via": "ai_assistant"},
	})
	return id, nil
}

// actorByUserID resolves the account behind a user id, as its own request
// would be resolved.
func (h *handler) actorByUserID(ctx context.Context, userID string) (*actor, error) {
	var sub string
	if err := h.db.QueryRow(ctx, `SELECT COALESCE(keycloak_sub,'') FROM identity.user WHERE id=$1::uuid`, userID).Scan(&sub); err != nil || sub == "" {
		return nil, fmt.Errorf("account %s not found", userID)
	}
	return h.actorByKeycloakSub(ctx, sub)
}

// aiAttachGoogleSheet is the attach_google_sheet read tool: the Sheets
// import's own fetch route, as the developer, its CSV attached to the chat
// as an uploaded spreadsheet is.
func (h *handler) aiAttachGoogleSheet(ctx context.Context, r *http.Request, sessionID, userID, sheetURL string) (string, error) {
	status, body := h.callAsCaller(ctx, r, http.MethodPost, "/api/import/sheets/fetch", nil, map[string]string{"sheet_url": sheetURL})
	if status != http.StatusOK {
		return "", routeError(status, body)
	}
	var got struct {
		CSV           string `json:"csv"`
		SpreadsheetID string `json:"spreadsheet_id"`
		GID           string `json:"gid"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		return "", err
	}
	gid := got.GID
	if gid == "" {
		gid = "0"
	}
	id := got.SpreadsheetID
	if len(id) > 10 {
		id = id[:10]
	}
	filename := fmt.Sprintf("google-sheet-%s-%s.csv", id, gid)
	data := []byte(got.CSV)
	text, mimeType, truncated, err := aiassistant.ExtractDocumentText(filename, data)
	if err != nil {
		return "", err
	}
	var raw []byte
	if len(data) <= maxAIImportBytes {
		raw = data
	}
	doc, err := aiassistant.NewDocumentStore(h.db.For(ctx)).CreateDocument(ctx, sessionID, filename, mimeType, text, truncated, raw)
	if err != nil {
		return "", fmt.Errorf("attach the sheet: %w", err)
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAIAssistant, EventType: auditlog.EventAIDocumentUploaded,
		ActorUserID: userID, ResourceType: "ai_document", ResourceID: doc.ID,
		Metadata: map[string]string{"session_id": sessionID, "filename": filename, "source": "google_sheet"},
	})
	rows := strings.Count(strings.TrimRight(got.CSV, "\n"), "\n")
	header := strings.SplitN(got.CSV, "\n", 2)[0]
	return fmt.Sprintf("Attached the sheet as %s: %d data row(s), columns %s. preview_file_import {\"file\": %q, ...} previews it into a grid or dimension; "+
		"create_file_integration with \"sheet_url\" saves it as a Google Sheets integration that re-reads the sheet on every run.", filename, rows, header, filename), nil
}
