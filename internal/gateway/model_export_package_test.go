// Tests GET /api/admin/models/{id}/export/package — the tar.gz standalone
// deployment package, the HTTP surface that replaced the unreachable gRPC
// deployment builder. Mirrors model_transfer_test.go's role-gate and
// cross-tenant shape, plus a round trip proving the packaged package.json
// imports cleanly through the existing, unmodified POST
// /api/admin/models/import endpoint — the same file both this route and
// adminModelExport ultimately produce (internal/modeltransfer.CollectExport)
// — and retention assertions proving the package actually lands in object
// storage via pkg/objectstore.
package gateway

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testobjectstore"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/objectstore"
)

// testObjectStore bundles the pieces withObjectStore constructs: store is
// what production code (the handler) uses to Save; meta/blob are exposed
// separately so tests can assert on what actually landed (Store itself is
// deliberately write-only — see its doc comment).
type testObjectStore struct {
	store *objectstore.Store
	meta  *objectstore.MetaStore
	blob  *objectstore.Client
}

// withObjectStore replaces f.srv with a new server backed by
// NewHandlerWithObjectStore against a real MinIO testcontainer — local to
// this file's tests, not the shared setupRollupFixture, which ~5+ unrelated
// test files also use and never touch this route. The original server
// (built by setupRollupFixture) is still closed by its own already-
// registered t.Cleanup; this just points f.srv at a second one for the
// rest of the calling test.
func withObjectStore(t *testing.T, f *rollupFixture) testObjectStore {
	t.Helper()
	ctx := context.Background()

	// Built from deploy/docker/minio: no registry serves MinIO to anonymous
	// pulls any more (internal/testobjectstore says why).
	ctr := testobjectstore.Run(t)
	endpoint, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("minio connection string: %v", err)
	}
	blobClient, err := objectstore.NewClient(endpoint, ctr.Username, ctr.Password, "mavericks-test", false)
	if err != nil {
		t.Fatalf("new object store client: %v", err)
	}
	if err := blobClient.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	meta := objectstore.NewMetaStore(f.pool)
	store := objectstore.NewStore(blobClient, meta)

	newSrv := httptest.NewServer(NewHandlerWithObjectStore(logger.New("test"), f.pool, nil, store))
	t.Cleanup(newSrv.Close)
	f.srv = newSrv

	return testObjectStore{store: store, meta: meta, blob: blobClient}
}

// doRaw issues a request and returns the raw response — model_transfer_test.go's
// rollupFixture.do always JSON-decodes the body, which a binary tar.gz
// response can't go through.
func doRaw(t *testing.T, f *rollupFixture, method, path, persona string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", persona)
	req.Header.Set("X-App-Id", f.appID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header, body
}

// extractPackageJSON reads a tar.gz response body and returns
// mavericks-model/package.json's raw content.
func extractPackageJSON(t *testing.T, tarGz []byte) []byte {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(tarGz))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close() //nolint:errcheck
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			t.Fatal("tar archive has no mavericks-model/package.json")
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if hdr.Name == "mavericks-model/package.json" {
			data, err := io.ReadAll(tr) //nolint:gosec
			if err != nil {
				t.Fatalf("read package.json: %v", err)
			}
			return data
		}
	}
}

