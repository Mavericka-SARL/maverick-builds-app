package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// The consent round trip for a REST connection: a developer presses
// Connect (start), the provider sends the browser to the public callback,
// the gateway exchanges the code — PKCE verifier and client secret checked
// by a fake provider — seals the tokens into the connection, and returns
// the person to the console. A run then uses the token, and refreshes it
// once it has expired. Disconnect forgets the tokens, keeping the client.
func TestIntegrationOAuthAuthorizationCode(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("DEV_MODE", "true")
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed: %v", err)
		}
		return id
	}
	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('OAuthCo','enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid,'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid,$2::uuid,'App','planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid,'M') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid,'Working') RETURNING id::text`, modelID)
	if _, err := pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID); err != nil {
		t.Fatal(err)
	}
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('oauth-dev','dev@oauthco.com','Dev',$1::uuid) RETURNING id::text`, custID)
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'developer',$2::uuid)`, devID, wsID); err != nil {
		t.Fatal(err)
	}

	// The provider: consent is simulated by the test; the token endpoint
	// is real enough to check what a strict provider checks.
	var issuedChallenge string
	tokens := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		user, pass, _ := r.BasicAuth()
		if user != "client-123" || pass != "s3cret" {
			http.Error(w, `{"error":"invalid_client"}`, 401)
			return
		}
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "good-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != issuedChallenge || !strings.HasSuffix(r.Form.Get("redirect_uri"), "/api/integrations/oauth/callback") {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return
			}
			tokens++
			_, _ = w.Write([]byte(`{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600,"token_type":"Bearer"}`))
		case "refresh_token":
			if r.Form.Get("refresh_token") != "rt-1" {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return
			}
			tokens++
			_, _ = w.Write([]byte(`{"access_token":"at-2","refresh_token":"rt-2","expires_in":3600}`))
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, 400)
		}
	}))
	t.Cleanup(provider.Close)

	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, publicURL: "https://console.example.test"}
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
		req.Header.Set("X-Dev-User", "oauth-dev")
		req.Header.Set("X-App-Id", appID)
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

	code, conn := do("POST", "/api/developer/integration-connections", map[string]any{
		"name": "CRM", "auth_type": "oauth2_authorization_code",
		"meta":   map[string]any{"authorization_url": provider.URL + "/authorize?access_type=offline", "token_url": provider.URL + "/token", "client_id": "client-123", "scope": "read"},
		"secret": map[string]any{"client_secret": "s3cret"},
	})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create connection: %d %v", code, conn)
	}
	connID := conn["id"].(string)

	if code, body := do("POST", "/api/developer/integration-connections/"+connID+"/test", nil); code != 200 || body["ok"] != false || !strings.Contains(body["error"].(string), "Connect") {
		t.Fatalf("test before connecting: %d %v", code, body)
	}

	code, start := do("POST", "/api/developer/integration-connections/"+connID+"/oauth/start", map[string]any{"return_to": "/developer/integrations"})
	if code != 200 {
		t.Fatalf("start: %d %v", code, start)
	}
	authURL, _ := url.Parse(start["authorization_url"].(string))
	aq := authURL.Query()
	if !strings.HasPrefix(authURL.String(), provider.URL+"/authorize") || aq.Get("access_type") != "offline" || aq.Get("client_id") != "client-123" ||
		aq.Get("response_type") != "code" || aq.Get("scope") != "read" || aq.Get("code_challenge_method") != "S256" || aq.Get("state") == "" ||
		aq.Get("redirect_uri") != "https://console.example.test/api/integrations/oauth/callback" {
		t.Fatalf("authorization url: %s", authURL)
	}
	issuedChallenge = aq.Get("code_challenge")

	// The browser comes back from the provider. No session needed.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cb, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/integrations/oauth/callback?state="+url.QueryEscape(aq.Get("state"))+"&code=good-code", nil)
	resp, err := noRedirect.Do(cb)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || loc != "https://console.example.test/developer/integrations?oauth=connected" {
		t.Fatalf("callback: %d %s", resp.StatusCode, loc)
	}
	if tokens != 1 {
		t.Fatalf("token exchanges = %d", tokens)
	}
	// A replayed callback finds no state.
	cb2, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/integrations/oauth/callback?state="+url.QueryEscape(aq.Get("state"))+"&code=good-code", nil)
	resp2, _ := noRedirect.Do(cb2)
	resp2.Body.Close() //nolint:errcheck
	if !strings.Contains(resp2.Header.Get("Location"), "oauth=error") {
		t.Fatalf("replayed callback: %s", resp2.Header.Get("Location"))
	}

	code, body := do("POST", "/api/developer/integration-connections/"+connID+"/test", nil)
	if code != 200 || body["ok"] != true {
		t.Fatalf("test after connecting: %d %v", code, body)
	}
	var metaJSON string
	_ = pool.QueryRow(ctx, `SELECT meta::text FROM model.integration_connection WHERE id=$1::uuid`, connID).Scan(&metaJSON)
	if !strings.Contains(metaJSON, `"connected_at"`) || strings.Contains(metaJSON, "at-1") {
		t.Fatalf("meta after connect: %s", metaJSON)
	}

	// A run's bearer: the stored token while fresh; a refresh once expired,
	// and the refreshed tokens are stored for the next run.
	st := integration.NewStore(pool)
	authType, metaRaw, secretRaw, err := st.OpenCredential(ctx, appID, connID)
	if err != nil || authType != integration.AuthTypeOAuthCode {
		t.Fatalf("open: %v %s", err, authType)
	}
	secret := map[string]string{}
	_ = json.Unmarshal(secretRaw, &secret)
	if b, err := st.OAuthBearer(ctx, provider.Client(), appID, connID, metaRaw, secret, true); err != nil || b != "at-1" {
		t.Fatalf("fresh bearer: %q %v", b, err)
	}
	secret["expires_at"] = "2020-01-01T00:00:00Z"
	if b, err := st.OAuthBearer(ctx, provider.Client(), appID, connID, metaRaw, secret, true); err != nil || b != "at-2" {
		t.Fatalf("refreshed bearer: %q %v", b, err)
	}
	_, _, secretRaw, _ = st.OpenCredential(ctx, appID, connID)
	if !strings.Contains(string(secretRaw), `"refresh_token":"rt-2"`) || !strings.Contains(string(secretRaw), `"client_secret":"s3cret"`) {
		t.Fatalf("rotated tokens not stored: %s", secretRaw)
	}

	if code, body := do("POST", "/api/developer/integration-connections/"+connID+"/oauth/disconnect", nil); code != 200 || body["has_secret"] != true {
		t.Fatalf("disconnect: %d %v", code, body)
	}
	_, _, secretRaw, _ = st.OpenCredential(ctx, appID, connID)
	if strings.Contains(string(secretRaw), "access_token") || !strings.Contains(string(secretRaw), "client_secret") {
		t.Fatalf("after disconnect: %s", secretRaw)
	}
}
