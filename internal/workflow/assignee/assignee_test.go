package assignee_test

import (
	"context"
	"slices"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/workflow/assignee"
	"github.com/mavericks-engine/mavericks/internal/workflow/assignee/assigneetest"
)

// assignees evaluates the predicate over every user for one application and
// one roles value (a jsonb literal, or SQL NULL when roles is nil).
func assignees(t *testing.T, f *assigneetest.Fixture, app string, roles *string) []string {
	t.Helper()
	rows, err := f.Pool.Query(context.Background(), `
		SELECT u.id::text FROM identity.user u
		WHERE `+assignee.SQL("$1::uuid", "$2::jsonb", "u.id"), app, roles)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return f.Names(ids)
}

func TestSQLDrawsTheWorkspaceBoundary(t *testing.T) {
	f := assigneetest.New(t)
	for _, c := range assigneetest.Cases {
		t.Run(c.Name, func(t *testing.T) {
			roles := `[]`
			if len(c.Roles) > 0 {
				roles = `["` + c.Roles[0] + `"]`
			}
			if got := assignees(t, f, c.App(f), &roles); !slices.Equal(got, c.Want) {
				t.Errorf("assignees = %v, want %v", got, c.Want)
			}
		})
	}

	// SQL NULL names no role, as [] does.
	var open []string
	for _, c := range assigneetest.Cases {
		if c.Name == "no role named, workspace app" {
			open = c.Want
		}
	}
	if got, want := assignees(t, f, f.App1, nil), open; len(want) == 0 || !slices.Equal(got, want) {
		t.Errorf("NULL roles: assignees = %v, want %v", got, want)
	}
	// A value that is not an array assigns the step to no one, rather than
	// failing the query or opening the step to everyone.
	for _, v := range []string{`"business_admin"`, `{"a":1}`, `null`} {
		if got := assignees(t, f, f.App1, &v); len(got) != 0 {
			t.Errorf("roles %s: assignees = %v, want none", v, got)
		}
	}
	// Two named roles add up.
	both := `["business_admin","Approvers"]`
	if got, want := assignees(t, f, f.App1, &both), []string{"appr_1a", "ba_1a", "ba_dev_global", "owner_1"}; !slices.Equal(got, want) {
		t.Errorf("two roles: assignees = %v, want %v", got, want)
	}
}

// A disabled account is an assignee of nothing — it was notified and
// reminded of steps it cannot sign in to see — and is one again once it is
// enabled.
func TestSQLLeavesOutDisabledAccounts(t *testing.T) {
	f := assigneetest.New(t)
	ctx := context.Background()
	named := `["business_admin"]`
	if got := assignees(t, f, f.App1, &named); slices.Contains(got, "ba_1a_off") {
		t.Errorf("disabled business admin is an assignee: %v", got)
	}
	if got := assignees(t, f, f.App1, nil); slices.Contains(got, "ba_1a_off") {
		t.Errorf("disabled business admin is an assignee of a step naming no role: %v", got)
	}
	if _, err := f.Pool.Exec(ctx, `UPDATE identity.user SET disabled_at = NULL WHERE id = $1::uuid`, f.Users["ba_1a_off"]); err != nil {
		t.Fatal(err)
	}
	if got := assignees(t, f, f.App1, &named); !slices.Contains(got, "ba_1a_off") {
		t.Errorf("re-enabled business admin is not an assignee: %v", got)
	}
}

// A business role named like a platform role is not that role, whichever
// platform role it copies; renamed to a name of its own it is a business
// role like any other.
func TestSQLBusinessRoleNamedLikeAPlatformRole(t *testing.T) {
	f := assigneetest.New(t)
	for _, name := range []string{"business_admin", "developer", "tenant_admin", "platform_admin"} {
		roles := `["` + name + `"]`
		for _, app := range []string{f.App1, f.AppT} {
			if got := assignees(t, f, app, &roles); slices.Contains(got, "imp_1a") {
				t.Errorf("member of business role %q is an assignee of a step naming the platform role: %v", name, got)
			}
		}
	}
	if _, err := f.Pool.Exec(context.Background(), `
		UPDATE identity.business_role SET name = 'Tenant admins'
		WHERE workspace_id = $1::uuid AND name = 'tenant_admin'`, f.WS1a); err != nil {
		t.Fatal(err)
	}
	own := `["Tenant admins"]`
	if got, want := assignees(t, f, f.App1, &own), []string{"imp_1a"}; !slices.Equal(got, want) {
		t.Errorf("renamed business role: assignees = %v, want %v", got, want)
	}
}