func newRollupTenantAdmin(t *testing.T, f *rollupFixture, sub, email string) string {
	t.Helper()
	ctx := context.Background()
	var custID, wsID string
	if err := f.pool.QueryRow(ctx, `
		SELECT c.id::text, w.id::text FROM core.customer c JOIN core.workspace w ON w.customer_id=c.id
		WHERE w.id = (SELECT workspace_id FROM core.application WHERE id=$1::uuid)
	`, f.appID).Scan(&custID, &wsID); err != nil {
		t.Fatalf("find customer/workspace: %v", err)
	}
	var taID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $2, 'TA', $3::uuid) RETURNING id::text
	`, sub, email, custID).Scan(&taID); err != nil {
		t.Fatalf("insert tenant admin: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, taID, wsID); err != nil {
		t.Fatalf("grant tenant_admin: %v", err)
	}
	devPersonas["rollup-"+sub] = sub
	t.Cleanup(func() { delete(devPersonas, "rollup-"+sub) })
	return taID
}

func TestModelExportPackage_TenantAdminOnlyAndValidTarGz(t *testing.T) {
	f := setupRollupFixture(t)
	withObjectStore(t, f)
	newRollupTenantAdmin(t, f, "test-pkg-ta", "pkg-ta@t.com")

	packagePath := fmt.Sprintf("/api/admin/models/%s/export/package?revision_id=%s", f.modelID, f.workingRevID)

	for _, persona := range []string{"rollup-test-approver", "rollup-test-manager"} {
		if status, _, _ := doRaw(t, f, "GET", packagePath, persona); status != http.StatusForbidden {
			t.Errorf("export package as %s status = %d, want 403", persona, status)
		}
	}

	status, headers, body := doRaw(t, f, "GET", packagePath, "rollup-test-pkg-ta")
	if status != http.StatusOK {
		t.Fatalf("export package status = %d, body = %s", status, body)
	}
	if ct := headers.Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}
	if cd := headers.Get("Content-Disposition"); cd == "" {
		t.Error("Content-Disposition header is empty")
	}
	// gzip magic bytes.
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatal("response body is not a gzip archive")
	}

	pkgJSON := extractPackageJSON(t, body)
	var pkg struct {
		Format       string `json:"format"`
		RevisionName string `json:"revision_name"`
		FactsPolicy  string `json:"facts_policy"`
	}
	if err := json.Unmarshal(pkgJSON, &pkg); err != nil {
		t.Fatalf("unmarshal package.json: %v", err)
	}
	if pkg.Format != "mavericks-model-export" {
		t.Errorf("package.json format = %q, want mavericks-model-export", pkg.Format)
	}
	if pkg.RevisionName != "Working" {
		t.Errorf("package.json revision_name = %q, want Working", pkg.RevisionName)
	}
	if pkg.FactsPolicy == "" {
		t.Error("package.json facts_policy is empty, want the explicit policy string")
	}
}

func TestModelExportPackage_RejectsCrossTenantAccess(t *testing.T) {
	f := setupRollupFixture(t)
	withObjectStore(t, f)

	otherCustID := f.pool.QueryRow(context.Background(), `INSERT INTO core.customer (name, plan) VALUES ('Other Pkg Co', 'standard') RETURNING id::text`)
	var otherCustomerID string
	if err := otherCustID.Scan(&otherCustomerID); err != nil {
		t.Fatalf("insert other customer: %v", err)
	}
	var otherWsID string
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Other Pkg WS') RETURNING id::text`, otherCustomerID).Scan(&otherWsID); err != nil {
		t.Fatalf("insert other workspace: %v", err)
	}
	var otherTAID string
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-pkg-other', 'pkg-other@t.com', 'Other TA', $1::uuid) RETURNING id::text`, otherCustomerID).Scan(&otherTAID); err != nil {
		t.Fatalf("insert other tenant admin: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, otherTAID, otherWsID); err != nil {
		t.Fatalf("grant other tenant_admin: %v", err)
	}
	devPersonas["rollup-test-pkg-other"] = "test-pkg-other"
	t.Cleanup(func() { delete(devPersonas, "rollup-test-pkg-other") })

	packagePath := fmt.Sprintf("/api/admin/models/%s/export/package?revision_id=%s", f.modelID, f.workingRevID)
	if status, _, body := doRaw(t, f, "GET", packagePath, "rollup-test-pkg-other"); status != http.StatusForbidden {
		t.Errorf("other tenant's admin downloading f's package status = %d, want 403, body = %s", status, body)
	}
}

