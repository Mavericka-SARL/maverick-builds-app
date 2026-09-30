package gateway

// Account boundaries (decided 2026-09-30, account_boundaries.go): anyone but
// a platform admin renames, deletes, re-invites an account only when its home
// (identity.user customer_id) is one of the caller's tenants; an account of
// another tenant, or of none, is listed and removed from the tenant instead;
// inviting an address that already has an account adds it inside a workspace
// and changes nothing else about it.
//
// These reuse setupDevRouteFixture: tenant 1 has ws1a (app1, models A and
// B), ws1b (app2) and appT; tenant 2 has ws2 (app3, model D).
// mm-tenant-admin is tenant 1's admin, mm-platform a platform admin, ws-dev-ba
// a developer of ws1a.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mavericks-engine/mavericks/internal/notification"
)

// listedPermissions is AdminUser.permissions as the console reads it.
type listedPermissions struct {
	Rename           bool `json:"rename"`
	Delete           bool `json:"delete"`
	Disable          bool `json:"disable"`
	Reinvite         bool `json:"reinvite"`
	RemoveFromTenant bool `json:"remove_from_tenant"`
}

type listedUser struct {
	ID          string            `json:"id"`
	HomeTenant  string            `json:"home_tenant"`
	Permissions listedPermissions `json:"permissions"`
}

// listUsers is GET /api/admin/users as sub, by id.
func (f *devRouteFixture) listUsers(t *testing.T, sub string) map[string]listedUser {
	t.Helper()
	var users []listedUser
	if err := json.Unmarshal(f.expect(t, sub, "GET", "/api/admin/users", "", nil, http.StatusOK), &users); err != nil {
		t.Fatal(err)
	}
	out := map[string]listedUser{}
	for _, u := range users {
		out[u.ID] = u
	}
	return out
}

// accountRow is what the account-level actions may change.
func (f *devRouteFixture) accountRow(t *testing.T, id string) string {
	t.Helper()
	return f.one(t, `SELECT keycloak_sub||'|'||email||'|'||display_name||'|'||COALESCE(customer_id::text,'-')||'|'||(disabled_at IS NULL)::text
		FROM identity.user WHERE id=$1::uuid`, id)
}

