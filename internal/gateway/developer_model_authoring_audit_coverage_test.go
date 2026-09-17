// Developer Console model-authoring surfaces (metrics, dimensions, grids,
// folders, dashboards) previously only logged on create — updates/deletes,
// and the member/property/attach/detach sub-actions folded into
// EventDimensionUpdated/EventGridUpdated, were unaudited.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type devAuthoringFixture struct {
	pool                                           *pgxpool.Pool
	srv                                            *httptest.Server
	appID, modelID, revID, dimID, formID, metricID string
}

func setupDevAuthoringFixture(t *testing.T) *devAuthoringFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	custID := q(`INSERT INTO core.customer (name) VALUES ('Dev Audit Co') RETURNING id::text`)
	appID := q(`INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, 'App', 'planning') RETURNING id::text`, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Model') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'v1') RETURNING id::text`, modelID)

	dimID := q(`INSERT INTO model.dimension_def (model_id, name, agg_rule, revision_id) VALUES ($1::uuid, 'FixtureRegion', 'sum', $2::uuid) RETURNING id::text`, modelID, revID)
	formID := q(`INSERT INTO model.form_def (model_id, name, label) VALUES ($1::uuid, 'expense_form', 'Expense Form') RETURNING id::text`, modelID)
	metricID := q(`INSERT INTO model.metric_def (model_id, name, is_input, agg_rule, revision_id) VALUES ($1::uuid, 'FixtureRevenue', true, 'sum', $2::uuid) RETURNING id::text`, modelID, revID)

	developerID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('dev-audit-pa', 'dev@devaudit.dev', 'Dev') RETURNING id::text`)
	exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'developer')`, developerID)

	t.Setenv("DEV_MODE", "true")
	srv := httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(srv.Close)

	return &devAuthoringFixture{
		pool: pool, srv: srv, appID: appID, modelID: modelID, revID: revID,
		dimID: dimID, formID: formID, metricID: metricID,
	}
}

