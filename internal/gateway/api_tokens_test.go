package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A personal API token acts as the person who made it, in what they may do
// and nothing else: read-only stays read-only, a revoked or expired token or
// a disabled account opens nothing, a token cannot manage tokens, and what
// it changes is audited as made through it.
func TestPersonalAPITokens(t *testing.T) {
	f := setupDevAuthoringFixture(t)
	ctx := context.Background()
	// call sends a request as the developer signed in (token "") or with a
	// token alone — no X-Dev-User, which a token must not need.
	call := func(method, path, token string, body any) (int, map[string]any, []any) {
		t.Helper()
		var buf []byte
		if body != nil {
			buf, _ = json.Marshal(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, f.srv.URL+path, bytes.NewReader(buf))
		if token == "" {
			req.Header.Set("X-Dev-User", "dev-audit-pa")
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("X-App-Id", f.appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var raw json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&raw)
		var obj map[string]any
		var arr []any
		if json.Unmarshal(raw, &obj) != nil {
			_ = json.Unmarshal(raw, &arr)
		}
		return resp.StatusCode, obj, arr
	}
	create := func(name, access string) (string, string) {
		t.Helper()
		status, body, _ := call(http.MethodPost, "/api/me/api-tokens", "", map[string]any{"name": name, "access": access, "expires_in_days": 30})
		if status != http.StatusOK {
			t.Fatalf("create %s token: %d %v", access, status, body)
		}
		tok, _ := body["token"].(string)
		item, _ := body["api_token"].(map[string]any)
		if !strings.HasPrefix(tok, apiTokenPrefix) || item == nil {
			t.Fatalf("create answered %v", body)
		}
		return tok, item["id"].(string)
	}
	upload := map[string]string{"csv": "FixtureRevenue\n100\n", "revision_id": f.revID, "import_mode": "replace"}

	writeTok, writeID := create("nightly loader", "write")
	readTok, _ := create("report reader", "read")

	// Only a hash is kept.
	var stored int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM platform.api_token WHERE token_hash = $1 OR hint = $1`, writeTok).Scan(&stored)
	if stored != 0 {
		t.Error("the token itself is stored")
	}
	if _, _, list := call(http.MethodGet, "/api/me/api-tokens", "", nil); len(list) != 2 {
		t.Errorf("the owner's list holds %d tokens, want 2", len(list))
	} else if strings.Contains(tokenListJSON(list), writeTok) {
		t.Error("the list shows a token")
	}

	// The token is the person: their identity, without a sign-in.
	if status, me, _ := call(http.MethodGet, "/api/me", writeTok, nil); status != http.StatusOK || me["email"] != "dev@devaudit.dev" {
		t.Fatalf("GET /api/me with the token: %d %v, want the developer", status, me)
	}
	if status, body, _ := call(http.MethodPost, "/api/import/upload", writeTok, upload); status != http.StatusOK {
		t.Fatalf("an import with the write token: %d %v", status, body)
	}
	var via string
	_ = f.pool.QueryRow(ctx, `SELECT metadata->>'via' FROM audit.audit_event WHERE event_type = 'import.uploaded' ORDER BY occurred_at DESC LIMIT 1`).Scan(&via)
	if via != "api_token:nightly loader" {
		t.Errorf("the import's audit event says via %q, want the token", via)
	}

	// Read-only reads and changes nothing.
	if status, _, _ := call(http.MethodGet, "/api/me", readTok, nil); status != http.StatusOK {
		t.Errorf("a read with the read-only token: %d", status)
	}
	if status, body, _ := call(http.MethodPost, "/api/import/upload", readTok, upload); status != http.StatusForbidden {
		t.Errorf("an import with the read-only token: %d %v, want 403", status, body)
	}

	// A token manages no tokens, its own included.
	if status, _, _ := call(http.MethodPost, "/api/me/api-tokens", writeTok, map[string]any{"name": "another", "access": "write"}); status != http.StatusForbidden {
		t.Errorf("a token making a token: %d, want 403", status)
	}
	if status, _, _ := call(http.MethodGet, "/api/me/api-tokens", writeTok, nil); status != http.StatusForbidden {
		t.Errorf("a token listing tokens: %d, want 403", status)
	}

	// Garbage, expired, revoked, a disabled account: nothing opens.
	if status, _, _ := call(http.MethodGet, "/api/me", apiTokenPrefix+"not-a-token", nil); status != http.StatusUnauthorized {
		t.Errorf("an unknown token: %d, want 401", status)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE platform.api_token SET expires_at = now() - interval '1 second' WHERE name = 'report reader'`); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := call(http.MethodGet, "/api/me", readTok, nil); status != http.StatusUnauthorized {
		t.Errorf("an expired token: %d, want 401", status)
	}
	if status, _, _ := call(http.MethodDelete, "/api/me/api-tokens/"+writeID, "", nil); status != http.StatusOK {
		t.Fatalf("revoke: %d", status)
	}
	if status, _, _ := call(http.MethodGet, "/api/me", writeTok, nil); status != http.StatusUnauthorized {
		t.Errorf("a revoked token: %d, want 401", status)
	}
	if status, _, _ := call(http.MethodDelete, "/api/me/api-tokens/"+writeID, "", nil); status != http.StatusNotFound {
		t.Errorf("revoking a revoked token: %d, want 404", status)
	}
	third, _ := create("third", "write")
	if _, err := f.pool.Exec(ctx, `UPDATE identity.user SET disabled_at = now() WHERE keycloak_sub = 'dev-audit-pa'`); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := call(http.MethodGet, "/api/me", third, nil); status == http.StatusOK {
		t.Errorf("a disabled account's token still opened /api/me")
	}

	var events int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE event_type IN ('api_token.created', 'api_token.revoked')`).Scan(&events)
	if events != 4 {
		t.Errorf("%d token events audited, want 4 (three made, one revoked)", events)
	}
}

// Someone else's token is not theirs to revoke, nor to see.
func TestAPITokensAreTheirOwnersAlone(t *testing.T) {
	f := setupDevAuthoringFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('other-person', 'other@devaudit.dev', 'Other')`); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := f.pool.QueryRow(ctx, `INSERT INTO platform.api_token (keycloak_sub, name, token_hash, hint, access, expires_at)
		VALUES ('other-person', 'theirs', 'x', 'mvx_pat_xxxx', 'write', now() + interval '1 day') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.do(t, http.MethodDelete, "/api/me/api-tokens/"+id, nil); status != http.StatusNotFound {
		t.Errorf("revoking another person's token: %d, want 404", status)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/api/me/api-tokens", nil)
	req.Header.Set("X-Dev-User", "dev-audit-pa")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 0 {
		t.Errorf("the list shows %d tokens of another person", len(list))
	}
}

func tokenListJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