func TestAccountLevelActionsOnlyOnOwnAccounts(t *testing.T) {
	f := setupDevRouteFixture(t)
	ctx := context.Background()
	const ta = "mm-tenant-admin"
	taID := f.one(t, `SELECT id::text FROM identity.user WHERE keycloak_sub=$1`, ta)
	paID := f.one(t, `SELECT id::text FROM identity.user WHERE keycloak_sub='mm-platform'`)
	own := f.gbUser(t, "ab-own", f.cust1, [2]string{"business_user", f.ws1a})
	other := f.gbUser(t, "ab-other", f.cust2, [2]string{"business_user", f.ws1a})
	none := f.gbUser(t, "ab-none", "", [2]string{"business_user", f.ws1a})
	coAdmin := f.gbUser(t, "ab-coadmin", f.cust2, [2]string{"tenant_admin", f.ws1a})

	// Another tenant's account, and one of none: listed, not changed.
	for _, id := range []string{other, none} {
		before := f.accountRow(t, id)
		raw := f.expect(t, ta, "PATCH", "/api/admin/users/"+id, "", map[string]string{"display_name": "Renamed"}, http.StatusForbidden)
		if !strings.Contains(string(raw), "remove it from your tenant") {
			t.Errorf("rename refusal names no remedy: %s", raw)
		}
		f.expect(t, ta, "POST", "/api/admin/users/"+id+"/invite", "", nil, http.StatusForbidden)
		f.expect(t, ta, "DELETE", "/api/admin/users/"+strings.ToUpper(id), "", nil, http.StatusForbidden)
		if got := f.accountRow(t, id); got != before {
			t.Errorf("refused actions changed %s: %q → %q", id, before, got)
		}
	}
	// A developer administering users draws the same line.
	f.expect(t, "ws-dev-ba", "PATCH", "/api/admin/users/"+other, "", map[string]string{"display_name": "Renamed"}, http.StatusForbidden)
	f.expect(t, "ws-dev-ba", "DELETE", "/api/admin/users/"+none, "", nil, http.StatusForbidden)

	// A role held with no workspace is not the tenant's: neither given to
	// nor taken from an account whose home is elsewhere.
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'business_user'), ($2::uuid, 'business_user')`, none, own); err != nil {
		t.Fatal(err)
	}
	f.expect(t, ta, "DELETE", "/api/admin/users/"+none+"/roles/business_user", "", nil, http.StatusForbidden)
	f.expect(t, ta, "POST", "/api/admin/users/"+other+"/roles", "", map[string]string{"role": "business_user"}, http.StatusForbidden)
	f.expect(t, ta, "DELETE", "/api/admin/users/"+own+"/roles/business_user", "", nil, http.StatusOK)
	if n := f.one(t, `SELECT count(*)::text FROM identity.role_assignment WHERE user_id=ANY($1::uuid[]) AND workspace_id IS NULL`, []string{none, other}); n != "1" {
		t.Errorf("unscoped grants of the foreign accounts: %s, want the 1 there was", n)
	}
	// Inside the caller's workspaces they stay the caller's to change.
	f.expect(t, ta, "POST", "/api/admin/users/"+other+"/roles", "", map[string]string{"role": "business_admin", "workspace_id": f.ws1b}, http.StatusOK)
	f.expect(t, ta, "DELETE", "/api/admin/users/"+other+"/roles/business_admin?workspace_id="+f.ws1b, "", nil, http.StatusOK)

	// What the console is told, computed by the rule the mutations use.
	want := map[string]listedUser{
		own:     {HomeTenant: "own", Permissions: listedPermissions{Rename: true, Delete: true, Disable: true, Reinvite: true}},
		other:   {HomeTenant: "other", Permissions: listedPermissions{RemoveFromTenant: true}},
		none:    {HomeTenant: "none", Permissions: listedPermissions{RemoveFromTenant: true}},
		coAdmin: {HomeTenant: "other"}, // tenant_admin there is a platform admin's to remove
		taID:    {HomeTenant: "own", Permissions: listedPermissions{Rename: true, Disable: true, Reinvite: true}},
		paID:    {HomeTenant: "own"},
	}
	listed := f.listUsers(t, ta)
	for id, w := range want {
		got, ok := listed[id]
		w.ID = id
		if !ok || got != w {
			t.Errorf("tenant admin's list, %s: %+v (listed %v), want %+v", id, got, ok, w)
		}
	}
	// A developer removes no one from the tenant: that is resource access.
	if got := f.listUsers(t, "ws-dev-ba")[other]; got.Permissions != (listedPermissions{}) || got.HomeTenant != "other" {
		t.Errorf("developer's list, other tenant's account: %+v", got)
	}
	// A platform admin is unchanged: every action but deleting itself.
	pl := f.listUsers(t, "mm-platform")
	for id, w := range map[string]listedPermissions{
		other: {Rename: true, Delete: true, Disable: true, Reinvite: true},
		none:  {Rename: true, Delete: true, Disable: true, Reinvite: true},
		paID:  {Rename: true, Disable: true, Reinvite: true},
	} {
		if pl[id].Permissions != w {
			t.Errorf("platform admin's list, %s: %+v, want %+v", id, pl[id].Permissions, w)
		}
	}
	f.expect(t, "mm-platform", "PATCH", "/api/admin/users/"+none, "", map[string]string{"display_name": "Renamed by platform"}, http.StatusOK)

	// The tenant's own account is the tenant admin's, as before.
	f.expect(t, ta, "PATCH", "/api/admin/users/"+strings.ToUpper(own), "", map[string]string{"display_name": "Own Renamed"}, http.StatusOK)
	f.expect(t, ta, "POST", "/api/admin/users/"+own+"/invite", "", nil, http.StatusServiceUnavailable) // no identity provider here
	f.expect(t, ta, "DELETE", "/api/admin/users/"+own, "", nil, http.StatusOK)
	if n := f.one(t, `SELECT count(*)::text FROM identity.user WHERE id=$1::uuid`, own); n != "0" {
		t.Errorf("own account not deleted")
	}
}

func TestRemoveAccountFromTenant(t *testing.T) {
	f := setupDevRouteFixture(t)
	ctx := context.Background()
	const ta = "mm-tenant-admin"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	role2 := f.one(t, `INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Tenant two reviewers') RETURNING id::text`, f.ws2)

	// A member of tenant 1 whose home is tenant 2: roles, grants, business
	// roles and access rules in both.
	m := f.gbUser(t, "ab-member", f.cust2, [2]string{"business_user", f.ws1a}, [2]string{"developer", f.ws1b}, [2]string{"business_user", f.ws2})
	exec(`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid), ($1::uuid, $3::uuid), ($1::uuid, $4::uuid)`, m, f.app1, f.appT, f.app3)
	exec(`INSERT INTO identity.user_model_access (user_id, model_id) VALUES ($1::uuid, $2::uuid), ($1::uuid, $3::uuid)`, m, f.modelA, f.modelD)
	exec(`INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $3::uuid), ($2::uuid, $3::uuid)`, f.roleID, role2, m)
	exec(`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES
		($1::uuid, 'metric', $2, 'read'), ($1::uuid, 'metric', $3, 'read'),
		($1::uuid, 'dimension_member', upper($4), 'hidden'), ($1::uuid, 'dimension_member', $5, 'hidden'),
		($1::uuid, 'button', 'btn-1', 'hidden')`, m, f.metricA, f.metricD, f.memberA, f.memberD)
	before := f.accountRow(t, m)

	// Refused: a developer (resource access is not theirs), a platform admin
	// (its scope names no one tenant), the tenant's own account, the caller
	// itself, and an account holding a role there only a platform admin
	// removes.
	f.expect(t, "ws-dev-ba", "DELETE", "/api/admin/users/"+m+"/tenant-access", "", nil, http.StatusForbidden)
	f.expect(t, "mm-platform", "DELETE", "/api/admin/users/"+m+"/tenant-access", "", nil, http.StatusBadRequest)
	own := f.gbUser(t, "ab-own", f.cust1, [2]string{"business_user", f.ws1a})
	f.expect(t, ta, "DELETE", "/api/admin/users/"+own+"/tenant-access", "", nil, http.StatusBadRequest)
	taID := f.one(t, `SELECT id::text FROM identity.user WHERE keycloak_sub=$1`, ta)
	f.expect(t, ta, "DELETE", "/api/admin/users/"+taID+"/tenant-access", "", nil, http.StatusForbidden)
	coAdmin := f.gbUser(t, "ab-coadmin", f.cust2, [2]string{"tenant_admin", f.ws1a}, [2]string{"business_user", f.ws1b})
	f.expect(t, ta, "DELETE", "/api/admin/users/"+coAdmin+"/tenant-access", "", nil, http.StatusForbidden)
	if n := f.one(t, `SELECT count(*)::text FROM identity.role_assignment WHERE user_id=$1::uuid`, coAdmin); n != "2" {
		t.Errorf("refused removal left %s of the co-admin's 2 roles", n)
	}
	pa := f.gbUser(t, "ab-pa", "", [2]string{"platform_admin", ""}, [2]string{"business_user", f.ws1a})
	f.expect(t, ta, "DELETE", "/api/admin/users/"+pa+"/tenant-access", "", nil, http.StatusForbidden)

	// Removed: everything it holds in tenant 1, nothing else.
	raw := f.expect(t, ta, "DELETE", "/api/admin/users/"+strings.ToUpper(m)+"/tenant-access", "", nil, http.StatusOK)
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil || resp["status"] != "removed" || resp["revoked"] != nil {
		t.Errorf("removal answered %s", raw)
	}
	for what, c := range map[string]struct{ sql, want string }{
		"roles": {`SELECT string_agg(role::text||'@'||COALESCE(workspace_id::text,''), ',') FROM identity.role_assignment WHERE user_id=$1::uuid`,
			"business_user@" + f.ws2},
		"app grants":     {`SELECT string_agg(application_id::text, ',') FROM identity.user_app_access WHERE user_id=$1::uuid`, f.app3},
		"model grants":   {`SELECT string_agg(model_id::text, ',') FROM identity.user_model_access WHERE user_id=$1::uuid`, f.modelD},
		"business roles": {`SELECT string_agg(role_id::text, ',') FROM identity.business_role_member WHERE user_id=$1::uuid`, role2},
		"access rules": {`SELECT string_agg(rule_type||':'||ref_id, ',' ORDER BY rule_type, ref_id) FROM identity.user_access_rule WHERE user_id=$1::uuid`,
			"button:btn-1,dimension_member:" + f.memberD + ",metric:" + f.metricD},
	} {
		if got := f.one(t, "SELECT COALESCE(("+c.sql+"), '')", m); got != c.want {
			t.Errorf("%s after the removal: %q, want %q", what, got, c.want)
		}
	}
	if got := f.accountRow(t, m); got != before {
		t.Errorf("the account itself changed: %q → %q", before, got)
	}
	if n := f.one(t, `SELECT count(*)::text FROM audit.audit_event WHERE event_type='user.role_revoked' AND resource_id=$1
		AND metadata->>'action'='removed_from_tenant' AND metadata->>'app_access'='2' AND metadata->>'access_rules'='2'`, m); n != "1" {
		t.Errorf("%s removal audit events, want 1", n)
	}
	// It is no longer the tenant's to see.
	if _, listed := f.listUsers(t, ta)[m]; listed {
		t.Errorf("removed account still listed")
	}
	f.expect(t, ta, "DELETE", "/api/admin/users/"+m+"/tenant-access", "", nil, http.StatusForbidden)

	// A developer with no tenant narrowed to tenant 1's application: removed,
	// its last grant goes, and its developer grant with it.
	nd := f.gbUser(t, "ab-narrowed", "", [2]string{"developer", ""}, [2]string{"business_user", f.ws1a})
	exec(`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, nd, f.app1)
	raw = f.expect(t, ta, "DELETE", "/api/admin/users/"+nd+"/tenant-access", "", nil, http.StatusOK)
	if !strings.Contains(string(raw), "ab-narrowed@gb.test") {
		t.Errorf("removal does not report the revoked developer grant: %s", raw)
	}
	f.assertNoBuilderReach(t, "ab-narrowed", nd, "removed_from_tenant")
}

func TestInviteExistingAccountAddsItToTheTenant(t *testing.T) {
	f := setupDevRouteFixture(t)
	ctx := context.Background()
	const ta = "mm-tenant-admin"
	taID := f.one(t, `SELECT id::text FROM identity.user WHERE keycloak_sub=$1`, ta)
	invite := func(email, role, ws string, want int) map[string]any {
		t.Helper()
		body := map[string]string{"email": email, "first_name": "Some", "last_name": "One", "role": role}
		if ws != "" {
			body["workspace_id"] = ws
		}
		var out map[string]any
		_ = json.Unmarshal(f.expect(t, ta, "POST", "/api/admin/users", "", body, want), &out)
		return out
	}
	keys := func(m map[string]any) []string {
		var k []string
		for key := range m {
			k = append(k, key)
		}
		slices.Sort(k)
		return k
	}

	ex := f.gbUser(t, "ab-existing", f.cust2, [2]string{"business_user", f.ws2})
	if _, err := f.pool.Exec(ctx, `UPDATE identity.user SET disabled_at = now() WHERE id=$1::uuid`, ex); err != nil {
		t.Fatal(err)
	}
	before := f.accountRow(t, ex)
	// Tenant 1 sends notifications by e-mail; the account's own tenant 2
	// does not. The news is tenant 1's, so its choice decides.
	if _, err := notification.NewStore(f.pool).UpdateSettings(ctx, f.cust1, notification.Settings{EmailEnabled: true}); err != nil {
		t.Fatal(err)
	}

	// What cannot be given to a new account cannot be given to this one.
	invite("ab-existing@gb.test", "tenant_admin", f.ws1b, http.StatusForbidden)
	invite("ab-existing@gb.test", "business_user", f.ws2, http.StatusForbidden)

	fresh := invite("ab-fresh@gb.test", "business_user", f.ws1b, http.StatusOK)
	// An existing account is added inside a workspace, or not at all — and
	// answered as a new address sent the same request is: no error naming
	// the account, and an id that is not its own.
	for i, c := range [][2]string{{"business_user", ""}, {"", ""}} {
		got := invite("ab-existing@gb.test", c[0], c[1], http.StatusOK)
		newAddr := invite(fmt.Sprintf("ab-new-%d@gb.test", i), c[0], c[1], http.StatusOK)
		if !slices.Equal(keys(got), keys(newAddr)) || got["status"] != newAddr["status"] || got["invited"] != newAddr["invited"] || got["id"] == ex {
			t.Errorf("existing address with role %q and no workspace answered %v, a new one %v", c[0], got, newAddr)
		}
	}
	added := invite("AB-Existing@GB.test", "business_user", f.ws1b, http.StatusOK)
	if !slices.Equal(keys(added), keys(fresh)) || added["status"] != fresh["status"] || added["invited"] != fresh["invited"] {
		t.Errorf("adding an existing account answered %v, a new invitation %v", added, fresh)
	}
	if added["id"] != ex {
		t.Errorf("answered id %v, want the account's %s", added["id"], ex)
	}
	// Developer is given inside the workspace, never without one.
	invite("ab-existing@gb.test", "developer", f.ws1a, http.StatusOK)
	if got := f.one(t, `SELECT string_agg(role::text||'@'||COALESCE(workspace_id::text,'')||'@'||COALESCE(assigned_by::text,''), ',' ORDER BY role::text, workspace_id = $2::uuid)
		FROM identity.role_assignment WHERE user_id=$1::uuid`, ex, f.ws1b); got !=
		"business_user@"+f.ws2+"@,business_user@"+f.ws1b+"@"+taID+",developer@"+f.ws1a+"@"+taID {
		t.Errorf("the account's roles: %q", got)
	}
	if got := f.accountRow(t, ex); got != before {
		t.Errorf("the account itself changed: %q → %q", before, got)
	}
	f.renameMetric(t, "ab-existing", f.metricD, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound)

	// Told, in the notification centre, where it was given access, in the
	// platform's words.
	if got := f.one(t, `SELECT count(*)::text||'|'||min(template_vars->>'subject')||'|'||min(template_vars->>'message') FROM notification.notification
		WHERE recipient_user_id=$1::uuid AND template_id='access_granted' AND channel='in_app' AND resource_type='workspace' AND resource_id=$2`, ex, f.ws1b); !strings.HasPrefix(got, "1|You were given access to ") ||
		!strings.Contains(got, "“ws1b”") || strings.Contains(got, ta) {
		t.Errorf("access-granted notifications for ws1b: %q", got)
	}
	if n := f.one(t, `SELECT count(*)::text FROM notification.notification
		WHERE recipient_user_id=$1::uuid AND template_id='access_granted' AND channel='email'`, ex); n != "2" {
		t.Errorf("%s access-granted e-mails queued, want 2 (tenant 1 sends e-mail)", n)
	}
	if n := f.one(t, `SELECT count(*)::text FROM audit.audit_event WHERE event_type='user.role_granted' AND resource_id=$1
		AND metadata->>'existing_account'='true' AND metadata->>'granted'='true'`, ex); n != "2" {
		t.Errorf("%s audited additions, want 2", n)
	}
	// The refusals are audited, for the platform only.
	if n := f.one(t, `SELECT count(*)::text FROM audit.audit_event WHERE event_type='user.role_granted' AND resource_id=$1
		AND metadata->>'granted'='false' AND metadata->>'refused' <> '' AND metadata->>'visibility'='platform'`, ex); n != "2" {
		t.Errorf("%s audited refusals without a workspace, want 2", n)
	}

	// Added, removed and added again: told once a day per workspace.
	f.expect(t, ta, "DELETE", "/api/admin/users/"+ex+"/tenant-access", "", nil, http.StatusOK)
	invite("ab-existing@gb.test", "business_user", f.ws1b, http.StatusOK)
	if n := f.one(t, `SELECT count(*)::text FROM notification.notification
		WHERE recipient_user_id=$1::uuid AND template_id='access_granted' AND channel='in_app'`, ex); n != "2" {
		t.Errorf("%s access-granted notifications after adding it again, want the 2 there were", n)
	}
	if n := f.one(t, `SELECT count(*)::text FROM identity.role_assignment WHERE user_id=$1::uuid AND workspace_id=$2::uuid`, ex, f.ws1b); n != "1" {
		t.Errorf("added again: %s roles in ws1b, want 1", n)
	}

	// Tenant 1's news leaves under the platform's name, not the brand of the
	// account's own tenant.
	mail := &brandRecordingMailer{}
	d := &notification.Dispatcher{Store: notification.NewStore(f.pool), Mailer: mail,
		BrandName: func(context.Context, string) string { return "Tenant Two Brand" }}
	if _, err := d.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	mail.sent = slices.DeleteFunc(mail.sent, func(m brandedMail) bool { return m.to != "ab-existing@gb.test" })
	if len(mail.sent) != 2 || slices.ContainsFunc(mail.sent, func(m brandedMail) bool { return m.brand != "" }) {
		t.Errorf("access-granted e-mails sent: %+v, want 2 unbranded", mail.sent)
	}
	if _, err := notification.NewStore(f.pool).UpdateSettings(ctx, f.cust2, notification.Settings{EmailEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := notification.NewStore(f.pool).Notify(ctx, ex, "own_news", map[string]string{"subject": "Tenant two news"}, "workspace", f.ws2); err != nil {
		t.Fatal(err)
	}
	mail.sent = nil
	if _, err := d.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	mail.sent = slices.DeleteFunc(mail.sent, func(m brandedMail) bool { return m.to != "ab-existing@gb.test" })
	if len(mail.sent) != 1 || mail.sent[0].brand != "Tenant Two Brand" {
		t.Errorf("its own tenant's e-mail: %+v, want 1 branded", mail.sent)
	}

	// A platform admin's account is never changed: the same answer, nothing
	// granted, the refusal audited.
	pa := f.gbUser(t, "ab-pa", "", [2]string{"platform_admin", ""})
	paBefore := f.accountRow(t, pa)
	got := invite("ab-pa@gb.test", "business_user", f.ws1b, http.StatusOK)
	if !slices.Equal(keys(got), keys(fresh)) || got["status"] != fresh["status"] || got["id"] == pa {
		t.Errorf("inviting a platform admin's address answered %v, a new invitation %v", got, fresh)
	}
	if n := f.one(t, `SELECT count(*)::text FROM identity.role_assignment WHERE user_id=$1::uuid`, pa); n != "1" {
		t.Errorf("platform admin's account has %s roles, want its 1", n)
	}
	if f.accountRow(t, pa) != paBefore {
		t.Errorf("platform admin's account changed")
	}
	if n := f.one(t, `SELECT count(*)::text FROM notification.notification WHERE recipient_user_id=$1::uuid`, pa); n != "0" {
		t.Errorf("platform admin was notified of a grant it did not get")
	}
	if n := f.one(t, `SELECT count(*)::text FROM audit.audit_event WHERE event_type='user.role_granted' AND resource_id=$1
		AND metadata->>'existing_account'='true' AND metadata->>'granted'='false' AND metadata->>'refused' <> ''`, pa); n != "1" {
		t.Errorf("%s audited refusals, want 1", n)
	}
	// Shown to the platform, not to the tenant: it said whose the address was.
	if auditedFor(t, f, ta, pa) {
		t.Errorf("the tenant admin's audit log shows the refusal to add a platform admin")
	}
	if !auditedFor(t, f, "mm-platform", pa) {
		t.Errorf("the platform admin's audit log does not show the refusal")
	}
}

// auditedFor reports whether GET /api/admin/audit, as sub, shows an event
// about the account id.
func auditedFor(t *testing.T, f *devRouteFixture, sub, id string) bool {
	t.Helper()
	var events []struct {
		ResourceID string `json:"resource_id"`
	}
	if err := json.Unmarshal(f.expect(t, sub, "GET", "/api/admin/audit", "", nil, http.StatusOK), &events); err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(events, func(e struct {
		ResourceID string `json:"resource_id"`
	}) bool {
		return e.ResourceID == id
	})
}

type brandedMail struct{ brand, to, subject string }

// brandRecordingMailer records what the notification dispatcher sends, and under
// which brand.
type brandRecordingMailer struct{ sent []brandedMail }

func (m *brandRecordingMailer) Send(_ context.Context, to, _, subject, _ string) error {
	m.sent = append(m.sent, brandedMail{to: to, subject: subject})
	return nil
}

func (m *brandRecordingMailer) SendAs(_ context.Context, brand, to, _, subject, _ string) error {
	m.sent = append(m.sent, brandedMail{brand: brand, to: to, subject: subject})
	return nil
}

// Changing an account takes being able to revoke every role it holds: a
// developer — or a tenant admin elsewhere holding developer in one of the
// tenant's workspaces — renamed, re-invited and deleted the tenant's
// administrator, identity-provider account and all, though it may not
// revoke that administrator's role (2026-09-30).
func TestAccountActionsNeedEveryRoleRevocable(t *testing.T) {
	f := setupDevRouteFixture(t)
	const ta = "mm-tenant-admin"
	taID := f.one(t, `SELECT id::text FROM identity.user WHERE keycloak_sub=$1`, ta)
	f.gbUser(t, "ab-t2-admin-dev", f.cust2, [2]string{"tenant_admin", ""}, [2]string{"developer", f.ws1a})
	peer := f.gbUser(t, "ab-peer-admin", f.cust1, [2]string{"tenant_admin", ""})
	dev := f.gbUser(t, "ab-own-dev", f.cust1, [2]string{"developer", f.ws1b})
	bu := f.gbUser(t, "ab-own-bu", f.cust1, [2]string{"business_user", f.ws1a})

	before := f.accountRow(t, taID)
	for _, sub := range []string{"ws-dev-ba", "ab-t2-admin-dev"} {
		if got := f.listUsers(t, sub)[taID]; got.HomeTenant != "own" || got.Permissions != (listedPermissions{}) {
			t.Errorf("%s's list, the tenant admin: %+v, want own and no action", sub, got)
		}
		raw := f.expect(t, sub, "DELETE", "/api/admin/users/"+taID, "", nil, http.StatusForbidden)
		if !strings.Contains(string(raw), "tenant_admin") {
			t.Errorf("refusal does not name the role: %s", raw)
		}
		f.expect(t, sub, "PATCH", "/api/admin/users/"+taID, "", map[string]string{"display_name": "Taken"}, http.StatusForbidden)
		f.expect(t, sub, "POST", "/api/admin/users/"+taID+"/invite", "", nil, http.StatusForbidden)
	}
	if got := f.accountRow(t, taID); got != before {
		t.Errorf("the tenant admin's account changed: %q → %q", before, got)
	}
	// A developer deletes no developer; what it may revoke it still changes.
	f.expect(t, "ws-dev-ba", "DELETE", "/api/admin/users/"+dev, "", nil, http.StatusForbidden)
	f.expect(t, "ws-dev-ba", "PATCH", "/api/admin/users/"+bu, "", map[string]string{"display_name": "Renamed by a developer"}, http.StatusOK)
	// A tenant admin deletes its developer, not its peer; itself it renames.
	f.expect(t, ta, "DELETE", "/api/admin/users/"+peer, "", nil, http.StatusForbidden)
	f.expect(t, ta, "DELETE", "/api/admin/users/"+dev, "", nil, http.StatusOK)
	f.expect(t, ta, "PATCH", "/api/admin/users/"+taID, "", map[string]string{"display_name": "Tenant Admin"}, http.StatusOK)
	if n := f.one(t, `SELECT count(*)::text FROM identity.user WHERE id=ANY($1::uuid[])`, []string{peer, dev}); n != "1" {
		t.Errorf("%s of the peer and the developer left, want the peer", n)
	}
}

// A builder whose scope is every tenant keeps the accounts it invites, which
// have no tenant: it renames, re-invites and deletes them as before.
func TestPlatformWideBuilderKeepsItsInvitees(t *testing.T) {
	f := setupDevRouteFixture(t)
	const pw = "ab-pw-dev"
	f.gbUser(t, pw, "", [2]string{"developer", ""})
	var made map[string]any
	if err := json.Unmarshal(f.expect(t, pw, "POST", "/api/admin/users", "", map[string]string{"email": "pw-made@gb.test",
		"first_name": "Pw", "last_name": "Made", "role": "business_user", "workspace_id": f.ws1a}, http.StatusOK), &made); err != nil {
		t.Fatal(err)
	}
	id, _ := made["id"].(string)
	if got := f.listUsers(t, pw)[id]; got.HomeTenant != "none" ||
		got.Permissions != (listedPermissions{Rename: true, Delete: true, Disable: true, Reinvite: true}) {
		t.Errorf("its invitee in its list: %+v", got)
	}
	f.expect(t, pw, "PATCH", "/api/admin/users/"+id, "", map[string]string{"display_name": "Pw Renamed"}, http.StatusOK)
	f.expect(t, pw, "POST", "/api/admin/users/"+id+"/invite", "", nil, http.StatusServiceUnavailable) // no identity provider here
	f.expect(t, pw, "DELETE", "/api/admin/users/"+id, "", nil, http.StatusOK)
}

// The users list shows of each account only its part in the caller's
// tenants: inviting an existing account brought any account into the list,
// with every tenant it belonged to, by name, and every application and
// model it was granted (2026-09-30).
func TestUsersListShowsOnlyTheTenantsPart(t *testing.T) {
	f := setupDevRouteFixture(t)
	ctx := context.Background()
	const ta = "mm-tenant-admin"
	t2 := f.gbUser(t, "ab-t2-ta", f.cust2, [2]string{"tenant_admin", ""}, [2]string{"business_user", f.ws2})
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, t2, f.app3); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_model_access (user_id, model_id) VALUES ($1::uuid, $2::uuid)`, t2, f.modelD); err != nil {
		t.Fatal(err)
	}
	f.expect(t, ta, "POST", "/api/admin/users", "", map[string]string{"email": "AB-T2-TA@gb.test", "first_name": "T", "last_name": "Two",
		"role": "business_user", "workspace_id": f.ws1b}, http.StatusOK)
	f.expect(t, ta, "POST", "/api/admin/users/"+t2+"/access/apps/"+f.app1, "", nil, http.StatusOK)

	type listed struct {
		ID          string `json:"id"`
		Assignments []struct {
			Role         string `json:"role"`
			WorkspaceID  string `json:"workspace_id"`
			CustomerName string `json:"customer_name"`
		} `json:"assignments"`
		AppIDs   []string `json:"app_ids"`
		ModelIDs []string `json:"model_ids"`
	}
	find := func(sub string) listed {
		t.Helper()
		var users []listed
		if err := json.Unmarshal(f.expect(t, sub, "GET", "/api/admin/users", "", nil, http.StatusOK), &users); err != nil {
			t.Fatal(err)
		}
		for _, u := range users {
			if u.ID == t2 {
				return u
			}
		}
		t.Fatalf("%s's list does not show the account", sub)
		return listed{}
	}
	cust2Name := f.one(t, `SELECT name FROM core.customer WHERE id=$1::uuid`, f.cust2)
	u := find(ta)
	raw, _ := json.Marshal(u)
	if len(u.Assignments) != 1 || u.Assignments[0].Role != "business_user" || u.Assignments[0].WorkspaceID != f.ws1b ||
		strings.Contains(string(raw), cust2Name) || !slices.Equal(u.AppIDs, []string{f.app1}) || len(u.ModelIDs) != 0 {
		t.Errorf("tenant 1's admin sees of tenant 2's admin: %s", raw)
	}
	if u := find("mm-platform"); len(u.Assignments) != 3 || len(u.AppIDs) != 2 || len(u.ModelIDs) != 1 {
		t.Errorf("the platform admin sees of it: %+v, want all of it", u)
	}
}