func (f *devAuthoringFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf []byte
	if body != nil {
		var err error
		buf, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", "dev-audit-pa")
	req.Header.Set("X-App-Id", f.appID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

func (f *devAuthoringFixture) latestAuditEvent(t *testing.T, eventType string) auditRow {
	t.Helper()
	var row auditRow
	err := f.pool.QueryRow(context.Background(), `
		SELECT category::text, event_type, resource_type, resource_id,
		       application_id::text, revision_id::text
		FROM audit.audit_event
		WHERE event_type = $1
		ORDER BY occurred_at DESC
		LIMIT 1
	`, eventType).Scan(&row.category, &row.eventType, &row.resourceType, &row.resourceID, &row.applicationID, &row.revisionID)
	if err != nil {
		t.Fatalf("no audit row for event_type %q: %v", eventType, err)
	}
	return row
}

func (f *devAuthoringFixture) latestAuditMetadata(t *testing.T, eventType string) map[string]string {
	t.Helper()
	var raw []byte
	err := f.pool.QueryRow(context.Background(), `
		SELECT metadata FROM audit.audit_event WHERE event_type = $1 ORDER BY occurred_at DESC LIMIT 1
	`, eventType).Scan(&raw)
	if err != nil {
		t.Fatalf("no audit row for event_type %q: %v", eventType, err)
	}
	var meta map[string]string
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	return meta
}

func TestDeveloperModelAuthoringMutationsAreAudited(t *testing.T) {
	f := setupDevAuthoringFixture(t)

	// ── metric ──────────────────────────────────────────────────────────
	status, body := f.do(t, "POST", "/api/developer/metrics", map[string]any{"name": "Revenue", "revision_id": f.revID, "agg_rule": "sum", "is_input": true})
	if status != http.StatusOK {
		t.Fatalf("create metric: status=%d body=%v", status, body)
	}
	metricID, _ := body["id"].(string)
	if metricID == "" {
		t.Fatalf("expected metric id, got %v", body)
	}
	f.latestAuditEvent(t, "metric.created")

	status, _ = f.do(t, "PATCH", "/api/developer/metrics/"+metricID, map[string]string{"name": "Revenue2", "agg_rule": "sum"})
	if status != http.StatusOK {
		t.Fatalf("update metric: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "metric.updated"); row.resourceID != metricID || row.applicationID == nil || *row.applicationID != f.appID {
		t.Errorf("metric.updated row = %+v, want resource_id=%s application_id=%s", row, metricID, f.appID)
	}

	status, _ = f.do(t, "DELETE", "/api/developer/metrics/"+metricID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete metric: status=%d", status)
	}
	f.latestAuditEvent(t, "metric.deleted")

	// ── dimension + members + properties ──────────────────────────────────
	status, body = f.do(t, "POST", "/api/developer/dimensions", map[string]string{"name": "Region", "revision_id": f.revID, "agg_rule": "sum"})
	if status != http.StatusOK {
		t.Fatalf("create dimension: status=%d body=%v", status, body)
	}
	dimID, _ := body["id"].(string)
	if dimID == "" {
		t.Fatalf("expected dimension id, got %v", body)
	}
	f.latestAuditEvent(t, "dimension.created")

	status, _ = f.do(t, "PATCH", "/api/developer/dimensions/"+dimID, map[string]string{"name": "Region2", "agg_rule": "sum"})
	if status != http.StatusOK {
		t.Fatalf("update dimension: status=%d", status)
	}
	if meta := f.latestAuditMetadata(t, "dimension.updated"); meta["sub_action"] != "dimension" {
		t.Errorf("dimension.updated sub_action = %q, want %q", meta["sub_action"], "dimension")
	}

	status, body = f.do(t, "POST", "/api/developer/dimensions/"+dimID+"/members", map[string]string{"code": "US", "label": "United States"})
	if status != http.StatusOK {
		t.Fatalf("add member: status=%d body=%v", status, body)
	}
	memberID, _ := body["id"].(string)
	if meta := f.latestAuditMetadata(t, "dimension.updated"); meta["sub_action"] != "member_added" {
		t.Errorf("member add sub_action = %q, want %q", meta["sub_action"], "member_added")
	}

	status, _ = f.do(t, "PATCH", "/api/developer/dimensions/"+dimID+"/members/"+memberID, map[string]string{"code": "US", "label": "USA"})
	if status != http.StatusOK {
		t.Fatalf("update member: status=%d", status)
	}
	if meta := f.latestAuditMetadata(t, "dimension.updated"); meta["sub_action"] != "member_updated" {
		t.Errorf("member update sub_action = %q, want %q", meta["sub_action"], "member_updated")
	}

	status, _ = f.do(t, "DELETE", "/api/developer/dimensions/"+dimID+"/members/"+memberID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete member: status=%d", status)
	}
	if meta := f.latestAuditMetadata(t, "dimension.updated"); meta["sub_action"] != "member_deleted" {
		t.Errorf("member delete sub_action = %q, want %q", meta["sub_action"], "member_deleted")
	}

	status, body = f.do(t, "POST", "/api/developer/dimensions/"+dimID+"/properties", map[string]string{"name": "region_code", "data_type": "text"})
	if status != http.StatusOK {
		t.Fatalf("add property: status=%d body=%v", status, body)
	}
	propID, _ := body["id"].(string)
	if meta := f.latestAuditMetadata(t, "dimension.updated"); meta["sub_action"] != "property_added" {
		t.Errorf("property add sub_action = %q, want %q", meta["sub_action"], "property_added")
	}

	status, _ = f.do(t, "DELETE", "/api/developer/dimensions/"+dimID+"/properties/"+propID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete property: status=%d", status)
	}
	if meta := f.latestAuditMetadata(t, "dimension.updated"); meta["sub_action"] != "property_deleted" {
		t.Errorf("property delete sub_action = %q, want %q", meta["sub_action"], "property_deleted")
	}

	status, _ = f.do(t, "DELETE", "/api/developer/dimensions/"+dimID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete dimension: status=%d", status)
	}
	f.latestAuditEvent(t, "dimension.deleted")

	// ── grid ────────────────────────────────────────────────────────────
	status, body = f.do(t, "POST", "/api/developer/grids", map[string]string{"name": "Grid1", "revision_id": f.revID})
	if status != http.StatusOK {
		t.Fatalf("create grid: status=%d body=%v", status, body)
	}
	gridID, _ := body["id"].(string)
	if gridID == "" {
		t.Fatalf("expected grid id, got %v", body)
	}
	if row := f.latestAuditEvent(t, "grid.created"); row.applicationID == nil || *row.applicationID != f.appID {
		t.Errorf("grid.created application_id = %v, want %s", row.applicationID, f.appID)
	}

	status, _ = f.do(t, "PATCH", "/api/developer/grids/"+gridID, map[string]string{"name": "Grid1b"})
	if status != http.StatusOK {
		t.Fatalf("update grid: status=%d", status)
	}
	if meta := f.latestAuditMetadata(t, "grid.updated"); meta["sub_action"] != "grid" {
		t.Errorf("grid update sub_action = %q, want %q", meta["sub_action"], "grid")
	}

	status, _ = f.do(t, "DELETE", "/api/developer/grids/"+gridID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete grid: status=%d", status)
	}
	f.latestAuditEvent(t, "grid.deleted")

	// ── folder ──────────────────────────────────────────────────────────
	status, body = f.do(t, "POST", "/api/developer/folders", map[string]string{"name": "Folder1"})
	if status != http.StatusOK {
		t.Fatalf("create folder: status=%d body=%v", status, body)
	}
	folderID, _ := body["id"].(string)
	if folderID == "" {
		t.Fatalf("expected folder id, got %v", body)
	}
	f.latestAuditEvent(t, "folder.created")

	status, _ = f.do(t, "PATCH", "/api/developer/folders/"+folderID, map[string]string{"name": "Folder1b"})
	if status != http.StatusOK {
		t.Fatalf("update folder: status=%d", status)
	}
	f.latestAuditEvent(t, "folder.updated")

	status, _ = f.do(t, "DELETE", "/api/developer/folders/"+folderID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete folder: status=%d", status)
	}
	f.latestAuditEvent(t, "folder.deleted")

	// ── dashboard + widget ──────────────────────────────────────────────
	status, body = f.do(t, "POST", "/api/developer/dashboards", map[string]any{"name": "Dash1", "revision_id": f.revID})
	if status != http.StatusOK {
		t.Fatalf("create dashboard: status=%d body=%v", status, body)
	}
	dashID, _ := body["id"].(string)
	if dashID == "" {
		t.Fatalf("expected dashboard id, got %v", body)
	}
	if row := f.latestAuditEvent(t, "dashboard.created"); row.applicationID == nil || *row.applicationID != f.appID {
		t.Errorf("dashboard.created application_id = %v, want %s", row.applicationID, f.appID)
	}

	status, _ = f.do(t, "PATCH", "/api/developer/dashboards/"+dashID, map[string]any{"name": "Dash1b"})
	if status != http.StatusOK {
		t.Fatalf("update dashboard: status=%d", status)
	}
	f.latestAuditEvent(t, "dashboard.updated")

	status, body = f.do(t, "POST", "/api/developer/dashboards/"+dashID+"/widgets", map[string]any{"widget_type": "text", "content": "hi"})
	if status != http.StatusOK {
		t.Fatalf("create widget: status=%d body=%v", status, body)
	}
	widgetID, _ := body["id"].(string)
	f.latestAuditEvent(t, "widget.created")

	status, _ = f.do(t, "PATCH", "/api/developer/dashboards/"+dashID+"/widgets/"+widgetID, map[string]any{"content": "hi2", "pos_x": 1, "pos_y": 1, "size_w": 40, "size_h": 40})
	if status != http.StatusOK {
		t.Fatalf("update widget: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "widget.updated"); row.resourceID != widgetID {
		t.Errorf("widget.updated resource_id = %s, want %s", row.resourceID, widgetID)
	}

	status, _ = f.do(t, "DELETE", "/api/developer/dashboards/"+dashID+"/widgets/"+widgetID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete widget: status=%d", status)
	}
	f.latestAuditEvent(t, "widget.deleted")

	status, _ = f.do(t, "DELETE", "/api/developer/dashboards/"+dashID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete dashboard: status=%d", status)
	}
	f.latestAuditEvent(t, "dashboard.deleted")
}
