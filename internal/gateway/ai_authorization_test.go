// Tests the authorization-parity fix: every /api/ai/... route was gated
// developer_or_admin (developer, platform_admin, OR tenant_admin), while
// every manual developer-console write endpoint it mirrors (metrics,
// dimensions, grids, workflows, forms) requires developer only — a real gap
// letting a platform_admin/tenant_admin with no developer role reach AI
// writes that mutate the exact same model.* rows the manual endpoints
// protect. All /api/ai/... routes are now dev()-gated to match.
package gateway

import (
	"context"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

func TestAIRoutes_RequireDeveloperRole_AdminAloneIsForbidden(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()

	var wsID string
	if err := f.pool.QueryRow(ctx, `SELECT workspace_id::text FROM core.application WHERE id=$1::uuid`, f.appID).Scan(&wsID); err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}

	adminSub := "ai-authz-admin"
	var adminID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ($1, 'admin@promoteco.com', 'Admin', (SELECT customer_id FROM core.application WHERE id=$2::uuid))
		RETURNING id::text
	`, adminSub, f.appID).Scan(&adminID); err != nil {
		t.Fatalf("seed admin user: %v", err)
	}
	// platform_admin only — deliberately no developer role.
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'platform_admin', $2::uuid)`, adminID, wsID); err != nil {
		t.Fatalf("assign platform_admin role: %v", err)
	}

	chatStore := aiassistant.NewChatStore(f.pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session (as the developer owner): %v", err)
	}

	cases := []struct {
		method, path string
	}{
		{"GET", "/api/ai/sessions"},
		{"POST", "/api/ai/sessions"},
		{"GET", "/api/ai/sessions/" + sess.ID},
		{"DELETE", "/api/ai/sessions/" + sess.ID},
		{"POST", "/api/ai/sessions/" + sess.ID + "/messages"},
		{"POST", "/api/ai/sessions/" + sess.ID + "/promote-draft"},
		{"POST", "/api/ai/sessions/" + sess.ID + "/proposals/00000000-0000-0000-0000-000000000000/confirm"},
		{"POST", "/api/ai/sessions/" + sess.ID + "/proposals/00000000-0000-0000-0000-000000000000/reject"},
		{"GET", "/api/ai/settings"},
		{"PUT", "/api/ai/settings"},
		{"POST", "/api/ai/settings/test"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			status, body := f.do(t, c.method, c.path, adminSub, nil)
			if status != http.StatusForbidden {
				t.Errorf("platform_admin (no developer role): status %d, want 403 — body %s", status, body)
			}
		})
	}

	// Sanity: the same routes work for the actual developer, proving the
	// 403s above are a role gate and not a broken fixture/route.
	status, body := f.do(t, "GET", "/api/ai/sessions", f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("developer GET /api/ai/sessions: status %d, body %s", status, body)
	}
}
