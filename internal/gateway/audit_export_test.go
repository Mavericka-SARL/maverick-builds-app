package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/ee/auditexport"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// The export is the listing's scope, streamed: a tenant admin gets their
// own tenant's events and nothing else, oldest first, in full; a collector
// pages with the cursor and misses nothing; retention has a floor and the
// sweep removes only what is older than it.
func TestAuditExportAndRetention(t *testing.T) {
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
	tenant := func(name string) (custID, wsID, appID, adminSub, adminID string) {
		custID = q(`INSERT INTO core.customer (name, plan) VALUES ($1, 'enterprise') RETURNING id::text`, name)
		wsID = q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
		appID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, $3, 'planning') RETURNING id::text`, wsID, custID, name+" app")
		adminSub = "audit-admin-" + strings.ToLower(name)
		adminID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $2, $3, $4::uuid) RETURNING id::text`, adminSub, adminSub+"@test", name+" Admin", custID)
		exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, adminID, wsID)
		return
	}
	_, _, appA, adminA, adminAID := tenant("Alpha")
	_, _, appB, _, adminBID := tenant("Beta")
	log := logger.New("test")
	// Three events for Alpha (two on its app, one with no app but an Alpha
	// actor), one for Beta, in a known order.
	base := time.Now().Add(-time.Hour)
	stamp := func(id string, at time.Time) {
		exec(`UPDATE audit.audit_event SET occurred_at = $2 WHERE id = $1::uuid`, id, at)
	}
	logged := func(f auditlog.Fields, at time.Time) string {
		auditlog.Log(ctx, pool, log, f)
		id := q(`SELECT id::text FROM audit.audit_event ORDER BY occurred_at DESC, id DESC LIMIT 1`)
		stamp(id, at)
		return id
	}
	e1 := logged(auditlog.Fields{Category: auditlog.CategoryModelChange, EventType: auditlog.EventWorkflowDefCreated, ActorUserID: adminAID, ApplicationID: appA, ResourceType: "workflow_def", ResourceID: "w1", Metadata: map[string]string{"name": "Flow"}}, base)
	e2 := logged(auditlog.Fields{Category: auditlog.CategoryAdmin, EventType: auditlog.EventRoleCreated, ActorUserID: adminAID, ResourceType: "business_role", ResourceID: "r1"}, base.Add(time.Minute))
	e3 := logged(auditlog.Fields{Category: auditlog.CategoryModelChange, EventType: auditlog.EventWorkflowPublished, ActorUserID: adminAID, ApplicationID: appA, ResourceType: "workflow_def", ResourceID: "w1"}, base.Add(2*time.Minute))
	eB := logged(auditlog.Fields{Category: auditlog.CategoryModelChange, EventType: auditlog.EventWorkflowDefCreated, ActorUserID: adminBID, ApplicationID: appB, ResourceType: "workflow_def", ResourceID: "wb"}, base.Add(3*time.Minute))

	t.Setenv("DEV_MODE", "true")
	srv := httptest.NewServer(NewHandlerWithDeps(log, pool, nil, Deps{License: enterpriseManager(t)}))
	t.Cleanup(srv.Close)
	community := httptest.NewServer(NewHandler(log, pool, nil))
	t.Cleanup(community.Close)
	get := func(s *httptest.Server, persona, path string) (int, http.Header, []byte) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+path, nil)
		req.Header.Set("X-Dev-User", persona)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, resp.Header, out.Bytes()
	}

	// CSV: Alpha's three, oldest first, never Beta's.
	code, hdr, body := get(srv, adminA, "/api/admin/audit/export?format=csv")
	if code != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "text/csv") || !strings.Contains(hdr.Get("Content-Disposition"), "attachment") {
		t.Fatalf("csv: %d %v", code, hdr)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "occurred_at,category,event_type,actor_name") {
		t.Fatalf("csv lines: %d\n%s", len(lines), body)
	}
	for i, want := range []string{e1, e2, e3} {
		if !strings.HasSuffix(lines[i+1], ","+want) {
			t.Errorf("csv row %d should be event %s: %s", i+1, want, lines[i+1])
		}
	}
	if strings.Contains(string(body), eB) || strings.Contains(string(body), "Beta") {
		t.Errorf("another tenant's event leaked into the export:\n%s", body)
	}
	if !strings.Contains(lines[2], "Alpha Admin") || !strings.Contains(lines[2], ",{},") {
		t.Errorf("row carries the actor name and metadata: %s", lines[2])
	}

	// JSON Lines with a cursor: two per page, nothing missed, nothing twice.
	var seen []string
	after := ""
	for page := 0; page < 5; page++ {
		path := "/api/admin/audit/export?format=jsonl&limit=2"
		if after != "" {
			path += "&after=" + after
		}
		code, hdr, body := get(srv, adminA, path)
		if code != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "application/x-ndjson") {
			t.Fatalf("jsonl page %d: %d %v", page, code, hdr)
		}
		after = ""
		sc := bufio.NewScanner(bytes.NewReader(body))
		for sc.Scan() {
			var row map[string]any
			if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
				t.Fatalf("not JSON: %s", sc.Text())
			}
			if c, ok := row["next_cursor"].(string); ok {
				after = c
				continue
			}
			seen = append(seen, fmt.Sprint(row["id"]))
			if row["metadata"] == nil {
				t.Errorf("metadata missing: %v", row)
			}
		}
		if after == "" {
			break
		}
	}
	// The three seeded events come first; anything after them is the
	// record of an earlier export in this test (never of the pull itself).
	if len(seen) < 3 || strings.Join(seen[:3], ",") != strings.Join([]string{e1, e2, e3}, ",") {
		t.Errorf("paged export saw %v, want [%s %s %s …]", seen, e1, e2, e3)
	}
	for _, extra := range seen[3:] {
		var typ string
		_ = pool.QueryRow(ctx, `SELECT event_type FROM audit.audit_event WHERE id = $1::uuid`, extra).Scan(&typ)
		if typ != "audit.exported" {
			t.Errorf("unexpected extra event %s (%s) in the paged export", extra, typ)
		}
	}
	// Filters compose with the scope.
	if code, _, body := get(srv, adminA, "/api/admin/audit/export?format=jsonl&event_type=role.created"); code != http.StatusOK || strings.Count(string(body), "\n") != 1 || !strings.Contains(string(body), e2) {
		t.Errorf("event_type filter: %d\n%s", code, body)
	}
	since := base.Add(90 * time.Second).UTC().Format(time.RFC3339)
	if code, _, body := get(srv, adminA, "/api/admin/audit/export?format=jsonl&since="+since); code != http.StatusOK || !strings.Contains(string(body), e3) || strings.Contains(string(body), e1) {
		t.Errorf("since filter: %d\n%s", code, body)
	}
	if code, _, _ := get(srv, adminA, "/api/admin/audit/export?format=xml"); code != http.StatusBadRequest {
		t.Errorf("bad format: %d", code)
	}
	if code, _, _ := get(srv, adminA, "/api/admin/audit/export?format=jsonl&after=garbage"); code != http.StatusBadRequest {
		t.Errorf("bad cursor: %d", code)
	}
	if code, _, _ := get(community, adminA, "/api/admin/audit/export"); code != http.StatusForbidden {
		t.Errorf("community: %d", code)
	}
	// The export itself is on the record.
	var exports int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE event_type = 'audit.exported' AND actor_user_id = $1::uuid`, adminAID).Scan(&exports)
	if exports == 0 {
		t.Error("exports are not recorded in the audit log")
	}

	// Retention: a floor, and a sweep that removes only what is older.
	put := func(days int) (int, []byte) {
		t.Helper()
		b, _ := json.Marshal(map[string]int{"retention_days": days})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, srv.URL+"/api/admin/audit/settings", bytes.NewReader(b))
		req.Header.Set("X-Dev-User", adminA)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.Bytes()
	}
	if code, body := put(10); code != http.StatusBadRequest || !strings.Contains(string(body), "30") {
		t.Errorf("below the floor: %d %s", code, body)
	}
	if code, body := put(45); code != http.StatusOK || !strings.Contains(string(body), `"retention_days":45`) {
		t.Errorf("set retention: %d %s", code, body)
	}
	old := logged(auditlog.Fields{Category: auditlog.CategoryAdmin, EventType: auditlog.EventRoleCreated, ActorUserID: adminAID, ResourceType: "business_role", ResourceID: "ancient"}, time.Now().Add(-60*24*time.Hour))
	n, err := auditexport.Sweep(ctx, pool)
	if err != nil || n != 1 {
		t.Fatalf("sweep removed %d (err %v), want exactly the 60-day-old event", n, err)
	}
	var remaining int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE id = $1::uuid`, old).Scan(&remaining)
	if remaining != 0 {
		t.Error("the old event survived the sweep")
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE id IN ($1::uuid, $2::uuid, $3::uuid, $4::uuid)`, e1, e2, e3, eB).Scan(&remaining)
	if remaining != 4 {
		t.Errorf("recent events after the sweep: %d, want 4", remaining)
	}
	if code, _ := put(0); code != http.StatusOK {
		t.Errorf("retention off: %d", code)
	}
	if n, _ := auditexport.Sweep(ctx, pool); n != 0 {
		t.Errorf("a retention of 0 removed %d events", n)
	}

	// A person's download is the whole log, not the first page: more than
	// one page of events all arrive, under a single header.
	extra := auditexport.MaxLimit + 5
	exec(`INSERT INTO audit.audit_event (category, event_type, actor_user_id, application_id, resource_type, resource_id, occurred_at)
		SELECT 'admin', 'bulk.test', $1::uuid, $2::uuid, 'bulk', g::text, now() - interval '1 minute' + g * interval '1 microsecond'
		FROM generate_series(1, $3::int) g`, adminAID, appA, extra)
	code, _, body = get(srv, adminA, "/api/admin/audit/export?format=csv")
	lines = strings.Split(strings.TrimSpace(string(body)), "\n")
	if code != http.StatusOK || strings.Count(string(body), "occurred_at,category") != 1 || strings.Count(string(body), ",bulk.test,") != extra {
		t.Errorf("full download: %d, %d lines, %d bulk rows, want all %d under one header",
			code, len(lines), strings.Count(string(body), ",bulk.test,"), extra)
	}
}
