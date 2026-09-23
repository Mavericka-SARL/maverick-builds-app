package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/imagedata"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// A dashboard image is its own content: the picture is stored inline, so it
// survives a revision copy without a second store to keep in step. What is
// not a picture is refused, at creation and at update alike — the content
// column is served straight back to a browser as an image source.
func TestDashboardImageWidget(t *testing.T) {
	t.Setenv("DEV_MODE", "true")
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	cust := q(`INSERT INTO core.customer (name) VALUES ('Picture Co') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid,'ws') RETURNING id::text`, cust)
	app := q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid,$2::uuid,'App','planning') RETURNING id::text`, cust, ws)
	model := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid,'M') RETURNING id::text`, app)
	rev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid,'Working') RETURNING id::text`, model)
	if _, err := pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid WHERE id=$2::uuid`, rev, model); err != nil {
		t.Fatal(err)
	}
	dash := q(`INSERT INTO model.dashboard_def (model_id, name, revision_id) VALUES ($1::uuid,'Guide',$2::uuid) RETURNING id::text`, model, rev)
	dev := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('pic-dev','dev@pic.test','Dev',$1::uuid) RETURNING id::text`, cust)
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'developer',$2::uuid)`, dev, ws); err != nil {
		t.Fatal(err)
	}

	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)
	do := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var buf []byte
		if body != nil {
			buf, _ = json.Marshal(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(buf))
		req.Header.Set("X-Dev-User", "pic-dev")
		req.Header.Set("X-App-Id", app)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	svg := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg xmlns='http://www.w3.org/2000/svg'><rect width='4' height='4'/></svg>"))
	code, body := do("POST", "/api/developer/dashboards/"+dash+"/widgets", map[string]any{
		"widget_type": "image", "content": svg, "pos_x": 0, "pos_y": 0, "size_w": 600, "size_h": 200,
		"widget_props": map[string]any{"alt": "How it fits together", "image_fit": "contain"},
	})
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("store an image: %d %v", code, body)
	}
	widgetID, _ := body["id"].(string)
	if widgetID == "" {
		t.Fatalf("no widget id: %v", body)
	}

	// Not a picture: refused on the way in, and on a later edit.
	for _, bad := range []string{"https://example.com/x.png", "data:text/html;base64,PHNjcmlwdD4="} {
		if code, body := do("POST", "/api/developer/dashboards/"+dash+"/widgets", map[string]any{
			"widget_type": "image", "content": bad, "size_w": 100, "size_h": 100,
		}); code != http.StatusBadRequest {
			t.Fatalf("%q was accepted: %d %v", bad, code, body)
		}
		if code, body := do("PATCH", "/api/developer/dashboards/"+dash+"/widgets/"+widgetID, map[string]any{"content": bad}); code != http.StatusBadRequest {
			t.Fatalf("%q was accepted on update: %d %v", bad, code, body)
		}
	}
	oversized := "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, imagedata.MaxWidgetBytes+1))
	if code, _ := do("POST", "/api/developer/dashboards/"+dash+"/widgets", map[string]any{
		"widget_type": "image", "content": oversized, "size_w": 100, "size_h": 100,
	}); code != http.StatusBadRequest {
		t.Fatalf("an oversized image was accepted: %d", code)
	}
	// A text widget's content is prose and is never held to that bar.
	if code, body := do("POST", "/api/developer/dashboards/"+dash+"/widgets", map[string]any{
		"widget_type": "text", "content": "# Heading\n\nSome **prose**.", "size_w": 600, "size_h": 100,
	}); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("store prose: %d %v", code, body)
	}

	var stored string
	_ = pool.QueryRow(ctx, `SELECT content FROM model.dashboard_widget WHERE id=$1::uuid`, widgetID).Scan(&stored)
	if stored != svg {
		t.Fatalf("stored content is not the picture: %.40s", stored)
	}

	// The point of storing it inline: a new revision carries the picture.
	code, body = do("POST", "/api/developer/revisions", map[string]any{"name": "Next", "source_revision_id": rev})
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("new revision: %d %v", code, body)
	}
	var copies int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id = w.dashboard_id
		WHERE d.model_id=$1::uuid AND w.widget_type='image' AND w.content = $2`, model, svg).Scan(&copies); err != nil {
		t.Fatal(err)
	}
	if copies != 2 {
		t.Fatalf("image widgets after copying the revision = %d, want the original and its copy", copies)
	}
	if !strings.HasPrefix(stored, "data:image/") {
		t.Fatal("the stored picture is not a data URL")
	}
}