// An account with no tenant holding tenant_admin without a workspace
// administers every tenant it holds any workspace role in: even
// business_user in a new tenant's workspace made it that tenant's
// administrator, which only a platform admin may make anyone.
func TestNoRoleMakesACustomerlessAdminAnotherTenantsAdmin(t *testing.T) {
	f := setupDevRouteFixture(t)
	const ta = "mm-tenant-admin"
	cta := f.gbUser(t, "ab-cta", "", [2]string{"tenant_admin", ""}, [2]string{"business_user", f.ws2})
	roles := func() string {
		return f.one(t, `SELECT string_agg(role::text||'@'||COALESCE(workspace_id::text,''), ',' ORDER BY role::text) FROM identity.role_assignment WHERE user_id=$1::uuid`, cta)
	}
	before := roles()
	var got map[string]any
	_ = json.Unmarshal(f.expect(t, ta, "POST", "/api/admin/users", "", map[string]string{"email": "ab-cta@gb.test", "first_name": "C", "last_name": "Ta",
		"role": "business_user", "workspace_id": f.ws1b}, http.StatusOK), &got)
	if got["id"] == cta {
		t.Errorf("the answer names the account: %v", got)
	}
	// dr-devws-ta2 administers tenant 2 (and, as a developer, tenant 1).
	f.expect(t, "dr-devws-ta2", "POST", "/api/admin/users/"+cta+"/roles", "", map[string]string{"role": "business_user", "workspace_id": f.ws1a}, http.StatusForbidden)
	if got := roles(); got != before {
		t.Errorf("its roles: %q, want %q", got, before)
	}
	if _, listed := f.listUsers(t, "ab-cta")[f.user1b]; listed {
		t.Errorf("it administers tenant 1")
	}
	// In a tenant it administers already, a role is the tenant's to give.
	f.expect(t, "dr-devws-ta2", "POST", "/api/admin/users/"+cta+"/roles", "", map[string]string{"role": "business_admin", "workspace_id": f.ws2}, http.StatusOK)
}

