// The connector's tokens against real signature checks: a local JWK Set,
// tokens signed with its key (jwt_auth_test.go's fixture). A connector
// token opens /mcp and nothing else; a console token opens the REST API and
// not /mcp.
package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mavericks-engine/mavericks/internal/gateway"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

const connectorResource = "https://app.example.test/mcp"

type withBearer struct{ token string }

func (b withBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestConnectorTokens(t *testing.T) {
	ctx := context.Background()
	pool := setupJWTAuthDB(t)
	jf := setupJWKSFixture(t)
	defer jf.srv.Close()
	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('conn-sub-1', 'conn@t.com', 'Connie') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	jwks, err := identity.NewJWKSValidator(ctx, jf.srv.URL, jwtTestRealm, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gateway.NewHandlerWithDeps(logger.New("test"), pool, jwks, gateway.Deps{
		MCP: gateway.MCPConfig{Enabled: true, ResourceURL: connectorResource},
	}))
	defer srv.Close()

	token := func(azp string, aud []string, scope string, exp time.Time) string {
		return jf.signClaims(t, identity.Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: "conn-sub-1", Issuer: jf.issuer, Audience: aud,
				IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(exp),
			},
			AuthorizedParty: azp, Scope: scope,
		})
	}
	hour := time.Now().Add(time.Hour)
	connector := token("claude-connector", []string{connectorResource}, "openid models:read", hour)
	console := jf.sign(t, "conn-sub-1", jf.issuer, hour)

	type answer struct {
		StatusCode int
		Header     http.Header
	}
	do := func(method, path, tok, body string) (answer, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, strings.NewReader(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return answer{StatusCode: resp.StatusCode, Header: resp.Header}, string(raw)
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

	t.Run("the REST API takes console tokens only", func(t *testing.T) {
		if resp, body := do("GET", "/api/me", console, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("console token on REST: %d %s", resp.StatusCode, body)
		}
		if resp, _ := do("GET", "/api/me", connector, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("connector token on REST: %d, want 401 — it would carry the account's full read-write reach", resp.StatusCode)
		}
		if resp, _ := do("GET", "/api/me", token("", nil, "", hour), ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("a token naming no client on REST: %d, want 401", resp.StatusCode)
		}
	})

	t.Run("the connection details are for signed-in people", func(t *testing.T) {
		if resp, _ := do("GET", "/api/connector", "", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("no token: %d, want 401", resp.StatusCode)
		}
		if resp, body := do("GET", "/api/connector", console, ""); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"enabled":true`) {
			t.Errorf("console token: %d %s", resp.StatusCode, body)
		}
		if resp, _ := do("GET", "/api/connector", connector, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("a connector token on the REST API: %d, want 401", resp.StatusCode)
		}
	})

	t.Run("/mcp refuses everything but a connector token", func(t *testing.T) {
		resp, _ := do("POST", "/mcp", "", initialize)
		if resp.StatusCode != http.StatusUnauthorized ||
			!strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="https://app.example.test/.well-known/oauth-protected-resource/mcp"`) {
			t.Errorf("no token: %d, WWW-Authenticate %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
		for name, tok := range map[string]string{
			"a console token":  console,
			"another audience": token("claude-connector", []string{"https://elsewhere.test/mcp"}, "models:read", hour),
			"an expired token": token("claude-connector", []string{connectorResource}, "models:read", time.Now().Add(-time.Hour)),
			"a console-client token minted for the connector": token("mavericks-web", []string{connectorResource}, "models:read", hour),
			"a client registered for no host":                 token("some-other-app", []string{connectorResource}, "models:read", hour),
		} {
			if resp, body := do("POST", "/mcp", tok, initialize); resp.StatusCode != http.StatusUnauthorized || strings.Contains(body, "conn-sub-1") {
				t.Errorf("%s: %d %s, want 401", name, resp.StatusCode, body)
			}
		}
		if resp, _ := do("POST", "/mcp", token("claude-connector", []string{connectorResource}, "openid", hour), initialize); resp.StatusCode != http.StatusForbidden {
			t.Errorf("a token without models:read: %d, want 403", resp.StatusCode)
		}
	})

	t.Run("the protected-resource metadata names the resource and the realm", func(t *testing.T) {
		resp, body := do("GET", "/.well-known/oauth-protected-resource/mcp", "", "")
		var meta struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
			Scopes               []string `json:"scopes_supported"`
		}
		_ = json.Unmarshal([]byte(body), &meta)
		if resp.StatusCode != http.StatusOK || meta.Resource != connectorResource || len(meta.Scopes) != 2 || meta.Scopes[0] != "models:read" || meta.Scopes[1] != "grids:write" {
			t.Errorf("metadata: %d %s", resp.StatusCode, body)
		}
	})

	t.Run("a connector token reads as its subject, until the account is disabled", func(t *testing.T) {
		client := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil)
		cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + "/mcp",
			HTTPClient: &http.Client{Transport: withBearer{connector}}, MaxRetries: -1}, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer cs.Close() //nolint:errcheck
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "get_connection_access"})
		if err != nil || res.IsError {
			t.Fatalf("get_connection_access: %v %+v", err, res)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		if !strings.Contains(string(raw), `"roles":[]`) || !strings.Contains(string(raw), `"read_only":true`) ||
			strings.Contains(string(raw), "conn@t.com") || strings.Contains(string(raw), "Connie") {
			t.Errorf("access summary: %s — want roles and read_only, and no name or address", raw)
		}
		if _, err := pool.Exec(ctx, `UPDATE identity.user SET disabled_at = now() WHERE id = $1::uuid`, userID); err != nil {
			t.Fatal(err)
		}
		res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "list_models"})
		if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*sdk.TextContent).Text, "unauthorized") {
			t.Errorf("a disabled account with a still-valid token: %v %+v", err, res)
		}
	})
}
