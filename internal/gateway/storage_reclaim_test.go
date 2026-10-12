package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/plan"
)

// Deleting a model gives its space back at once, in every edition: the
// DELETE re-measures the workspace before it answers, so the tenant listing
// right after shows the smaller figure — and a workspace that was read-only
// for its storage is writable again — with nothing of the model left in
// any table. Before, a dedicated database was measured by its files, which
// PostgreSQL does not shrink when rows are deleted, so the figure did not
// move.
func TestDeletingAModelBringsAWorkspaceBackUnderItsStorageLimit(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	id, workspace, tctx := f.tenant(t, "Playground Co")
	pool, err := f.router.Pool(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	app := seedApp(t, tctx, pool, id, workspace)
	small := seedModel(t, tctx, pool, app, "Small", 3000)
	analyze(t, tctx, pool)
	base, err := plan.StorageBytes(tctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	limitMB := int(base>>20) + 2
	big := seedModel(t, tctx, pool, app, "Big", 80000)
	analyze(t, tctx, pool)

	// The platform admin sets the Community Edition's limit just above the
	// workspace without its big model, and moves the workspace onto it.
	code, body := do(t, f.srv, f.pa, http.MethodGet, "/api/admin/plans", nil)
	var plans []plan.Plan
	if code != http.StatusOK || json.Unmarshal(body, &plans) != nil {
		t.Fatalf("plans: %d %s", code, body)
	}
	for _, p := range plans {
		if p.Key == "community" {
			p.Limits.MaxStorageMB = limitMB
			if code, body := do(t, f.srv, f.pa, http.MethodPut, "/api/admin/plans/community", p); code != http.StatusOK {
				t.Fatalf("set community limit: %d %s", code, body)
			}
		}
	}
	if code, body := do(t, f.srv, f.pa, http.MethodPatch, "/api/admin/tenants/"+id, map[string]string{"plan": "community"}); code != http.StatusOK {
		t.Fatalf("move to community: %d %s", code, body)
	}
	f.plans.InvalidateAll()
	tenant, err := f.plans.Sweep(tctx, pool, id)
	if err != nil || tenant.LimitState != "over" {
		t.Fatalf("sweep with the big model = %+v err=%v, want over a %d MB limit", tenant, err, limitMB)
	}
	listed := f.listedTenant(t, id)
	if listed.Models[big] < 20*listed.Models[small] || listed.Models[small] <= 0 || !listed.ReadOnly {
		t.Fatalf("listed before the delete = %+v, want Big ~27x Small and read-only", listed)
	}

	if code, body := do(t, f.srv, call{persona: "held-pa", tenant: id}, http.MethodDelete, "/api/admin/models/"+big, nil); code != http.StatusOK {
		t.Fatalf("delete Big: %d %s", code, body)
	}
	after := f.listedTenant(t, id)
	t.Logf("storage %d MB with the big model, %d MB right after deleting it (limit %d MB)", listed.Storage>>20, after.Storage>>20, limitMB)
	if after.ReadOnly || after.Storage > listed.Storage/3 {
		t.Fatalf("right after deleting Big: %+v, was %+v", after, listed)
	}
	if len(after.Models) != 1 || after.Models[small] <= 0 {
		t.Fatalf("listed model sizes after the delete = %v", after.Models)
	}
	noRowsOf(t, tctx, pool, big)
}

// An edition without a storage limit shows the space given back too.
func TestDeletingAModelLowersStorageInAnUnlimitedEdition(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	id, workspace, tctx := f.tenant(t, "Enterprise Co") // enterprise: no storage limit
	pool, err := f.router.Pool(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	app := seedApp(t, tctx, pool, id, workspace)
	seedModel(t, tctx, pool, app, "Kept", 3000)
	gone := seedModel(t, tctx, pool, app, "Deleted", 60000)
	analyze(t, tctx, pool)
	if _, err := f.plans.Sweep(tctx, pool, id); err != nil {
		t.Fatal(err)
	}
	before := f.listedTenant(t, id)
	if code, body := do(t, f.srv, call{persona: "held-pa", tenant: id}, http.MethodDelete, "/api/admin/models/"+gone, nil); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	after := f.listedTenant(t, id)
	t.Logf("enterprise storage %d MB -> %d MB right after the delete", before.Storage>>20, after.Storage>>20)
	if after.Storage > before.Storage/3 || before.Models[gone] <= 0 || len(after.Models) != 1 {
		t.Fatalf("enterprise: before %+v, after %+v", before, after)
	}
	noRowsOf(t, tctx, pool, gone)
}

type listedTenant struct {
	Storage  int64
	ReadOnly bool
	Models   map[string]int64
}

// listedTenant is the tenant as GET /api/admin/tenants shows it to the
// platform admin: its storage, whether it is read-only, each model's size.
func (f *dedicatedFixture) listedTenant(t *testing.T, tenantID string) listedTenant {
	t.Helper()
	code, body := do(t, f.srv, f.pa, http.MethodGet, "/api/admin/tenants", nil)
	var tenants []struct {
		ID        string `json:"id"`
		PlanState *struct {
			ReadOnly     bool   `json:"read_only"`
			StorageBytes *int64 `json:"storage_bytes"`
		} `json:"plan_state"`
		Applications []struct {
			Models []struct {
				ID           string `json:"id"`
				StorageBytes *int64 `json:"storage_bytes"`
			} `json:"models"`
		} `json:"applications"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &tenants) != nil {
		t.Fatalf("tenants: %d %s", code, body)
	}
	out := listedTenant{Models: map[string]int64{}}
	for _, tn := range tenants {
		if tn.ID != tenantID {
			continue
		}
		if tn.PlanState != nil {
			out.ReadOnly = tn.PlanState.ReadOnly
			if tn.PlanState.StorageBytes != nil {
				out.Storage = *tn.PlanState.StorageBytes
			}
		}
		for _, a := range tn.Applications {
			for _, m := range a.Models {
				if m.StorageBytes != nil {
					out.Models[m.ID] = *m.StorageBytes
				}
			}
		}
	}
	return out
}

func seedApp(t *testing.T, tctx context.Context, pool *pgxpool.Pool, customerID, workspace string) string {
	t.Helper()
	var app string
	if err := pool.QueryRow(tctx, `INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Planning', 'planning') RETURNING id::text`, customerID, workspace).Scan(&app); err != nil {
		t.Fatal(err)
	}
	return app
}

// seedModel adds a model with facts and calculated results of its own.
func seedModel(t *testing.T, tctx context.Context, pool *pgxpool.Pool, app, name string, cells int) string {
	t.Helper()
	var model, rev, metric string
	if err := pool.QueryRow(tctx, `INSERT INTO core.model (application_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, app, name).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(tctx, `INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, model).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(tctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, 'Amount', true, 'sum') RETURNING id::text`, model, rev).Scan(&metric); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO runtime.fact_input (model_id, revision_id, dim_members, metric_id, value)
		 SELECT $1::uuid, $2::uuid, jsonb_build_object('i', i), $3::uuid, i FROM generate_series(1, $4::int) i`,
		`INSERT INTO runtime.calc_result (model_id, revision_id, dim_members, metric_id, value, partition_key)
		 SELECT $1::uuid, $2::uuid, jsonb_build_object('i', i), $3::uuid, i * 2, 'p' FROM generate_series(1, $4::int) i`,
	} {
		if _, err := pool.Exec(tctx, sql, model, rev, metric, cells); err != nil {
			t.Fatal(err)
		}
	}
	return model
}

func analyze(t *testing.T, tctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(tctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(tctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatal(err)
	}
}

// noRowsOf fails if any table with a model_id column still holds a row of
// the deleted model.
func noRowsOf(t *testing.T, tctx context.Context, pool *pgxpool.Pool, modelID string) {
	t.Helper()
	rows, err := pool.Query(tctx, `
		SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname)
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE a.attname = 'model_id' AND NOT a.attisdropped AND c.relkind IN ('r', 'p') AND NOT c.relispartition
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if len(tables) < 10 {
		t.Fatalf("only %d tables with a model_id column: %v", len(tables), tables)
	}
	for _, table := range tables {
		var n int
		if err := pool.QueryRow(tctx, `SELECT count(*) FROM `+table+` WHERE model_id::text = $1`, modelID).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d rows of the deleted model", table, n)
		}
	}
}