// An application deleted while a grant to it is sent, and the account's
// other grant removed: the delete locks the application first, so it reads
// — and locks — every account the grant reached, and the removal waits for
// it. Read unlocked, the grant committed while the delete waited for it was
// cascaded away from an account the delete had not locked, the removal saw
// it still there, and the account was left with no grant and its unscoped
// developer grant: a builder of every tenant (2026-09-30).
func TestApplicationDeleteRacingAGrantLeavesNoPlatformWideBuilder(t *testing.T) {
	f := setupDevRouteFixture(t)
	ctx := context.Background()
	exec := func(q pgxExecer, sql string, args ...any) {
		t.Helper()
		if _, err := q.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	appA := f.one(t, `INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Raced', 'planning') RETURNING id::text`, f.ws1a, f.cust1)
	x := f.gbUser(t, "race-x", "", [2]string{"developer", ""})
	y := f.gbUser(t, "race-y", "", [2]string{"developer", ""})
	exec(f.pool, `INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid), ($3::uuid, $4::uuid)`, x, f.app2, y, appA)

	begin := func() (pgx.Tx, int) {
		t.Helper()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) })
		var pid int
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		return tx, pid
	}
	// hold keeps y's developer grant, where the delete, once through its
	// cascade, stops to revoke it; grant is the grant of appA to x, not yet
	// committed.
	hold, holdPID := begin()
	exec(hold, `SELECT 1 FROM identity.role_assignment WHERE user_id=$1::uuid AND role='developer' FOR UPDATE`, y)
	grant, grantPID := begin()
	exec(grant, `INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, x, appA)

	send := func(method, path string) <-chan int {
		done := make(chan int, 1)
		go func() {
			code := 0
			if req, err := http.NewRequestWithContext(ctx, method, f.srv.URL+path, nil); err == nil {
				req.Header.Set("X-Dev-User", "mm-tenant-admin")
				if resp, err := http.DefaultClient.Do(req); err == nil {
					code = resp.StatusCode
					_ = resp.Body.Close()
				}
			}
			done <- code
		}()
		return done
	}
	// waitingOn is the backend waiting for a lock pid holds, or 0.
	waitingOn := func(pid int) int {
		var w int
		_ = f.pool.QueryRow(ctx, `SELECT COALESCE(min(pid), 0) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, pid).Scan(&w)
		return w
	}
	await := func(what string, pid int, done <-chan int) (waiter int, finished bool, code int) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			select {
			case code = <-done:
				return 0, true, code
			default:
			}
			if w := waitingOn(pid); w != 0 {
				return w, false, 0
			}
		}
		t.Fatalf("%s: neither finished nor waited", what)
		return 0, false, 0
	}

	del := send(http.MethodDelete, "/api/admin/applications/"+appA)
	if _, finished, code := await("the delete, for the grant", grantPID, del); finished {
		t.Fatalf("the application's delete finished (%d) while a grant to it was open", code)
	}
	if err := grant.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	delPID, finished, code := await("the delete, for y's developer grant", holdPID, del)
	if finished {
		t.Fatalf("the application's delete finished (%d) before revoking y's developer grant", code)
	}
	rm := send(http.MethodDelete, "/api/admin/users/"+x+"/access/apps/"+f.app2)
	_, rmFinished, rmCode := await("the removal of x's other grant", delPID, rm)
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-del; code != http.StatusOK {
		t.Errorf("the application's delete answered %d", code)
	}
	if !rmFinished {
		rmCode = <-rm
	}
	if rmCode != http.StatusOK {
		t.Errorf("the removal of x's other grant answered %d", rmCode)
	}
	if n := f.one(t, `SELECT count(*)::text FROM unnest($1::uuid[]) AS acc(id) WHERE `+platformWideBuilderSQL("acc.id"), []string{x, y}); n != "0" {
		t.Errorf("%s of x and y left platform-wide", n)
	}
	if n := f.one(t, `SELECT count(*)::text FROM identity.role_assignment WHERE user_id=ANY($1::uuid[])`, []string{x, y}); n != "0" {
		t.Errorf("x and y kept %s developer grants", n)
	}
}

// pgxExecer is a pool or a transaction.
type pgxExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
