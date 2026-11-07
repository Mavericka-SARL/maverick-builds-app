package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A business role's access rules apply to every member (owner request,
// 2026-11-07): about 60 rules for each of 11 people had to be set one person
// at a time, and a new joiner got none. Joining takes effect at once and
// leaving undoes it; a person's own rule never loosens a role's, and
// saving a person's own rules leaves their roles' alone.
func TestRoleAccessRulesApplyToEveryMember(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()

	sees := func(persona, geo string) bool {
		t.Helper()
		status, raw := d.req("GET", "/api/grid?grid_id="+d.gridID, persona, nil)
		if status != http.StatusOK {
			t.Fatalf("grid as %s: %d %s", persona, status, raw)
		}
		var g struct {
			Cells map[string]float64 `json:"cells"`
		}
		_ = json.Unmarshal(raw, &g)
		for k := range g.Cells {
			if strings.HasPrefix(k, d.metric["revenue"]+":") && strings.Contains(":"+k+":", ":"+geo+":") {
				return true
			}
		}
		return false
	}
	writes := func() int {
		t.Helper()
		status, _ := d.writeCell(d.westRep, "revenue", "UK", "LAPTOP", "Q1", 910_000)
		return status
	}

	if !sees(d.westRep, "US") || writes() != http.StatusOK {
		t.Fatal("before the role: the rep should see the US and write the UK")
	}
	role := idOf(d.call("POST", "/api/business-admin/roles", d.admin, map[string]any{"name": "UK planners"}))
	d.call("PUT", "/api/business-admin/roles/"+role+"/access-rules", d.admin, map[string]any{"rules": []map[string]string{
		member(d.geo["AMER"], "hidden"), member(d.geo["UK"], "read"),
	}})
	if status, raw := d.req("PUT", "/api/business-admin/roles/"+role+"/access-rules", d.dev, map[string]any{"rules": []any{}}); status != http.StatusForbidden {
		t.Errorf("a developer setting a role's access rules: %d %s, want 403", status, raw)
	}

	d.call("POST", "/api/business-admin/roles/"+role+"/members", d.admin, map[string]string{"user_id": d.westRepID})
	if sees(d.westRep, "US") {
		t.Error("a member of the role still sees the Americas the role hides")
	}
	if status := writes(); status != http.StatusForbidden {
		t.Errorf("a member of the role wrote to the UK the role makes read-only: %d", status)
	}
	var fromRoles []roleRuleRow
	if err := json.Unmarshal(mustBody(t, d, "GET", "/api/business-admin/users/"+d.westRepID+"/role-access-rules"), &fromRoles); err != nil || len(fromRoles) != 2 || fromRoles[0].RoleName != "UK planners" {
		t.Errorf("the person's rules from roles: %+v (%v), want the role's two", fromRoles, err)
	}

	// The person's own rule says write; the role's read is stricter and wins.
	// Saving the person's own rules leaves the role's in place.
	d.setAccessRules(d.westRepID, member(d.geo["UK"], "write"))
	if status := writes(); status != http.StatusForbidden {
		t.Errorf("a person's own write loosened the role's read: %d", status)
	}
	if sees(d.westRep, "US") {
		t.Error("saving the person's own rules removed the role's")
	}

	d.call("DELETE", "/api/business-admin/roles/"+role+"/members/"+d.westRepID, d.admin, nil)
	if !sees(d.westRep, "US") || writes() != http.StatusOK {
		t.Error("after leaving the role, its rules still apply")
	}
}

func mustBody(t *testing.T, d *salesDemo, method, path string) []byte {
	t.Helper()
	status, raw := d.req(method, path, d.admin, nil)
	if status != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, status, raw)
	}
	return raw
}
