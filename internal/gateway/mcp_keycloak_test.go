// The connector's identity setup against a real Keycloak (the version the
// deployments run): cmd/connector-clients' EnsureConnectorClients on the dev
// realm, then each host's authorization-code flow with PKCE as a person —
// sign-in form, consent screen, code exchange — and the token that comes
// out of it at the gateway. It needs Docker and a minute, so it runs when
// MAVERICKS_KEYCLOAK_IT=1.
package gateway_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mavericks-engine/mavericks/internal/gateway"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/pkg/keycloak"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

const keycloakImage = "quay.io/keycloak/keycloak:24.0"

func startKeycloak(t *testing.T) string {
	t.Helper()
	if os.Getenv("MAVERICKS_KEYCLOAK_IT") != "1" {
		t.Skip("set MAVERICKS_KEYCLOAK_IT=1 to run against a real Keycloak (Docker)")
	}
	realm, err := filepath.Abs("../../deploy/docker/config/keycloak/mavericks-realm.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        keycloakImage,
			Env:          map[string]string{"KEYCLOAK_ADMIN": "admin", "KEYCLOAK_ADMIN_PASSWORD": "admin"},
			Cmd:          []string{"start-dev", "--import-realm"},
			ExposedPorts: []string{"8080/tcp"},
			Files:        []testcontainers.ContainerFile{{HostFilePath: realm, ContainerFilePath: "/opt/keycloak/data/import/mavericks-realm.json", FileMode: 0o644}},
			WaitingFor: wait.ForHTTP("/realms/mavericks/.well-known/openid-configuration").
				WithPort("8080/tcp").WithStartupTimeout(4 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start keycloak: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	// The master realm requires HTTPS for requests from outside the
	// container, which plain HTTP to a test container always is.
	for _, cmd := range [][]string{
		{"/opt/keycloak/bin/kcadm.sh", "config", "credentials", "--server", "http://localhost:8080", "--realm", "master", "--user", "admin", "--password", "admin"},
		{"/opt/keycloak/bin/kcadm.sh", "update", "realms/master", "-s", "sslRequired=NONE"},
		// A person created the way every real account is (console, sign-up,
		// SSO, SCIM: the admin API), so with the realm's default roles —
		// offline_access among them. The dev realm's imported users carry
		// only the roles their import lists.
		{"/opt/keycloak/bin/kcadm.sh", "create", "users", "-r", "mavericks", "-s", "username=casey", "-s", "email=casey@acme.com",
			"-s", "firstName=Casey", "-s", "lastName=Reader", "-s", "enabled=true", "-s", "emailVerified=true"},
		{"/opt/keycloak/bin/kcadm.sh", "set-password", "-r", "mavericks", "--username", "casey", "--new-password", "mavericks"},
	} {
		if code, out, err := c.Exec(ctx, cmd); err != nil || code != 0 {
			b, _ := io.ReadAll(out)
			t.Fatalf("%v: %d %v %s", cmd, code, err, b)
		}
	}
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "8080/tcp")
	return "http://" + host + ":" + port.Port()
}

var formAction = regexp.MustCompile(`action="([^"]*login-actions/[^"]*)"`)

// authorize runs the authorization-code flow a host runs, as username, and
// returns the callback Keycloak redirected the browser to.
func authorize(t *testing.T, base, clientID, redirectURI, username string, pkce bool) (*url.URL, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	kcHost := strings.TrimPrefix(base, "http://")
	browser := &http.Client{Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Host != kcHost {
			return http.ErrUseLastResponse // the host's callback: stop there
		}
		return nil
	}}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id": {clientID}, "response_type": {"code"}, "redirect_uri": {redirectURI},
		"scope": {"models:read offline_access"}, "state": {"st-1"}, "resource": {"https://app.example.test/mcp"},
	}
	if pkce {
		q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
		q.Set("code_challenge_method", "S256")
	}
	start, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/realms/mavericks/protocol/openid-connect/auth?"+q.Encode(), nil)
	resp, err := browser.Do(start)
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 4; step++ {
		if loc := resp.Header.Get("Location"); resp.StatusCode/100 == 3 && loc != "" {
			_ = resp.Body.Close()
			u, _ := url.Parse(loc)
			return u, verifier
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		m := formAction.FindSubmatch(body)
		if m == nil {
			t.Logf("no form at step %d: %d %.300s", step, resp.StatusCode, body)
			return nil, verifier
		}
		action := html.UnescapeString(string(m[1]))
		if strings.HasPrefix(action, "/") {
			action = base + action
		}
		form := url.Values{"username": {username}, "password": {"mavericks"}}
		if strings.Contains(action, "consent") {
			form = url.Values{"accept": {"Yes"}}
		}
		if resp, err = browser.Do(formPost(action, form)); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("the flow did not reach the callback")
	return nil, ""
}

func formPost(target string, form url.Values) *http.Request {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

type tokenSet struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func exchange(t *testing.T, base string, form url.Values) (tokenSet, int) {
	t.Helper()
	resp, err := http.DefaultClient.Do(formPost(base+"/realms/mavericks/protocol/openid-connect/token", form))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var ts tokenSet
	_ = json.NewDecoder(resp.Body).Decode(&ts)
	return ts, resp.StatusCode
}

func claimsOf(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	return c
}

func TestConnectorClientsAgainstKeycloak(t *testing.T) {
	base := startKeycloak(t)
	ctx := context.Background()
	const resource = "https://app.example.test/mcp"

	admin := keycloak.NewAdmin(base, "mavericks", "admin", "admin")
	first, err := admin.EnsureConnectorClients(ctx, keycloak.ConnectorSetup{ResourceURL: resource})
	if err != nil {
		t.Fatalf("set up: %v", err)
	}
	again, err := admin.EnsureConnectorClients(ctx, keycloak.ConnectorSetup{ResourceURL: resource})
	if err != nil {
		t.Fatalf("set up again: %v", err)
	}
	secrets := map[string]string{}
	for i, c := range first {
		if !c.Created || again[i].Created || again[i].Secret != c.Secret || c.Secret == "" {
			t.Errorf("%s: not idempotent (created %v then %v; secret kept %v)", c.ClientID, c.Created, again[i].Created, again[i].Secret == c.Secret)
		}
		secrets[c.ClientID] = c.Secret
	}

	pool := setupJWTAuthDB(t)
	jwks, err := identity.NewJWKSValidator(ctx, base, "mavericks", "")
	if err != nil {
		t.Fatal(err)
	}
	srv := newConnectorServer(t, pool, jwks, resource)

	for _, host := range keycloak.DefaultConnectorHosts {
		redirect := host.RedirectURIs[0]
		t.Run(host.Name, func(t *testing.T) {
			if cb, _ := authorize(t, base, host.ClientID, "https://evil.example/callback", "casey", true); cb != nil && cb.Query().Get("code") != "" {
				t.Errorf("a code was sent to a callback the client does not have: %s", cb)
			}
			if cb, _ := authorize(t, base, host.ClientID, redirect, "casey", false); cb != nil && cb.Query().Get("code") != "" {
				t.Error("a code was issued without PKCE")
			}

			cb, _ := authorize(t, base, host.ClientID, redirect, "casey", true)
			if cb == nil || cb.Query().Get("code") == "" {
				t.Fatalf("no code at the callback: %v", cb)
			}
			if !strings.HasPrefix(cb.String(), redirect) || cb.Query().Get("state") != "st-1" {
				t.Errorf("callback %s", cb)
			}
			if got := cb.Query().Get("iss"); got != base+"/realms/mavericks" {
				t.Errorf("authorization response iss = %q, want the issuer (ChatGPT's stable redirect needs it)", got)
			}
			if _, status := exchange(t, base, url.Values{"grant_type": {"authorization_code"}, "code": {cb.Query().Get("code")},
				"redirect_uri": {redirect}, "client_id": {host.ClientID}, "client_secret": {secrets[host.ClientID]},
				"code_verifier": {"not-the-verifier-not-the-verifier-not-the-verifier"}}); status == http.StatusOK {
				t.Error("a code was exchanged with the wrong PKCE verifier")
			}
			cb, verifier := authorize(t, base, host.ClientID, redirect, "casey", true)
			ts, status := exchange(t, base, url.Values{"grant_type": {"authorization_code"}, "code": {cb.Query().Get("code")},
				"redirect_uri": {redirect}, "client_id": {host.ClientID}, "client_secret": {secrets[host.ClientID]},
				"code_verifier": {verifier}})
			if status != http.StatusOK || ts.AccessToken == "" {
				t.Fatalf("code exchange: %d %s: %s", status, ts.Error, ts.Description)
			}
			c := claimsOf(t, ts.AccessToken)
			// grids:write is a default scope: granted on consent though the
			// host asked for models:read only.
			if c["azp"] != host.ClientID || !strings.Contains(c["scope"].(string), "models:read") ||
				!strings.Contains(c["scope"].(string), "grids:write") || c["sub"] == nil {
				t.Errorf("token claims: azp=%v scope=%v sub=%v", c["azp"], c["scope"], c["sub"])
			}
			auds, _ := json.Marshal(c["aud"])
			if !strings.Contains(string(auds), resource) {
				t.Errorf("token audience %s lacks the connector %s", auds, resource)
			}
			if exp, iat := c["exp"].(float64), c["iat"].(float64); exp-iat > 300 {
				t.Errorf("token lives %vs, want at most 300", exp-iat)
			}

			sub := c["sub"].(string)
			if _, err := pool.Exec(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1, $2, 'Casey')
				ON CONFLICT DO NOTHING`, sub, "casey-"+host.ClientID+"@t.com"); err != nil {
				t.Fatal(err)
			}
			if status := restStatus(t, srv, ts.AccessToken); status != http.StatusUnauthorized {
				t.Errorf("the host's token on the REST API: %d, want 401", status)
			}
			mcpClient := sdk.NewClient(&sdk.Implementation{Name: host.Name, Version: "1"}, nil)
			cs, err := mcpClient.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv + "/mcp",
				HTTPClient: &http.Client{Transport: withBearer{ts.AccessToken}}, MaxRetries: -1}, nil)
			if err != nil {
				t.Fatalf("connect with the host's token: %v", err)
			}
			defer cs.Close() //nolint:errcheck
			res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "get_connection_access"})
			if err != nil || res.IsError {
				t.Fatalf("get_connection_access: %v %+v", err, res)
			}
			if raw, _ := json.Marshal(res.StructuredContent); !strings.Contains(string(raw), `"read_only":false`) {
				t.Errorf("a connection granted grids:write reads as read-only: %s", raw)
			}

			refreshed, status := exchange(t, base, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {ts.RefreshToken},
				"client_id": {host.ClientID}, "client_secret": {secrets[host.ClientID]}})
			if status != http.StatusOK || !strings.Contains(refreshed.Scope, "models:read") || !strings.Contains(refreshed.Scope, "grids:write") {
				t.Errorf("refresh: %d scope %q", status, refreshed.Scope)
			}
		})
	}
}

func newConnectorServer(t *testing.T, pool *pgxpool.Pool, jwks *identity.JWKSValidator, resource string) string {
	t.Helper()
	srv := httptest.NewServer(gateway.NewHandlerWithDeps(logger.New("test"), pool, jwks, gateway.Deps{
		MCP: gateway.MCPConfig{Enabled: true, ResourceURL: resource},
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func restStatus(t *testing.T, base, token string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", base+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
