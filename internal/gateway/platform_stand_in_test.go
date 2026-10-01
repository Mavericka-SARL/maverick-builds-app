package gateway

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// A platform admin's per-tenant actions addressed a dedicated tenant's
// database, where the platform admin has no account: every one answered 401
// (2026-09-30). The actor now comes from the control plane, and the tenant's
// database holds a stand-in under the same id that what they write names — a
// stand-in holds nothing, is not listed, and is no account of the tenant's.
func TestPlatformAdminActsOnADedicatedTenant(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	acme, acmeWS, actx := f.tenant(t, "Acme")
	pa := call{persona: "held-pa", tenant: acme}

	// The platform admin adds Acme's first administrator.
	f.invite(t, pa, "ann@acme.test", "Ann", "tenant_admin", acmeWS, http.StatusOK)
	ann, ok := f.row(actx, "ann@acme.test")
	if !ok || ann.customer != acme {
		t.Fatalf("Ann in Acme: %+v (ok %v); want Acme's account", ann, ok)
	}
	if member := f.memberships(ann.sub); !slices.Equal(member, []string{acme}) {
		t.Errorf("the directory lists Ann in %v; want Acme", member)
	}
	var assignedBy string
	if err := f.reader.db.QueryRow(actx, `SELECT COALESCE(assigned_by::text, '') FROM identity.role_assignment
		WHERE user_id = $1::uuid AND role = 'tenant_admin'`, ann.id).Scan(&assignedBy); err != nil || assignedBy != f.paID {
		t.Errorf("Ann's tenant_admin was assigned by %q (%v); want the platform admin %s", assignedBy, err, f.paID)
	}
	var audited int
	if err := f.reader.db.QueryRow(actx, `SELECT count(*) FROM audit.audit_event
		WHERE event_type = 'user.created' AND actor_user_id = $1::uuid AND resource_id = $2`, f.paID, ann.id).Scan(&audited); err != nil || audited != 1 {
		t.Errorf("Acme's audit has %d user.created events by the platform admin for Ann (%v); want 1", audited, err)
	}

	// The stand-in: the platform admin's id, nothing held, not listed.
	var standIn bool
	var held int
	if err := f.reader.db.QueryRow(actx, `SELECT u.stand_in, (SELECT count(*) FROM identity.role_assignment WHERE user_id = u.id)
		FROM identity.user u WHERE u.id = $1::uuid`, f.paID).Scan(&standIn, &held); err != nil || !standIn || held != 0 {
		t.Errorf("the platform admin's row in Acme: stand-in %v, %d roles (%v); want a stand-in holding none", standIn, held, err)
	}
	var standInAddress string
	if err := f.reader.db.QueryRow(actx, `SELECT email FROM identity.user WHERE id = $1::uuid`, f.paID).Scan(&standInAddress); err != nil ||
		standInAddress != f.paID+"@stand-in.invalid" {
		t.Errorf("the stand-in's address is %q (%v); want the reserved one, not the platform admin's", standInAddress, err)
	}
	if _, listed := f.users(t, pa)[f.paID]; listed {
		t.Errorf("the platform admin's users list of Acme shows the stand-in")
	}
	annCall := call{persona: ann.sub}
	if _, listed := f.users(t, annCall)[f.paID]; listed {
		t.Errorf("Acme's admin sees the platform admin's stand-in")
	}
	for _, try := range []struct{ method, path string }{
		{http.MethodPost, "/api/admin/users/" + f.paID + "/roles"},
		{http.MethodPatch, "/api/admin/users/" + f.paID},
		{http.MethodDelete, "/api/admin/users/" + f.paID},
		{http.MethodDelete, "/api/admin/users/" + f.paID + "/tenant-access"},
	} {
		code, body := do(t, f.srv, annCall, try.method, try.path, map[string]string{"role": "business_user", "workspace_id": acmeWS, "display_name": "Mine"})
		if code != http.StatusNotFound {
			t.Errorf("Acme's admin %s %s: %d %s; want 404", try.method, try.path, code, body)
		}
	}
	if _, err := f.reader.db.Exec(actx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES ($1::uuid, 'business_user', $2::uuid)`, f.paID, acmeWS); err == nil || !strings.Contains(err.Error(), "stand-in") {
		t.Errorf("a role for the stand-in was stored (%v); want the trigger to refuse it", err)
	}
	f.invite(t, annCall, "pa@held.test", "pa", "business_user", acmeWS, http.StatusOK)
	if held := f.sent(&f.broker.realmRoles, "held-pa"); held != 0 {
		t.Errorf("Acme's admin inviting the platform admin's address reached their identity-provider account")
	}

	// The console's other per-tenant calls answer too: workspaces, settings,
	// the audit log, and a model's creation, revision and export.
	for _, path := range []string{"/api/admin/workspaces", "/api/notifications/settings", "/api/admin/audit"} {
		if code, body := do(t, f.srv, pa, http.MethodGet, path, nil); code != http.StatusOK {
			t.Errorf("the platform admin's GET %s on Acme: %d %s", path, code, body)
		}
	}
	id := func(code int, body []byte, what string) string {
		t.Helper()
		var out struct {
			ID string `json:"id"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &out) != nil || out.ID == "" {
			t.Fatalf("%s: %d %s", what, code, body)
		}
		return out.ID
	}
	code, body := do(t, f.srv, f.pa, http.MethodPost, "/api/admin/applications",
		map[string]string{"customer_id": acme, "name": "Acme Planning", "mode": "planning"})
	app := id(code, body, "create an application")
	code, body = do(t, f.srv, pa, http.MethodPost, "/api/admin/models",
		map[string]string{"application_id": app, "name": "Budget", "storage_type": "oltp"})
	model := id(code, body, "create a model")
	if code, body := do(t, f.srv, pa, http.MethodPost, "/api/admin/revisions", map[string]string{"model_id": model, "name": "Next", "description": ""}); code != http.StatusOK && code != http.StatusCreated {
		t.Errorf("the platform admin's revision in Acme: %d %s", code, body)
	}
	if code, body := do(t, f.srv, pa, http.MethodGet, "/api/admin/models/"+model+"/export", nil); code != http.StatusOK {
		t.Errorf("the platform admin's model export from Acme: %d %s", code, body)
	}

	// Nothing platform-level is held in a tenant's database: platform_admin
	// is refused, and a developer with no workspace on an account with no
	// tenant would be platform-wide there.
	if code, body := do(t, f.srv, pa, http.MethodPost, "/api/admin/users/"+ann.id+"/roles", map[string]string{"role": "platform_admin"}); code != http.StatusForbidden {
		t.Errorf("the platform admin granting platform_admin in Acme's database: %d %s; want 403", code, body)
	}
	f.invite(t, pa, "alt@acme.test", "Alt", "platform_admin", "", http.StatusForbidden)
	f.invite(t, pa, "dev@acme.test", "Dev", "developer", "", http.StatusForbidden)
	// A platform_admin row found in a tenant's database grants nothing.
	if _, err := f.reader.db.Exec(actx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, ann.id); err != nil {
		t.Fatal(err)
	}
	if code, body := do(t, f.srv, annCall, http.MethodPost, "/api/admin/tenants", map[string]string{"name": "Planted", "plan": "enterprise"}); code != http.StatusForbidden {
		t.Errorf("Acme's admin with platform_admin in Acme's database creating a tenant: %d %s; want 403", code, body)
	}
	// The stand-ins' domain is reserved.
	f.invite(t, pa, "someone@stand-in.invalid", "Some", "business_user", acmeWS, http.StatusBadRequest)
	if _, err := f.reader.db.Exec(actx, `INSERT INTO identity.user (keycloak_sub, email) VALUES ('kc-squat', $1)`, f.paID+"@stand-in.invalid"); err == nil {
		t.Errorf("an account that is no stand-in took a stand-in's address")
	}

	// Demoted in the control plane: nothing in Acme's database keeps them in.
	if _, err := f.control.Exec(f.ctx, `DELETE FROM identity.role_assignment WHERE user_id = $1::uuid`, f.paID); err != nil {
		t.Fatal(err)
	}
	if code, body := do(t, f.srv, pa, http.MethodGet, "/api/admin/users", nil); code != http.StatusUnauthorized {
		t.Errorf("a demoted platform admin addressing Acme: %d %s; want 401", code, body)
	}
}