// TestModelExportPackage_RoundTripsThroughExistingImport proves the tar.gz
// route's package.json is byte-for-byte the same shape adminModelImport
// already knows how to consume — both this route and adminModelExport
// funnel through the exact same modeltransfer.CollectExport.
func TestModelExportPackage_RoundTripsThroughExistingImport(t *testing.T) {
	f := setupRollupFixture(t)
	withObjectStore(t, f)
	newRollupTenantAdmin(t, f, "test-pkg-roundtrip", "pkg-roundtrip@t.com")

	packagePath := fmt.Sprintf("/api/admin/models/%s/export/package?revision_id=%s", f.modelID, f.workingRevID)
	status, _, body := doRaw(t, f, "GET", packagePath, "rollup-test-pkg-roundtrip")
	if status != http.StatusOK {
		t.Fatalf("export package status = %d, body = %s", status, body)
	}
	pkgJSON := extractPackageJSON(t, body)
	var pkg map[string]any
	if err := json.Unmarshal(pkgJSON, &pkg); err != nil {
		t.Fatalf("unmarshal package.json: %v", err)
	}

	status, res := f.do(t, "POST", "/api/admin/models/import", "rollup-test-pkg-roundtrip", map[string]any{
		"application_id": f.appID,
		"model_name":     "M From Package",
		"package":        pkg,
	})
	if status != http.StatusOK {
		t.Fatalf("import status = %d, body = %v", status, res)
	}
	newModelID, _ := res["model_id"].(string)
	if newModelID == "" || newModelID == f.modelID {
		t.Fatalf("unexpected import result: %v", res)
	}

	var dimCount, metricCount int
	_ = f.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM model.dimension_def WHERE model_id=$1::uuid`, newModelID).Scan(&dimCount)
	_ = f.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM model.metric_def WHERE model_id=$1::uuid`, newModelID).Scan(&metricCount)
	if dimCount != 3 {
		t.Errorf("imported dimensions = %d, want 3", dimCount)
	}
	if metricCount == 0 {
		t.Error("imported metrics = 0, want at least 1")
	}
}

// TestModelExportPackage_RetainedInObjectStoreAndOverwrittenOnReExport is
// the real, concrete outcome this item exists to prove: a successful
// export lands a storage.object row (not just a local temp file that gets
// deleted), and a second export of the SAME revision overwrites that row
// in place rather than accumulating history — matching decision "retained
// artifacts = durable, current-per-revision, not an unbounded archive."
func TestModelExportPackage_RetainedInObjectStoreAndOverwrittenOnReExport(t *testing.T) {
	f := setupRollupFixture(t)
	objStore := withObjectStore(t, f)
	newRollupTenantAdmin(t, f, "test-pkg-retain", "pkg-retain@t.com")

	packagePath := fmt.Sprintf("/api/admin/models/%s/export/package?revision_id=%s", f.modelID, f.workingRevID)

	status, _, body1 := doRaw(t, f, "GET", packagePath, "rollup-test-pkg-retain")
	if status != http.StatusOK {
		t.Fatalf("first export status = %d, body = %s", status, body1)
	}

	rec1, err := objStore.meta.Get(context.Background(), "deployment_package", f.workingRevID)
	if err != nil {
		t.Fatalf("get metadata after first export: %v", err)
	}
	if rec1.SizeBytes != int64(len(body1)) {
		t.Errorf("stored size_bytes = %d, want %d (the exact bytes served)", rec1.SizeBytes, len(body1))
	}

	// Re-export the SAME revision.
	status, _, body2 := doRaw(t, f, "GET", packagePath, "rollup-test-pkg-retain")
	if status != http.StatusOK {
		t.Fatalf("second export status = %d, body = %s", status, body2)
	}
	rec2, err := objStore.meta.Get(context.Background(), "deployment_package", f.workingRevID)
	if err != nil {
		t.Fatalf("get metadata after second export: %v", err)
	}
	if rec2.ID != rec1.ID {
		t.Errorf("second export created a NEW object record (id %s != %s) — want overwrite in place", rec2.ID, rec1.ID)
	}

	var rowCount int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM storage.object WHERE owner_type='deployment_package' AND owner_id=$1
	`, f.workingRevID).Scan(&rowCount); err != nil {
		t.Fatalf("count storage.object rows: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("storage.object row count = %d, want 1 (re-export must overwrite, not accumulate)", rowCount)
	}

	// The retained object is actually retrievable and matches what was served.
	blobData, err := objStore.blob.Get(context.Background(), rec2.ObjectKey)
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	if !bytes.Equal(blobData, body2) {
		t.Error("retained blob does not match the bytes served on the second export")
	}
}
