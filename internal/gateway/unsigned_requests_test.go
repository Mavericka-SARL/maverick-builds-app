package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/gateway"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// A request with no sign-in to a route open to any role is refused as one,
// never answered as a server fault: GET /api/import/jobs answered 500
// "resolve model: missing Bearer token" (2026-10-23), since the routes that
// resolve the request's model first passed resolveActor's error on to
// jsonAccessErr, which answered everything it did not know with 500.
func TestUnsignedRequestsToAnyRoleRoutesAreNotServerFaults(t *testing.T) {
	ctx := context.Background()
	pool := setupJWTAuthDB(t)
	jf := setupJWKSFixture(t)
	defer jf.srv.Close()
	jwks, err := identity.NewJWKSValidator(ctx, jf.srv.URL, jwtTestRealm, "")
	if err != nil {
		t.Fatalf("new jwks validator: %v", err)
	}
	srv := httptest.NewServer(gateway.NewHandler(logger.New("test"), pool, jwks))
	defer srv.Close()

	const id = "00000000-0000-0000-0000-000000000001"
	wildcard := regexp.MustCompile(`\{[^}]+\}`)
	do := func(t *testing.T, method, path string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Error
	}

	routes := 0
	for _, r := range gateway.BuildRoutes() {
		if r.Auth != "any" {
			continue
		}
		routes++
		method := r.Method
		if method == "" {
			method = http.MethodGet
		}
		path := wildcard.ReplaceAllString(r.Pattern, id)
		if strings.HasSuffix(path, "/") { // a subtree: one item under it
			path += id
		}
		status, msg := do(t, method, path)
		if status >= 500 {
			t.Errorf("%s %s with no sign-in: %d %q, want a refusal", method, path, status, msg)
		}
	}
	if routes == 0 {
		t.Fatal("no route is registered for any role")
	}

	// The route the Playground answered 500 on, and the body it answers.
	if status, msg := do(t, http.MethodGet, "/api/import/jobs"); status != http.StatusUnauthorized || msg != "unauthorized" {
		t.Errorf("GET /api/import/jobs with no sign-in: %d %q, want 401 \"unauthorized\"", status, msg)
	}
}