// Invitations and SCIM creations before 2026-09-30 adopted people another
// database held. ReconcileAdoptedAccounts undoes it at start-up: a platform
// admin's or platform-wide builder's row and directory entry go; anyone
// else's row becomes a member's, so they keep the tenant they have worked
// in. A second run changes nothing.
func TestReconcileAdoptedAccounts(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	acme, acmeWS, actx := f.tenant(t, "Acme")
	globex, globexWS, gctx := f.tenant(t, "Globex")

	// Left by an adoption: the platform admin as an account of Acme's. The
	// directory entry used to route the platform admin into Acme, off the
	// platform console; routing now goes by their platform reach in the
	// control plane, so the console answers even before the clean-up.
	f.seed(t, actx, "held-pa", "pa@held.test", "Pat", "business_user", acmeWS)
	if code, body := do(t, f.srv, call{persona: "held-pa"}, http.MethodGet, "/api/admin/tenants", nil); code != http.StatusOK {
		t.Fatalf("the adopted platform admin lost the platform console before the clean-up: %d %s", code, body)
	}
	// A platform-wide builder and a person of a shared-database tenant, both
	// in the control plane, adopted by Acme the same way.
	controlRow := func(sub, email, role string) {
		t.Helper()
		var id string
		if err := f.control.QueryRow(f.ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name)
			VALUES ($1, $2, $1) RETURNING id::text`, sub, email).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if role != "" {
			if _, err := f.control.Exec(f.ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, $2::identity.user_role)`, id, role); err != nil {
				t.Fatal(err)
			}
		}
	}
	controlRow("cp-bea", "bea@builder.test", "developer")
	f.seed(t, actx, "cp-bea", "bea@builder.test", "Bea", "business_user", acmeWS)
	controlRow("cp-carl", "carl@shared.test", "")
	carlAcme := f.seed(t, actx, "cp-carl", "carl@shared.test", "Carl", "business_user", acmeWS)
	// Globex's developer, adopted by Acme with developer and no workspace.
	f.seed(t, gctx, "kc-dev", "dev@globex.test", "Dev", "developer", globexWS)
	devAcme := f.seed(t, actx, "kc-dev", "dev@globex.test", "Dev", "developer", "")
	// The reverse: Acme's own Dana, whom a control-plane creation adopted
	// later. Her Acme row is hers.
	danaAcme := f.seed(t, actx, "kc-dana", "dana@acme.test", "Dana", "tenant_admin", acmeWS)
	controlRow("kc-dana", "dana@acme.test", "platform_admin")
	danaBefore, _ := f.row(actx, "dana@acme.test")
	// The control plane's own adoption: Acme's Ed, taken later by a
	// shared-database tenant's account holding developer without a
	// workspace. That row becomes a member's, its developer moved into the
	// shared tenant's workspace.
	sharedID, sharedWS, _ := f.sharedTenant(t, "SharedCo")
	f.seed(t, actx, "kc-ed", "ed@acme.test", "Ed", "business_user", acmeWS)
	var edControl string
	if err := f.control.QueryRow(f.ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('kc-ed', 'ed@acme.test', 'Ed', $1::uuid) RETURNING id::text`, sharedID).Scan(&edControl); err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.Exec(f.ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'developer')`, edControl); err != nil {
		t.Fatal(err)
	}
	// Gil is at home in Globex; Acme adopted Gil later, SCIM-managed.
	gilGlobex := f.seed(t, gctx, "kc-gil", "gil@globex.test", "Gil", "business_user", globexWS)
	gilAcme := f.seed(t, actx, "kc-gil", "gil@globex.test", "Gil", "business_user", acmeWS)
	if _, err := f.reader.db.Exec(actx, `UPDATE identity.user SET scim_managed = TRUE WHERE id = $1::uuid`, gilAcme); err != nil {
		t.Fatal(err)
	}
	// Ann is Acme's own, in Acme only: untouched, but for a platform_admin
	// grant Acme's database holds, which grants nothing and goes.
	annID := f.seed(t, actx, "kc-ann", "ann@acme.test", "Ann", "tenant_admin", acmeWS)
	if _, err := f.reader.db.Exec(actx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, annID); err != nil {
		t.Fatal(err)
	}
	annBefore, _ := f.row(actx, "ann@acme.test")

	undone, err := ReconcileAdoptedAccounts(f.ctx, f.router, logger.New("test"))
	if err != nil || undone.Removed != 2 || undone.MadeMembers != 4 || undone.LeftAlone != 2 || undone.PlatformRolesRemoved != 1 {
		t.Fatalf("reconcile: %+v, %v; want two removed, four made members, two left alone, one platform_admin grant removed", undone, err)
	}
	var edCustomer string
	var edUnscoped, edScoped int
	if err := f.control.QueryRow(f.ctx, `SELECT COALESCE(customer_id::text, ''),
		(SELECT count(*) FROM identity.role_assignment WHERE user_id = $1::uuid AND workspace_id IS NULL),
		(SELECT count(*) FROM identity.role_assignment WHERE user_id = $1::uuid AND role = 'developer' AND workspace_id = $2::uuid)
		FROM identity.user WHERE id = $1::uuid`, edControl, sharedWS).Scan(&edCustomer, &edUnscoped, &edScoped); err != nil ||
		edCustomer != "" || edUnscoped != 0 || edScoped != 1 {
		t.Errorf("Ed's control-plane row: tenant %q, %d grants without a workspace, %d developer in SharedCo's workspace (%v); "+
			"want a member, developer of the workspace only", edCustomer, edUnscoped, edScoped, err)
	}
	var annPlatform int
	if err := f.reader.db.QueryRow(actx, `SELECT count(*) FROM identity.role_assignment WHERE user_id = $1::uuid AND role = 'platform_admin'`, annID).Scan(&annPlatform); err != nil || annPlatform != 0 {
		t.Errorf("Acme's database still holds %d platform_admin grants for Ann (%v)", annPlatform, err)
	}
	if dana, _ := f.row(actx, "dana@acme.test"); dana != danaBefore || dana.id != danaAcme {
		t.Errorf("Acme's own Dana, adopted by the control plane later, changed: %+v -> %+v", danaBefore, dana)
	}
	if member := f.memberships("kc-dana"); !slices.Equal(member, []string{acme}) {
		t.Errorf("the directory lists Dana in %v; want Acme still", member)
	}
	// The adopted developer is a member holding developer in Acme's
	// workspace, not a platform-wide builder of Acme's database.
	var platformWide bool
	var unscoped, scoped int
	if err := f.reader.db.QueryRow(actx, `SELECT `+platformWideBuilderSQL("$1::uuid")+`,
		(SELECT count(*) FROM identity.role_assignment WHERE user_id = $1::uuid AND workspace_id IS NULL),
		(SELECT count(*) FROM identity.role_assignment WHERE user_id = $1::uuid AND role = 'developer' AND workspace_id = $2::uuid)`,
		devAcme, acmeWS).Scan(&platformWide, &unscoped, &scoped); err != nil || platformWide || unscoped != 0 || scoped != 1 {
		t.Errorf("the adopted developer in Acme: platform-wide %v, %d grants without a workspace, %d developer in Acme's workspace (%v); "+
			"want a developer of the workspace only", platformWide, unscoped, scoped, err)
	}
	for _, p := range []struct{ sub, email string }{{"held-pa", "pa@held.test"}, {"cp-bea", "bea@builder.test"}} {
		if _, ok := f.row(actx, p.email); ok {
			t.Errorf("Acme still holds %s's adopted row", p.sub)
		}
		if member := f.memberships(p.sub); len(member) != 0 {
			t.Errorf("the directory still routes %s to %v", p.sub, member)
		}
	}
	// Carl keeps Acme, as a member Acme does not own.
	if carl, ok := f.row(actx, "carl@shared.test"); !ok || carl.id != carlAcme || carl.customer != "" {
		t.Errorf("Carl in Acme: %+v (ok %v); want the same row, a member's", carl, ok)
	}
	if member := f.memberships("cp-carl"); !slices.Equal(member, []string{acme}) {
		t.Errorf("the directory lists Carl in %v; want Acme still", member)
	}
	if code, body := do(t, f.srv, call{persona: "held-pa"}, http.MethodGet, "/api/admin/tenants", nil); code != http.StatusOK {
		t.Errorf("the platform admin's console after the reconciliation: %d %s", code, body)
	}
	var customer string
	var scim bool
	var roles int
	if err := f.reader.db.QueryRow(actx, `SELECT COALESCE(customer_id::text, ''), scim_managed,
		(SELECT count(*) FROM identity.role_assignment WHERE user_id = u.id) FROM identity.user u WHERE id = $1::uuid`, gilAcme).Scan(&customer, &scim, &roles); err != nil ||
		customer != "" || scim || roles != 1 {
		t.Errorf("Gil in Acme: tenant %q, SCIM-managed %v, %d roles (%v); want a member with the role kept", customer, scim, roles, err)
	}
	if home, _ := f.row(gctx, "gil@globex.test"); home.id != gilGlobex || home.customer != globex {
		t.Errorf("Gil's Globex row changed: %+v", home)
	}
	both := []string{acme, globex}
	slices.Sort(both)
	if member := f.memberships("kc-gil"); !slices.Equal(member, both) {
		t.Errorf("the directory lists Gil in %v; want both", member)
	}
	if after, _ := f.row(actx, "ann@acme.test"); after != annBefore {
		t.Errorf("Acme's own Ann changed: %+v -> %+v", annBefore, after)
	}
	listed := f.users(t, call{persona: "kc-ann"})
	for who, id := range map[string]string{"Carl": carlAcme, "Gil": gilAcme} {
		if u := listed[id]; u.HomeTenant != homeOther || u.Permissions.Delete || u.Permissions.Rename {
			t.Errorf("Acme's admin sees %s as %q with %+v; want another organisation's member", who, u.HomeTenant, u.Permissions)
		}
	}
	var events int
	if err := f.reader.db.QueryRow(actx, `SELECT count(*) FROM audit.audit_event
		WHERE metadata->>'cause' IN ('adopted_across_databases', 'platform_admin_outside_control_plane')
		  AND metadata->>'visibility' = 'platform'`).Scan(&events); err != nil || events != 6 {
		t.Errorf("Acme's audit has %d platform-only reconciliation events (%v); want 6", events, err)
	}

	again, err := ReconcileAdoptedAccounts(f.ctx, f.router, logger.New("test"))
	if err != nil || again != (AdoptionsUndone{LeftAlone: 2}) {
		t.Errorf("a second run: %+v, %v; want nothing but Dana's and Ed's entries left alone", again, err)
	}

	// Removed from Acme, Carl is the control plane's only; invited again, a
	// member of Acme again: the control plane and Acme are both his homes.
	ann := call{persona: "kc-ann"}
	if code, body := do(t, f.srv, ann, http.MethodDelete, "/api/admin/users/"+carlAcme+"/tenant-access", nil); code != http.StatusOK {
		t.Fatalf("Acme's admin removing Carl: %d %s", code, body)
	}
	if member := f.memberships("cp-carl"); len(member) != 0 {
		t.Errorf("after the removal the directory lists Carl in %v", member)
	}
	f.invite(t, ann, "carl@shared.test", "Carl", "business_user", acmeWS, http.StatusOK)
	if member := f.memberships("cp-carl"); !slices.Equal(member, []string{acme}) {
		t.Errorf("inviting Carl again lists him in %v; want Acme", member)
	}
	var carlRoles int
	if err := f.reader.db.QueryRow(actx, `SELECT count(*) FROM identity.role_assignment WHERE user_id = $1::uuid`, carlAcme).Scan(&carlRoles); err != nil || carlRoles != 1 {
		t.Errorf("Carl holds %d roles in Acme after being invited again (%v); want one", carlRoles, err)
	}
}

