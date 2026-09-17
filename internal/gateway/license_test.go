package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/license"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

func enterpriseManager(t *testing.T) *license.Manager {
	t.Helper()
	return license.Static(&license.Claims{
		ID: "lic-test", Edition: license.EditionEnterprise, Customer: "Acme Corp",
		IssuedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(24 * time.Hour),
	})
}

// requireFeature is the whole point of the license layer: without a key it
// must refuse with a message a person can act on, with one it must pass the
// request through untouched.
func TestRequireFeatureGatesByEdition(t *testing.T) {
	called := false
	inner := func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) }

	t.Run("community refuses with 403 and names the feature", func(t *testing.T) {
		h := &handler{} // nil license manager = community edition
		rec := httptest.NewRecorder()
		h.requireFeature(license.FeatureAuditExport, inner)(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil))
		if rec.Code != http.StatusForbidden || called {
			t.Fatalf("code=%d called=%v", rec.Code, called)
		}
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		for _, want := range []string{"Audit export", "enterprise edition", "community edition"} {
			if !strings.Contains(body["error"], want) {
				t.Errorf("error %q lacks %q", body["error"], want)
			}
		}
	})
	t.Run("commercial unlocks white-label only", func(t *testing.T) {
		h := &handler{lic: license.Static(&license.Claims{Edition: license.EditionCommercial, ExpiresAt: time.Now().Add(time.Hour)})}
		rec := httptest.NewRecorder()
		called = false
		h.requireFeature(license.FeatureWhiteLabel, inner)(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil))
		if rec.Code != http.StatusNoContent || !called {
			t.Fatalf("white-label: code=%d called=%v", rec.Code, called)
		}
		rec = httptest.NewRecorder()
		called = false
		h.requireFeature(license.FeatureSSO, inner)(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil))
		if rec.Code != http.StatusForbidden || called {
			t.Fatalf("sso: code=%d called=%v", rec.Code, called)
		}
	})
	t.Run("enterprise passes through", func(t *testing.T) {
		h := &handler{lic: enterpriseManager(t)}
		rec := httptest.NewRecorder()
		called = false
		h.requireFeature(license.FeatureSSO, inner)(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil))
		if rec.Code != http.StatusNoContent || !called {
			t.Fatalf("code=%d called=%v", rec.Code, called)
		}
	})
}

// GET /api/license is what the console reads; it must work for any signed-in
// role, refuse anonymous callers, and reflect the manager the handler was
// built with.
func TestLicenseInfoEndpoint(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('lic-bu', 'bu@license.dev', 'BU') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'business_user')`, userID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_MODE", "true")

	get := func(t *testing.T, srv *httptest.Server, persona string) (int, license.Status) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/license", nil)
		if persona != "" {
			req.Header.Set("X-Dev-User", persona)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var st license.Status
		_ = json.NewDecoder(resp.Body).Decode(&st)
		return resp.StatusCode, st
	}

	t.Run("community without a key", func(t *testing.T) {
		srv := httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
		defer srv.Close()
		code, st := get(t, srv, "lic-bu")
		if code != http.StatusOK || st.Edition != license.EditionCommunity || st.State != license.StateCommunity || len(st.Catalog) == 0 {
			t.Fatalf("code=%d status=%+v", code, st)
		}
	})
	t.Run("enterprise with a key", func(t *testing.T) {
		srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{License: enterpriseManager(t)}))
		defer srv.Close()
		code, st := get(t, srv, "lic-bu")
		if code != http.StatusOK || st.Edition != license.EditionEnterprise || st.State != license.StateActive || st.Customer != "Acme Corp" {
			t.Fatalf("code=%d status=%+v", code, st)
		}
		if len(st.Features) != len(license.Catalog()) {
			t.Fatalf("enterprise features = %v", st.Features)
		}
	})
	t.Run("anonymous is refused", func(t *testing.T) {
		srv := httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
		defer srv.Close()
		// An unknown persona falls back to the OPEX demo user, which this
		// database does not contain, so actor resolution fails.
		if code, _ := get(t, srv, "nobody-here"); code != http.StatusUnauthorized {
			t.Fatalf("code=%d, want 401", code)
		}
	})
}
