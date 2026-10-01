// examples/sales-planning.mavericks-model.json is the model a community user
// can import to see the platform with something in it: the sales-planning demo
// (internal/salesdemo, base model plus forecast), exported definitions only.
//
// This test is both how that file is made and how it stays true. It builds the
// demo over HTTP, exports it the way a tenant administrator does from the
// console, and compares the result with the committed file; then it imports
// the committed file into another application. A change to the demo or to the
// package format fails here until the example is regenerated:
//
//	UPDATE_EXAMPLES=1 go test ./internal/gateway -run TestSalesDemoExamplePackage
package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/internal/salesdemo"
)

var exampleUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// canonicalExample makes an export reproducible: every ID becomes a fixed
// one, numbered in order of first appearance (the same ID always maps to the
// same replacement, so references inside the package still line up; import
// assigns new IDs anyway), and the export time becomes the zero time.
func canonicalExample(t *testing.T, raw []byte) []byte {
	t.Helper()
	ids := map[string]string{}
	raw = exampleUUID.ReplaceAllFunc(raw, func(id []byte) []byte {
		s, ok := ids[string(id)]
		if !ok {
			s = fmt.Sprintf("00000000-0000-4000-8000-%012d", len(ids)+1)
			ids[string(id)] = s
		}
		return []byte(s)
	})
	var pkg modeltransfer.Package
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	pkg.ExportedAt = time.Time{}
	out, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		t.Fatalf("encode example: %v", err)
	}
	return append(out, '\n')
}

func TestSalesDemoExamplePackage(t *testing.T) {
	d := setupSalesDemo(t)
	caller := func(method, path string, body any) (map[string]any, error) {
		status, raw := d.req(method, path, d.dev, body)
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("%s %s: status %d: %s", method, path, status, raw)
		}
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		return parsed, nil
	}
	if err := salesdemo.BuildForecast(caller, d.revID, d.model); err != nil {
		t.Fatalf("build forecast: %v", err)
	}

	// Export and import are the tenant administrator's (the console's
	// Applications screen), so that is who does both here.
	const ta = "demo-tenant-admin"
	var taID string
	if err := d.pool.QueryRow(t.Context(),
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'tenant-admin@demo.co', 'Tess Admin', $2::uuid) RETURNING id::text`,
		ta, d.custID).Scan(&taID); err != nil {
		t.Fatalf("tenant admin: %v", err)
	}
	if _, err := d.pool.Exec(t.Context(),
		`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`,
		taID, d.wsID); err != nil {
		t.Fatalf("tenant admin role: %v", err)
	}

	status, raw := d.req("GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s&include_data=false", d.modelID, d.revID), ta, nil)
	if status != 200 {
		t.Fatalf("export: status %d: %s", status, raw)
	}
	got := canonicalExample(t, raw)

	path := filepath.Join("..", "..", "examples", "sales-planning.mavericks-model.json")
	if os.Getenv("UPDATE_EXAMPLES") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // a published example, meant to be world-readable
			t.Fatalf("write example: %v", err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed path inside the repository
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s no longer matches a fresh export of the sales demo; regenerate it with\n\tUPDATE_EXAMPLES=1 go test ./internal/gateway -run TestSalesDemoExamplePackage", path)
	}

	// What is published must never carry anyone's numbers.
	var pkg modeltransfer.Package
	if err := json.Unmarshal(want, &pkg); err != nil {
		t.Fatalf("decode example: %v", err)
	}
	if pkg.IncludeData || len(pkg.Facts) > 0 || len(pkg.FormRecords) > 0 {
		t.Fatalf("example carries data: include_data=%v, %d facts, %d form records", pkg.IncludeData, len(pkg.Facts), len(pkg.FormRecords))
	}

	// The committed file, not the fresh export, is what users download, so
	// that is what has to import.
	var otherApp string
	if err := d.pool.QueryRow(t.Context(),
		`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Imported examples', 'planning') RETURNING id::text`,
		d.wsID, d.custID).Scan(&otherApp); err != nil {
		t.Fatalf("second application: %v", err)
	}
	status, body := d.req("POST", "/api/admin/models/import", ta,
		map[string]any{"application_id": otherApp, "model_name": "Sales Planning (example)", "package": json.RawMessage(want)})
	if status < 200 || status >= 300 {
		t.Fatalf("import example: status %d: %s", status, body)
	}
	var res struct {
		ModelID    string `json:"model_id"`
		RevisionID string `json:"revision_id"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.RevisionID == "" {
		t.Fatalf("import example: unexpected response %s", body)
	}
	var metrics, dims, grids int
	if err := d.pool.QueryRow(t.Context(), `
		SELECT (SELECT count(*) FROM model.metric_def WHERE revision_id=$1::uuid),
		       (SELECT count(*) FROM model.dimension_def WHERE revision_id=$1::uuid),
		       (SELECT count(*) FROM model.grid_def WHERE revision_id=$1::uuid)`,
		res.RevisionID).Scan(&metrics, &dims, &grids); err != nil {
		t.Fatalf("count imported: %v", err)
	}
	if metrics != len(pkg.Metrics) || dims != len(pkg.Dimensions) || grids != len(pkg.Grids) {
		t.Fatalf("imported %d metrics, %d dimensions, %d grids; the example has %d, %d, %d",
			metrics, dims, grids, len(pkg.Metrics), len(pkg.Dimensions), len(pkg.Grids))
	}
}
