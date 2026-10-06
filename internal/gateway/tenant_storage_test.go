// Tests that a tenant admin sees the space their workspace uses and, on a
// plan with a storage limit, the space left — read from the usage sweep's
// measurement, in a dedicated database as on the hosted service. Until this
// the only sign was the read-only banner once the limit was already passed.
package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestTenantAdminSeesStorageSpace(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})

	acme, acmeWS, actx := f.tenant(t, "Acme")
	f.seed(t, actx, "kc-ann", "ann@acme.test", "Ann", "tenant_admin", acmeWS)
	ann := call{persona: "kc-ann"}

	// The hosted plan: 100 MB, set the way the platform admin sets it.
	if code, body := do(t, f.srv, f.pa, http.MethodPatch, "/api/admin/tenants/"+acme, map[string]string{"plan": "community"}); code != http.StatusOK {
		t.Fatalf("move Acme to the test plan: %d %s", code, body)
	}

	type planState struct {
		Plan struct {
			Limits struct {
				MaxStorageMB int `json:"max_storage_mb"`
			} `json:"limits"`
		} `json:"plan"`
		StorageBytes   *int64  `json:"storage_bytes"`
		UsageCheckedAt *string `json:"usage_checked_at"`
	}
	listed := func() planState {
		t.Helper()
		code, body := do(t, f.srv, ann, http.MethodGet, "/api/admin/tenants", nil)
		if code != http.StatusOK {
			t.Fatalf("tenant admin lists tenants: %d %s", code, body)
		}
		var tenants []struct {
			ID        string     `json:"id"`
			PlanState *planState `json:"plan_state"`
		}
		_ = json.Unmarshal(body, &tenants)
		for _, tn := range tenants {
			if tn.ID == acme && tn.PlanState != nil {
				return *tn.PlanState
			}
		}
		t.Fatalf("tenant admin's listing has no plan state for Acme: %s", body)
		return planState{}
	}

	// Before the first sweep there is nothing measured to show.
	if st := listed(); st.StorageBytes != nil || st.Plan.Limits.MaxStorageMB != 100 {
		t.Fatalf("before the sweep: storage %v of %d MB", st.StorageBytes, st.Plan.Limits.MaxStorageMB)
	}

	// The sweep measures Acme's own database — the plan does limit storage
	// here, and a dedicated database is measured on any plan.
	pool, err := f.router.Pool(f.ctx, acme)
	if err != nil {
		t.Fatal(err)
	}
	tn, err := f.plans.Sweep(actx, pool, acme)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if tn.StorageBytes == nil || *tn.StorageBytes < 1<<20 {
		t.Fatalf("sweep recorded %v bytes for a migrated database", tn.StorageBytes)
	}

	st := listed()
	if st.StorageBytes == nil || *st.StorageBytes != *tn.StorageBytes || st.UsageCheckedAt == nil {
		t.Fatalf("tenant admin sees storage %v (checked %v), want %d", st.StorageBytes, st.UsageCheckedAt, *tn.StorageBytes)
	}
	if st.Plan.Limits.MaxStorageMB != 100 {
		t.Fatalf("tenant admin sees a limit of %d MB, want 100", st.Plan.Limits.MaxStorageMB)
	}

	// /api/me carries the same state for the console's other surfaces.
	code, body := do(t, f.srv, ann, http.MethodGet, "/api/me", nil)
	var me struct {
		Plan *planState `json:"plan"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &me) != nil || me.Plan == nil || me.Plan.StorageBytes == nil || *me.Plan.StorageBytes != *tn.StorageBytes {
		t.Fatalf("/api/me for the tenant admin: %d %s", code, body)
	}
}
