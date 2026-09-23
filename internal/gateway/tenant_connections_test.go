package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// A tenant's Google service account, kept under Integrations › Google
// Sheets: the developer who imports sheets stores the key file, tests it,
// and from then on the import wizard reads PRIVATE sheets shared with that
// account — through a fake Google that checks the signed assertion. The
// tenant's admin sees it too; another tenant's admin cannot reach it;
// removing it puts the tenant back on link-shared sheets only.
func TestGoogleServiceAccountConnection(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	t.Setenv("INTEGRATION_CRED_KEY", "test-integration-credential-key-32-bytes!")
	t.Setenv("DEV_MODE", "true")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	custA := q(`INSERT INTO core.customer (name) VALUES ('Sheets A') RETURNING id::text`)
	custB := q(`INSERT INTO core.customer (name) VALUES ('Sheets B') RETURNING id::text`)
	appA := q(`INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, 'App', 'planning') RETURNING id::text`, custA)
	modelA := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Model') RETURNING id::text`, appA)
	revA := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'v1') RETURNING id::text`, modelA)
	exec(`UPDATE core.model SET active_revision_id=$2::uuid WHERE id=$1::uuid`, modelA, revA)
	mk := func(sub, role, cust string) {
		id := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1 || '@x.test', $1, $2::uuid) RETURNING id::text`, sub, cust)
		exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, $2)`, id, role)
	}
	mk("gsa-admin-a", "tenant_admin", custA)
	mk("gsa-admin-b", "tenant_admin", custB)
	mk("gsa-dev-a", "developer", custA)

	// The account and its key file, as Google Cloud would download it.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	keyFile, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "acme-planning", "client_email": "sheets@acme-planning.iam.gserviceaccount.com",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":   "https://oauth2.googleapis.com/token",
	})

	const privateSheet = "1PrivateSheetSharedWithTheAccount000000000"
	// Fake Google: the token endpoint verifies the assertion's signature;
	// the Sheets API serves the private sheet to a bearer of that token.
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_ = r.ParseForm()
			tok, err := jwt.Parse(r.Form.Get("assertion"), func(*jwt.Token) (any, error) { return &rsaKey.PublicKey, nil }, jwt.WithValidMethods([]string{"RS256"}))
			if err != nil || !tok.Valid {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"ya29.ok"}`))
		case r.Header.Get("Authorization") != "Bearer ya29.ok":
			http.Error(w, `{"error":{"code":401}}`, 401)
		case strings.HasSuffix(r.URL.Path, "/v4/spreadsheets/"+privateSheet):
			_, _ = w.Write([]byte(`{"sheets":[{"properties":{"sheetId":0,"title":"Data"}}]}`))
		case strings.Contains(r.URL.Path, "/v4/spreadsheets/"+privateSheet+"/values/"):
			_, _ = w.Write([]byte(`{"values":[["FixtureRevenue"],["250"]]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(google.Close)
	// The link-shared export answers a sign-in page for the private sheet:
	// without the account, the wizard's fetch must still fail as before.
	docs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>Sign in</html>"))
	}))
	t.Cleanup(docs.Close)

	h := &handler{
		log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true,
		sheets:         &importpkg.SheetFetcher{BaseURL: docs.URL, Client: google.Client()},
		sheetsTokenURL: google.URL + "/token", sheetsAPIBase: google.URL,
	}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)
	do := func(persona, method, path string, body any) (int, map[string]any) {
		t.Helper()
		var buf []byte
		if body != nil {
			buf, _ = json.Marshal(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Dev-User", persona)
		req.Header.Set("X-App-Id", appA)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	sheetURL := "https://docs.google.com/spreadsheets/d/" + privateSheet + "/edit#gid=0"

	if code, body := do("gsa-dev-a", "POST", "/api/import/sheets/fetch", map[string]string{"sheet_url": sheetURL}); code != 400 || !strings.Contains(body["error"].(string), "not link-shared") {
		t.Fatalf("private sheet without an account: %d %v", code, body)
	}
	if code, body := do("gsa-dev-a", "GET", "/api/developer/integrations/google-service-account", nil); code != 200 || body["configured"] != false {
		t.Fatalf("empty connection: %d %v", code, body)
	}
	if code, body := do("gsa-dev-a", "PUT", "/api/developer/integrations/google-service-account", map[string]string{"key_file": `{"type":"authorized_user"}`}); code != 400 {
		t.Fatalf("user credentials accepted: %d %v", code, body)
	}
	code, body := do("gsa-dev-a", "PUT", "/api/developer/integrations/google-service-account", map[string]string{"key_file": string(keyFile)})
	if code != 200 || body["configured"] != true || body["client_email"] != "sheets@acme-planning.iam.gserviceaccount.com" || body["project_id"] != "acme-planning" {
		t.Fatalf("store: %d %v", code, body)
	}
	var stored string
	_ = pool.QueryRow(ctx, `SELECT secret_enc FROM core.tenant_credential WHERE customer_id = $1::uuid`, custA).Scan(&stored)
	if !strings.HasPrefix(stored, "iv1:") || strings.Contains(stored, "PRIVATE KEY") {
		t.Fatalf("the private key is not sealed at rest: %.20s", stored)
	}
	if code, body := do("gsa-dev-a", "POST", "/api/developer/integrations/google-service-account/test", nil); code != 200 || body["status"] != "ok" {
		t.Fatalf("test: %d %v", code, body)
	}
	if code, body := do("gsa-admin-a", "GET", "/api/developer/integrations/google-service-account", nil); code != 200 || body["configured"] != true {
		t.Fatalf("the tenant admin does not see the account: %d %v", code, body)
	}
	if code, body := do("gsa-dev-a", "POST", "/api/import/sheets/fetch", map[string]string{"sheet_url": sheetURL}); code != 200 || body["csv"] != "FixtureRevenue\n250\n" {
		t.Fatalf("private sheet through the account: %d %v", code, body)
	}
	if code, body := do("gsa-admin-b", "GET", "/api/developer/integrations/google-service-account", nil); code != 403 {
		t.Fatalf("tenant B reaches A's account through A's application: %d %v", code, body)
	}
	if code, body := do("gsa-dev-a", "DELETE", "/api/developer/integrations/google-service-account", nil); code != 200 || body["configured"] != false {
		t.Fatalf("delete: %d %v", code, body)
	}
	if code, _ := do("gsa-dev-a", "POST", "/api/import/sheets/fetch", map[string]string{"sheet_url": sheetURL}); code != 400 {
		t.Fatalf("after removal the private sheet must be unreachable again: %d", code)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE event_type IN ('tenant_credential.updated','tenant_credential.tested','tenant_credential.deleted')`).Scan(&n)
	if n != 3 {
		t.Fatalf("audit rows = %d, want 3", n)
	}
}
