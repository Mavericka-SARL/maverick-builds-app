package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// With a database per tenant one identity can have a user row in the
// control plane and in tenant databases. Removing it from one removes the
// person from that database: the identity-provider account stays while
// another database holds the subject, and goes with the last row. Deleting
// it on the word of one tenant's administrator cut the person off every
// other tenant (2026-09-30).
func TestRemovingAnAccountKeepsAnIdentityAnotherDatabaseHolds(t *testing.T) {
	ctx := context.Background()
	control := testdb.New(t, migrationfs.FS, ".")
	router := tenantdb.New(control, tenantdb.Config{
		Migrations: migrationfs.FS, MigrationsDir: ".",
		AdminURL: testdb.AdminDSN(t), IdleTimeout: -1, Log: logger.New("test"),
	})
	t.Cleanup(router.Close)
	b, err := router.Provision(ctx, "Dedicated B", "enterprise")
	if err != nil {
		t.Fatal(err)
	}
	bPool, err := router.Pool(ctx, b.Tenant.CustomerID)
	if err != nil {
		t.Fatal(err)
	}
	bctx := tenantdb.WithScope(ctx, tenantdb.Scope{CustomerID: b.Tenant.CustomerID, Pool: bPool})
	broker := newFakeBroker(t)
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(control, router), devMode: true, kc: broker.client()}

	row := func(ctx context.Context, sub string) string {
		t.Helper()
		var id string
		if err := h.db.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1, $1 || '@elsewhere.test', $1) RETURNING id::text`, sub).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	gone := func(sub string) bool {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		return slices.Contains(broker.deleted, sub)
	}
	for _, sub := range []string{"kc-both", "kc-b-only", "kc-two-dbs"} {
		broker.users[sub] = &fakeKCUser{Email: sub + "@elsewhere.test", Enabled: true}
	}

	// In the control plane and in B's database: removed from B, kept.
	inControl, inB := row(ctx, "kc-both"), row(bctx, "kc-both")
	h.noteUser(bctx, "kc-both", "kc-both@elsewhere.test")
	if err := h.removeUserAccount(bctx, inB, "kc-both"); err != nil {
		t.Fatal(err)
	}
	if gone("kc-both") {
		t.Errorf("removing kc-both from tenant B deleted its identity-provider account; the control plane still holds it")
	}
	// Removed from the control plane too — its last row: deleted.
	if err := h.removeUserAccount(ctx, inControl, "kc-both"); err != nil {
		t.Fatal(err)
	}
	if !gone("kc-both") {
		t.Errorf("kc-both's last row is gone but its identity-provider account stays")
	}

	// In B's database and listed there only: deleted with that row.
	onlyB := row(bctx, "kc-b-only")
	h.noteUser(bctx, "kc-b-only", "kc-b-only@elsewhere.test")
	if err := h.removeUserAccount(bctx, onlyB, "kc-b-only"); err != nil {
		t.Fatal(err)
	}
	if !gone("kc-b-only") {
		t.Errorf("kc-b-only's only row is gone but its identity-provider account stays")
	}

	// In the control plane, and in B's database as the directory says:
	// removed from the control plane, kept.
	twoCtl := row(ctx, "kc-two-dbs")
	row(bctx, "kc-two-dbs")
	h.noteUser(bctx, "kc-two-dbs", "kc-two-dbs@elsewhere.test")
	if err := h.removeUserAccount(ctx, twoCtl, "kc-two-dbs"); err != nil {
		t.Fatal(err)
	}
	if gone("kc-two-dbs") {
		t.Errorf("removing kc-two-dbs from the control plane deleted its identity-provider account; tenant B still holds it")
	}
}

// With a database per tenant, an invitation into a dedicated tenant looked
// for the address in that database only. Someone another database held was
// adopted: a row the inviting tenant owned, a realm role and a set-password
// e-mail on the shared identity-provider account, and a directory entry that
// routed every request of a control-plane person's — a platform admin's too
// — to the inviting tenant (2026-09-30). Now another dedicated tenant's
// person is added as a member the inviting tenant does not own, labelled as
// someone from another tenant; removing them takes the directory entry and
// adding them again restores it. The control plane's is refused, answered as
// a new invitation is.
func TestAnInvitationDoesNotAdoptSomeoneAnotherDatabaseHolds(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	acme, acmeWS, actx := f.tenant(t, "Acme")
	globex, globexWS, gctx := f.tenant(t, "Globex")
	f.seed(t, actx, "kc-ann", "ann@acme.test", "Ann", "tenant_admin", acmeWS)
	f.seed(t, gctx, "kc-gil", "gil@globex.test", "Gil", "business_user", globexWS)
	f.seed(t, gctx, "kc-gia", "gia@globex.test", "Gia", "business_user", globexWS)
	gil, _ := f.row(gctx, "gil@globex.test")
	gia, _ := f.row(gctx, "gia@globex.test")
	ann := call{persona: "kc-ann"}

	// The platform admin's address, invited by Acme's admin: answered as a
	// new invitation, and nothing happens to the account.
	f.invite(t, ann, "pa@held.test", "pa", "business_user", acmeWS, http.StatusOK)
	if _, ok := f.row(actx, "pa@held.test"); ok {
		t.Errorf("Acme's database got a row for the platform admin")
	}
	if member := f.memberships("held-pa"); len(member) != 0 {
		t.Errorf("the directory routes the platform admin to %v", member)
	}
	if n := f.sent(&f.broker.invited, "held-pa") + f.sent(&f.broker.realmRoles, "held-pa"); n != 0 {
		t.Errorf("the platform admin's identity-provider account got %d set-password mails or realm roles", n)
	}
	if code, body := do(t, f.srv, f.pa, http.MethodGet, "/api/admin/tenants", nil); code != http.StatusOK ||
		!strings.Contains(string(body), acme) || !strings.Contains(string(body), globex) {
		t.Errorf("the platform admin lost the platform console: %d %s", code, body)
	}

	// Globex's person, invited by Acme's admin: a member of Acme under the
	// same subject, which Acme does not own, and nothing sent to them.
	f.invite(t, ann, "gil@globex.test", "gil", "business_user", acmeWS, http.StatusOK)
	inAcme, ok := f.row(actx, "gil@globex.test")
	if !ok || inAcme.sub != gil.sub || inAcme.customer != "" {
		t.Fatalf("Gil in Acme: %+v (ok %v); want subject %s and no tenant", inAcme, ok, gil.sub)
	}
	if inAcme.name != gil.name {
		t.Errorf("Gil's name in Acme is %q, the inviter's typing; want the identity provider's %q", inAcme.name, gil.name)
	}
	var granted bool
	if err := f.reader.db.QueryRow(actx, `SELECT EXISTS (SELECT 1 FROM identity.role_assignment
		WHERE user_id = $1::uuid AND role = 'business_user' AND workspace_id = $2::uuid)`, inAcme.id, acmeWS).Scan(&granted); err != nil || !granted {
		t.Errorf("Gil was not given business_user in Acme's workspace (%v)", err)
	}
	both := []string{acme, globex}
	slices.Sort(both)
	if member := f.memberships(gil.sub); !slices.Equal(member, both) {
		t.Errorf("the directory lists Gil in %v; want Globex and Acme", member)
	}
	if n := f.sent(&f.broker.invited, gil.sub) + f.sent(&f.broker.realmRoles, gil.sub); n != 0 {
		t.Errorf("Gil's identity-provider account got %d set-password mails or realm roles", n)
	}
	if again, _ := f.row(gctx, "gil@globex.test"); again != gil {
		t.Errorf("Gil's Globex row changed: %+v -> %+v", gil, again)
	}
	listed := f.users(t, ann)[inAcme.id]
	if listed.HomeTenant != homeOther {
		t.Errorf("Acme's admin sees Gil's home as %q; want %q", listed.HomeTenant, homeOther)
	}
	if p := listed.Permissions; p.Rename || p.Delete || p.Reinvite || !p.RemoveFromTenant {
		t.Errorf("Acme's admin may %+v on Gil; want only remove from tenant", p)
	}
	if code, body := do(t, f.srv, ann, http.MethodDelete, "/api/admin/users/"+inAcme.id, nil); code != http.StatusForbidden {
		t.Errorf("Acme's admin deleting Gil: %d %s; want 403", code, body)
	}

	// Removed from Acme: the directory stops routing Gil there; the row
	// stays for what Gil wrote. Invited again: routed there again.
	if code, body := do(t, f.srv, ann, http.MethodDelete, "/api/admin/users/"+inAcme.id+"/tenant-access", nil); code != http.StatusOK {
		t.Fatalf("Acme's admin removing Gil: %d %s", code, body)
	}
	if member := f.memberships(gil.sub); !slices.Equal(member, []string{globex}) {
		t.Errorf("after the removal the directory lists Gil in %v; want Globex only", member)
	}
	f.invite(t, ann, "gil@globex.test", "gil", "business_user", acmeWS, http.StatusOK)
	if member := f.memberships(gil.sub); !slices.Equal(member, both) {
		t.Errorf("after adding Gil again the directory lists %v; want Globex and Acme", member)
	}

	// The platform admin, addressing Acme, adds people other databases hold
	// — Globex's Gia, and the control plane's own platform admin — under
	// their own subjects, with no tenant, and nothing on their
	// identity-provider accounts.
	inAcmePA := call{persona: "held-pa", tenant: acme}
	f.invite(t, inAcmePA, "gia@globex.test", "gia", "business_user", acmeWS, http.StatusOK)
	if inAcme, ok := f.row(actx, "gia@globex.test"); !ok || inAcme.sub != gia.sub || inAcme.customer != "" {
		t.Errorf("Gia in Acme after the platform admin's creation: %+v (ok %v); want subject %s and no tenant", inAcme, ok, gia.sub)
	}
	f.invite(t, inAcmePA, "pa@held.test", "pa", "business_user", acmeWS, http.StatusOK)
	if inAcme, ok := f.row(actx, "pa@held.test"); !ok || inAcme.sub != "held-pa" || inAcme.customer != "" {
		t.Errorf("the platform admin in Acme after their own creation: %+v (ok %v); want their subject and no tenant", inAcme, ok)
	}
	for _, sub := range []string{gia.sub, "held-pa"} {
		if n := f.sent(&f.broker.invited, sub) + f.sent(&f.broker.realmRoles, sub); n != 0 {
			t.Errorf("%s's identity-provider account got %d set-password mails or realm roles", sub, n)
		}
	}
	// Still the platform admin, a member of Acme or not.
	if code, body := do(t, f.srv, f.pa, http.MethodGet, "/api/admin/tenants", nil); code != http.StatusOK || !strings.Contains(string(body), globex) {
		t.Errorf("the platform admin, also a member of Acme, lost the platform console: %d %s", code, body)
	}
}

// A member's row is a copy its tenant's administrators cannot rename; it
// follows the name and e-mail the person signs in with (followIdentity).
// A tenant's own account keeps what its administrators gave it.
func TestAMemberFollowsTheirSignIn(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	_, acmeWS, actx := f.tenant(t, "Acme")
	_, globexWS, gctx := f.tenant(t, "Globex")
	f.seed(t, actx, "kc-ann", "ann@acme.test", "Ann", "tenant_admin", acmeWS)
	f.seed(t, gctx, "kc-gil", "gil@globex.test", "Gil", "business_user", globexWS)
	f.invite(t, call{persona: "kc-ann"}, "gil@globex.test", "gil", "business_user", acmeWS, http.StatusOK)
	member, _ := f.row(actx, "gil@globex.test")
	own, _ := f.row(actx, "ann@acme.test")

	h := &handler{log: f.reader.log, db: f.reader.db}
	claims := func(sub, email, name string) *identity.Claims {
		c := &identity.Claims{Email: email, Name: name}
		c.Subject = sub
		return c
	}
	a := &actor{UserID: member.id, Email: member.email, Name: member.name}
	h.followIdentity(actx, a, claims("kc-gil", "gil.new@globex.test", "Gil Renamed"))
	if got, _ := f.row(actx, "gil.new@globex.test"); got.id != member.id || got.name != "Gil Renamed" {
		t.Errorf("Gil's member row after signing in renamed: %+v", got)
	}
	a = &actor{UserID: own.id, Email: own.email, Name: own.name, CustomerID: own.customer}
	h.followIdentity(actx, a, claims("kc-ann", "ann.new@acme.test", "Ann Renamed"))
	if got, _ := f.row(actx, "ann@acme.test"); got != own {
		t.Errorf("Acme's own account followed the sign-in: %+v -> %+v", own, got)
	}
}

// SCIM creation in a dedicated tenant looked for the address in that
// database only, and adopted the identity-provider account of someone
// another database held — which it then renamed, re-addressed, disabled or
// deleted for every database (2026-09-30). Now such a person is answered as
// an existing user, and deleting a user whose identity another database
// holds leaves the identity-provider account alone.
func TestScimDoesNotAdoptSomeoneAnotherDatabaseHolds(t *testing.T) {
	f := newDedicatedFixture(t, Deps{License: enterpriseManager(t), PublicURL: "https://console.test"})
	_, acmeWS, actx := f.tenant(t, "Acme")
	globex, globexWS, gctx := f.tenant(t, "Globex")
	f.seed(t, actx, "kc-ann", "ann@acme.test", "Ann", "tenant_admin", acmeWS)
	f.seed(t, gctx, "kc-gil", "gil@globex.test", "Gil", "business_user", globexWS)

	code, body := do(t, f.srv, call{persona: "kc-ann"}, http.MethodPost, "/api/admin/scim/tokens", map[string]any{"name": "Entra ID"})
	var issued struct {
		Token string `json:"token"`
	}
	if code != http.StatusCreated || json.Unmarshal(body, &issued) != nil || issued.Token == "" {
		t.Fatalf("issue a SCIM token: %d %s", code, body)
	}
	scimCall := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var buf []byte
		if body != nil {
			buf, _ = json.Marshal(body)
		}
		req, _ := http.NewRequestWithContext(f.ctx, method, f.srv.URL+"/api/scim/v2"+path, bytes.NewReader(buf))
		req.Header.Set("Authorization", "Bearer "+issued.Token)
		req.Header.Set("Content-Type", "application/scim+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	user := func(email string) map[string]any {
		return map[string]any{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:User"}, "userName": email,
			"name": map[string]string{"givenName": "Typed", "familyName": "Name"}, "active": true}
	}

	for _, p := range []struct{ email, sub string }{{"pa@held.test", "held-pa"}, {"gil@globex.test", "kc-gil"}} {
		if code, out := scimCall(http.MethodPost, "/Users", user(p.email)); code != http.StatusConflict {
			t.Errorf("SCIM creation of %s, held in another database: %d %v; want 409", p.email, code, out)
		}
		var n int
		if err := f.reader.db.QueryRow(actx, `SELECT count(*) FROM identity.user WHERE keycloak_sub = $1`, p.sub).Scan(&n); err != nil || n != 0 {
			t.Errorf("Acme's database has %d rows for %s (%v)", n, p.sub, err)
		}
		if f.sent(&f.broker.realmRoles, p.sub) != 0 {
			t.Errorf("%s's identity-provider account got a realm role", p.sub)
		}
	}
	if member := f.memberships("held-pa"); len(member) != 0 {
		t.Errorf("the directory routes the platform admin to %v", member)
	}

	// Gil, added to Acme as a member: Acme's directory may deactivate and
	// delete Gil's place in Acme, and nothing of Gil's account.
	f.invite(t, call{persona: "kc-ann"}, "gil@globex.test", "gil", "business_user", acmeWS, http.StatusOK)
	gilAcme, _ := f.row(actx, "gil@globex.test")
	patch := map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
		"Operations": []map[string]any{{"op": "replace", "value": map[string]any{"active": false}}}}
	if code, out := scimCall(http.MethodPatch, "/Users/"+gilAcme.id, patch); code != http.StatusOK || out["active"] != false {
		t.Errorf("SCIM deactivating the member Gil in Acme: %d %v", code, out)
	}
	var disabled bool
	if err := f.reader.db.QueryRow(actx, `SELECT disabled_at IS NOT NULL FROM identity.user WHERE id = $1::uuid`, gilAcme.id).Scan(&disabled); err != nil || !disabled {
		t.Errorf("Gil's Acme row is not disabled (%v)", err)
	}
	f.broker.mu.Lock()
	enabled := f.broker.users["kc-gil"].Enabled
	f.broker.mu.Unlock()
	if !enabled {
		t.Errorf("Acme's SCIM disabled Gil's identity-provider account")
	}
	if code, out := scimCall(http.MethodDelete, "/Users/"+gilAcme.id, nil); code != http.StatusNoContent {
		t.Errorf("SCIM deleting the member Gil from Acme: %d %v", code, out)
	}
	if _, ok := f.row(actx, "gil@globex.test"); ok {
		t.Errorf("Gil's Acme row is still there")
	}
	if member := f.memberships("kc-gil"); !slices.Equal(member, []string{globex}) {
		t.Errorf("after the SCIM deletion the directory lists Gil in %v; want Globex only", member)
	}
	if f.sent(&f.broker.deleted, "kc-gil") != 0 {
		t.Errorf("Acme's SCIM deleted Gil's identity-provider account")
	}

	// Dana is Acme's, and a platform admin the control plane made later:
	// her Acme row is no longer Acme's directory's to change.
	danaID := f.seed(t, actx, "kc-dana", "dana@acme.test", "Dana", "business_user", acmeWS)
	if _, err := f.reader.db.Exec(actx, `UPDATE identity.user SET scim_managed = TRUE WHERE id = $1::uuid`, danaID); err != nil {
		t.Fatal(err)
	}
	var danaControl string
	if err := f.control.QueryRow(f.ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name)
		VALUES ('kc-dana', 'dana@acme.test', 'Dana') RETURNING id::text`).Scan(&danaControl); err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.Exec(f.ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, danaControl); err != nil {
		t.Fatal(err)
	}
	rename := map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
		"Operations": []map[string]any{{"op": "replace", "value": map[string]any{"userName": "attacker@evil.test", "active": false}}}}
	if code, out := scimCall(http.MethodPatch, "/Users/"+danaID, rename); code != http.StatusForbidden {
		t.Errorf("Acme's SCIM changing the platform admin Dana: %d %v; want 403", code, out)
	}
	if code, out := scimCall(http.MethodDelete, "/Users/"+danaID, nil); code != http.StatusForbidden {
		t.Errorf("Acme's SCIM deleting the platform admin Dana: %d %v; want 403", code, out)
	}
	f.broker.mu.Lock()
	danaKC := *f.broker.users["kc-dana"]
	f.broker.mu.Unlock()
	if danaKC.Email != "dana@acme.test" || !danaKC.Enabled {
		t.Errorf("Dana's identity-provider account changed: %+v", danaKC)
	}
	if code, body := do(t, f.srv, call{persona: "kc-ann"}, http.MethodPatch, "/api/admin/users/"+danaID, map[string]string{"display_name": "Renamed"}); code != http.StatusForbidden {
		t.Errorf("Acme's admin renaming the platform admin Dana: %d %s; want 403", code, body)
	}

	// Acme's own user, later added to Globex too: deleting it in Acme keeps
	// the identity-provider account Globex's row signs in with.
	code, made := scimCall(http.MethodPost, "/Users", user("new@acme.test"))
	if code != http.StatusCreated {
		t.Fatalf("SCIM creation of a new address: %d %v", code, made)
	}
	created, _ := f.row(actx, "new@acme.test")
	if _, err := f.reader.db.Exec(gctx, `INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1, 'new@acme.test', 'New')`, created.sub); err != nil {
		t.Fatal(err)
	}
	if err := f.router.Catalog().AddUser(f.ctx, created.sub, globex, "new@acme.test"); err != nil {
		t.Fatal(err)
	}
	if code, out := scimCall(http.MethodDelete, "/Users/"+fmt.Sprint(made["id"]), nil); code != http.StatusNoContent {
		t.Fatalf("SCIM delete: %d %v", code, out)
	}
	if f.sent(&f.broker.deleted, created.sub) != 0 {
		t.Errorf("SCIM deleting Acme's user deleted the identity-provider account Globex's row signs in with")
	}
}
