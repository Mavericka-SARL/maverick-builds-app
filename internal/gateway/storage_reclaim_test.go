package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/plan"
)

// A workspace over its edition's storage limit gets back under it by
// deleting a model: the tenant listing shows each model's size, deleting the
// big one over HTTP gives its space back at the next sweep, and the
// workspace is writable again. Before, a dedicated database kept the deleted
// rows' space (and the model's facts were copied into cell history on the
// way out), so the measured size did not move.
func TestDeletingAModelBringsAWorkspaceBackUnderItsStorageLimit(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	id, workspace, tctx := f.tenant(t, "Playground Co")
	pool, err := f.router.Pool(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(tctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	app := q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Planning', 'planning') RETURNING id::text`, id, workspace)
	seed := func(name string, facts int) string {
		model := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, app, name)
		rev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, model)
		metric := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, 'Amount', true, 'sum') RETURNING id::text`, model, rev)
		if _, err := pool.Exec(tctx, `INSERT INTO runtime.fact_input (model_id, revision_id, dim_members, metric_id, value)
			SELECT $1::uuid, $2::uuid, jsonb_build_object('i', i), $3::uuid, i FROM generate_series(1, $4::int) i`, model, rev, metric, facts); err != nil {
			t.Fatal(err)
		}
		return model
	}
	small := seed("Small", 3000)
	if _, err := pool.Exec(tctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	var base int64
	if err := pool.QueryRow(tctx, `SELECT sum(pg_total_relation_size(c.oid))::bigint FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'm') AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	limitMB := int(base>>20) + 3
	big := seed("Big", 80000)
	if _, err := pool.Exec(tctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}

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
	sizes := f.modelSizes(t, id)
	if sizes[big] < 20*sizes[small] || sizes[small] <= 0 {
		t.Fatalf("listed model sizes = %v, want Big ~27x Small", sizes)
	}
	before := *tenant.StorageBytes

	if code, body := do(t, f.srv, call{persona: "held-pa", tenant: id}, http.MethodDelete, "/api/admin/models/"+big, nil); code != http.StatusOK {
		t.Fatalf("delete Big: %d %s", code, body)
	}
	if n := q(`SELECT count(*)::text FROM runtime.fact_input_history`); n != "0" {
		t.Fatalf("cell history after deleting the model = %s rows, want 0", n)
	}
	// Statistics see the deleted rows shortly after the transaction ends.
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		var deleted int64
		if err := pool.QueryRow(tctx, `SELECT COALESCE(sum(n_tup_del), 0)::bigint FROM pg_stat_user_tables WHERE relname LIKE 'fact_input%'`).Scan(&deleted); err != nil {
			t.Fatal(err)
		}
		if deleted >= 80000 {
			break
		}
	}

	tenant, err = f.plans.Sweep(tctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("storage %d MB with the big model, %d MB after deleting it (limit %d MB)", before>>20, *tenant.StorageBytes>>20, limitMB)
	if tenant.LimitState != "ok" || *tenant.StorageBytes >= before*2/3 {
		t.Fatalf("after deleting Big: state %s, storage %d (was %d)", tenant.LimitState, *tenant.StorageBytes, before)
	}
	if st, err := f.plans.State(tctx, pool, id); err != nil || st.ReadOnly {
		t.Fatalf("still read-only after deleting Big: %+v err=%v", st, err)
	}
	if sizes := f.modelSizes(t, id); len(sizes) != 1 || sizes[small] <= 0 {
		t.Fatalf("listed model sizes after the delete = %v", sizes)
	}
}

// modelSizes is each model's storage_bytes as GET /api/admin/tenants lists
// it for the platform admin.
func (f *dedicatedFixture) modelSizes(t *testing.T, tenantID string) map[string]int64 {
	t.Helper()
	code, body := do(t, f.srv, f.pa, http.MethodGet, "/api/admin/tenants", nil)
	var tenants []struct {
		ID           string `json:"id"`
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
	out := map[string]int64{}
	for _, tn := range tenants {
		if tn.ID != tenantID {
			continue
		}
		for _, a := range tn.Applications {
			for _, m := range a.Models {
				if m.StorageBytes != nil {
					out[m.ID] = *m.StorageBytes
				}
			}
		}
	}
	return out
}
