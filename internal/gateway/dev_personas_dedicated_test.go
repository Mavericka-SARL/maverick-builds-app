package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
)

// In dedicated mode a tenant's people live in its own database: the dev
// persona list read only the control database, so the persona box offered
// the platform administrator alone and nobody could act as a tenant's
// developer, planner or approver in a local test.
func TestDevPersonasListTenantDatabasesPeople(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	_, wsA, ctxA := f.tenant(t, "Brightfield")
	_, wsB, ctxB := f.tenant(t, "Northwind")
	f.seed(t, ctxA, "dev-erik", "erik@brightfield.test", "Erik", "developer", wsA)
	f.seed(t, ctxB, "bu-ola", "ola@northwind.test", "Ola", "business_user", wsB)

	code, body := do(t, f.srv, call{}, http.MethodGet, "/api/dev/personas", nil)
	if code != http.StatusOK {
		t.Fatalf("personas: %d %s", code, body)
	}
	var list []struct {
		Key    string   `json:"key"`
		Tenant string   `json:"tenant"`
		Roles  []string `json:"roles"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range list {
		got[p.Key] = p.Tenant
	}
	for key, tenant := range map[string]string{"dev-erik": "Brightfield", "bu-ola": "Northwind"} {
		if g, ok := got[key]; !ok || g != tenant {
			t.Errorf("persona %s: tenant %q present=%v, want %q (list %v)", key, g, ok, tenant, got)
		}
	}
	if _, ok := got["held-pa"]; !ok {
		t.Errorf("the platform administrator left the list: %v", got)
	}
}