// A tenant's account under the platform admin's own address kept the
// stand-in, which carried that address, from being written: the platform
// admin was refused by that tenant (2026-09-30). A stand-in carries a
// reserved address now.
func TestAStandInDoesNotNeedThePlatformAdminsAddress(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	acme, _, actx := f.tenant(t, "Acme")
	if _, err := f.reader.db.Exec(actx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('kc-lookalike', 'pa@held.test', 'Look Alike', $1::uuid)`, acme); err != nil {
		t.Fatal(err)
	}
	if code, body := do(t, f.srv, call{persona: "held-pa", tenant: acme}, http.MethodGet, "/api/admin/users", nil); code != http.StatusOK {
		t.Errorf("the platform admin addressing Acme, which has an account under their address: %d %s", code, body)
	}
}

// With an identity provider, it alone says whose an address is. A directory
// entry keeps the address a person had when they joined; once they changed it
// and someone new got the old one, an invitation of the new person was
// refused as someone else's (2026-09-30).
func TestAStaleDirectoryAddressIsNotHeld(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	acme, acmeWS, actx := f.tenant(t, "Acme")
	_, globexWS, gctx := f.tenant(t, "Globex")
	f.seed(t, actx, "kc-ann", "ann@acme.test", "Ann", "tenant_admin", acmeWS)
	f.seed(t, gctx, "kc-gil", "gil@globex.test", "Gil", "business_user", globexWS)
	f.broker.mu.Lock()
	f.broker.users["kc-gil"].Email = "gil.new@globex.test"
	f.broker.mu.Unlock()

	f.invite(t, call{persona: "kc-ann"}, "gil@globex.test", "Newcomer", "business_user", acmeWS, http.StatusOK)
	got, ok := f.row(actx, "gil@globex.test")
	if !ok || got.sub == "kc-gil" || got.customer != acme {
		t.Errorf("the newcomer under Gil's old address in Acme: %+v (ok %v); want a new account of Acme's", got, ok)
	}
}
