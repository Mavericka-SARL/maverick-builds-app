package gateway

// Dashboards' assignment rule on the list, detail, folder and chart-data
// routes: a business role's grants, which an administrator of the
// dashboard's own application passes — and a role held for another tenant
// or workspace does not (dashboardAdminBypass).

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestDashboardAdminBypassIsScoped(t *testing.T) {
	f := setupSalesDemo(t)
	revenue := f.metric["revenue"]

	dash := func(name string) string {
		return idOf(f.call("POST", "/api/developer/dashboards", f.dev, map[string]any{"name": name, "revision_id": f.revID}))
	}
	widget := func(dashID string, body map[string]any) string {
		return idOf(f.call("POST", "/api/developer/dashboards/"+dashID+"/widgets", f.dev, body))
	}
	overview, private := dash("Overview"), dash("Board pack")
	widget(overview, map[string]any{"widget_type": "metric_kpi", "ref_id": revenue, "widget_props": map[string]any{"kpi_context_mode": "total"}})
	widget(private, map[string]any{"widget_type": "metric_kpi", "ref_id": revenue, "widget_props": map[string]any{"kpi_context_mode": "total"}})

	// A business role grants Reed the overview and nothing else.
	role := idOf(f.call("POST", "/api/business-admin/roles", f.admin, map[string]any{"name": "Sales readers"}))
	f.call("PUT", "/api/business-admin/roles/"+role+"/dashboards", f.admin, map[string]any{"dashboard_ids": []string{overview}})
	f.call("POST", "/api/business-admin/roles/"+role+"/members", f.admin, map[string]any{"user_id": f.roID})

	t.Run("assignments decide what is listed and read", func(t *testing.T) {
		if status, raw := f.req("GET", "/api/dashboards", f.ro, nil); status != http.StatusOK || strings.Contains(string(raw), private) || !strings.Contains(string(raw), overview) {
			t.Errorf("REST list for Reed: %d %s — want the overview only", status, raw)
		}
		if status, _ := f.req("GET", "/api/dashboards/"+private, f.ro, nil); status != http.StatusNotFound {
			t.Errorf("REST detail of the unassigned dashboard for Reed: %d", status)
		}
	})

	t.Run("an application's business administrator reads every dashboard", func(t *testing.T) {
		if status, raw := f.req("GET", "/api/dashboards/"+private, f.admin, nil); status != http.StatusOK {
			t.Errorf("REST detail for Avery: %d %s (list and detail now agree)", status, raw)
		}
	})

	// A developer is a builder of every application of their own tenant,
	// so a developer of another workspace of this tenant does administer
	// it. What counts for nothing here is a role held for another tenant,
	// or administration of another workspace: each such person, a business
	// user here in Reed's role, sees only what the role grants.
	for _, c := range []struct{ name, sub, role, otherTenant string }{
		{"a developer of another tenant", "demo-xdev", "developer", "yes"},
		{"a business administrator of another workspace", "demo-xba", "business_admin", ""},
	} {
		t.Run(c.name+" is not an administrator here", func(t *testing.T) {
			ctx := context.Background()
			cust := f.custID
			if c.otherTenant != "" {
				_ = f.pool.QueryRow(ctx, `INSERT INTO core.customer (name, plan) VALUES ('Elsewhere Co', 'enterprise') RETURNING id::text`).Scan(&cust)
			}
			var ws2, uid string
			_ = f.pool.QueryRow(ctx, `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Elsewhere') RETURNING id::text`, cust).Scan(&ws2)
			_ = f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1, $1 || '@demo.co', $1) RETURNING id::text`, c.sub).Scan(&uid)
			for _, ra := range [][2]string{{c.role, ws2}, {"business_user", f.wsID}} {
				if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2::identity.user_role, $3::uuid)`, uid, ra[0], ra[1]); err != nil {
					t.Fatal(err)
				}
			}
			f.call("POST", "/api/business-admin/roles/"+role+"/members", f.admin, map[string]any{"user_id": uid})
			if status, raw := f.req("GET", "/api/dashboards", c.sub, nil); status != http.StatusOK || strings.Contains(string(raw), private) || !strings.Contains(string(raw), overview) {
				t.Errorf("REST list: %d %s — want the overview and not the unassigned dashboard", status, raw)
			}
			if status, _ := f.req("GET", "/api/dashboards/"+private, c.sub, nil); status != http.StatusNotFound {
				t.Errorf("REST detail of the unassigned dashboard: %d, want 404", status)
			}
			if status, raw := f.req("GET", "/api/folders", c.sub, nil); status != http.StatusOK {
				t.Errorf("REST folders: %d %s", status, raw)
			}
		})
	}
}
