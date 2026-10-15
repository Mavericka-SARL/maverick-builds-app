package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// With a database per tenant, a token is resolved to its person before the
// request is routed: a token made by someone in tenant A is served in A, and
// naming tenant B does not take it there.
func TestAPITokenIsServedInItsOwnersTenant(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	a, wsA, ctxA := f.tenant(t, "Token A")
	b, _, _ := f.tenant(t, "Token B")
	f.seed(t, ctxA, "ana-sub", "ana@a.test", "Ana", "tenant_admin", wsA)

	code, body := do(t, f.srv, call{persona: "ana-sub"}, http.MethodPost, "/api/me/api-tokens", map[string]any{"name": "loader", "access": "read"})
	var made struct {
		Token string `json:"token"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &made) != nil || made.Token == "" {
		t.Fatalf("create: %d %s", code, body)
	}
	var held int
	if err := f.control.QueryRow(f.ctx, `SELECT count(*) FROM platform.api_token WHERE keycloak_sub = 'ana-sub'`).Scan(&held); err != nil || held != 1 {
		t.Fatalf("the control plane holds %d of Ana's tokens (%v), want 1", held, err)
	}

	me := func(tenant string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, f.srv.URL+"/api/me", nil)
		req.Header.Set("Authorization", "Bearer "+made.Token)
		if tenant != "" {
			req.Header.Set("X-Tenant-Id", tenant)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	if code, out := me(""); code != http.StatusOK || out["email"] != "ana@a.test" || out["customer_id"] != a {
		t.Fatalf("GET /api/me with Ana's token: %d %v, want Ana in tenant %s", code, out, a)
	}
	if code, out := me(b); code == http.StatusOK && out["customer_id"] == b {
		t.Errorf("Ana's token naming tenant %s was served there: %v", b, out)
	}
}
