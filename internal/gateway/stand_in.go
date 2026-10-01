package gateway

// Platform admins and platform-wide builders acting on dedicated tenants.
//
// Their accounts and roles are in the control plane. The console addresses a
// dedicated tenant for them by its id (X-Tenant-Id) or one of its
// applications (X-App-Id), and tenantRouting sends the request to the
// tenant's database — where the actor was then looked up, and not found:
// every per-tenant action of the platform console answered 401 in dedicated
// mode (users, settings, model export, revisions), and a platform-wide
// builder reached no dedicated tenant at all (2026-09-30).
//
// Now such a request's actor comes from the control plane
// (platformActorOnControl), for a platform admin or a platform-wide builder
// only; tenantRouting routes them so even when a tenant also holds them. What
// they write in the tenant's database names them through foreign keys to
// identity.user — assigned_by, created_by, the audit log's actor — so that
// database holds a stand-in row under the same id (migration 103), with a
// subject and address of its own: no tenant, no role, no grant (a trigger
// refuses them), not listed, not counted, and answered 404 by the users
// routes. The tenant's database decides nothing about them: their roles are
// read from the control plane on every request, so a demotion there takes
// effect in every tenant at once, and isGlobalBuilder answers from what the
// control plane said (actor.onControl).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// actorOnControlKey marks a request that tenantRouting sent to a tenant's
// database for someone with no tenant of their own.
type actorOnControlKey struct{}

// controlCtx is ctx addressed to the control plane.
func controlCtx(ctx context.Context) context.Context {
	return tenantdb.WithScope(ctx, tenantdb.Scope{})
}

// platformActorOnControl resolves the actor of a request tenantRouting marked
// (actorOnControlKey) from the control plane. handled is false unless that
// actor is a platform admin or a platform-wide builder there: anyone else
// resolves as before, in the tenant's database, where a first sign-in
// through the tenant's identity provider creates their account
// (jitProvision). One whose stand-in cannot be written is refused rather
// than resolved in the tenant's database.
func (h *handler) platformActorOnControl(ctx context.Context, r *http.Request) (a *actor, handled bool, err error) {
	if on, _ := ctx.Value(actorOnControlKey{}).(bool); !on {
		return nil, false, nil
	}
	sub := h.subjectOf(r)
	if sub == "" {
		return nil, false, nil
	}
	cctx := controlCtx(ctx)
	a, err = h.actorByKeycloakSub(cctx, sub)
	if err != nil {
		return nil, false, nil
	}
	platformWide := h.isGlobalBuilder(cctx, a)
	if !a.hasRole("platform_admin") && !platformWide {
		return nil, false, nil
	}
	if !a.hasRole("platform_admin") {
		// Roles a platform-wide builder holds in the control plane's
		// workspaces are those tenants', never the dedicated tenant's this
		// request is routed to: here it is a builder and nothing else. Its
		// tenant_admin of a shared tenant passed the dedicated tenant's
		// administrator gates — SSO, SCIM tokens, branding (2026-10-01).
		a.Roles = []string{"developer"}
	}
	a.onControl, a.platformWide = true, platformWide
	if err := h.ensureStandIn(ctx, a); err != nil {
		h.log.Error().Err(err).Str("tenant", tenantdb.TenantFrom(ctx)).Str("user_id", a.UserID).
			Msg("platform account's stand-in could not be written in the tenant database — request refused")
		return nil, true, fmt.Errorf("unauthorized")
	}
	h.touchLastSeen(cctx, a.UserID)
	return a, true, nil
}

// standInRefresh is how often a platform admin's stand-in is rewritten per
// tenant: its name and e-mail follow the control plane's within this time.
const standInRefresh = 5 * time.Minute

// standInEmail is the address a stand-in carries: reserved, never delivered
// (RFC 2606's .invalid), and unique by the platform admin's id. Nothing mails
// a stand-in, and the audit log shows the actor's name. It carried the
// platform admin's own address, which is unique per database: an account of
// the tenant's under that address kept the stand-in from being written and
// the platform admin out of the tenant, and every tenant's database a
// platform admin acted in held their address (2026-09-30).
func standInEmail(userID string) string {
	return userID + standInDomain
}

// standInDomain is reserved for stand-ins (migration 103's
// stand_in_address_reserved): no account is invited, provisioned or renamed
// to an address in it.
const standInDomain = "@stand-in.invalid"

// reservedAddress reports whether email is in standInDomain.
func reservedAddress(email string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(email)), standInDomain)
}

// standInSubject is a stand-in's own subject. Nothing signs in as a
// stand-in (actorByKeycloakSub skips them), and the person's own subject may
// belong to an account of theirs in the same tenant — a platform admin who is
// also its member — which a stand-in under it would collide with.
func standInSubject(userID string) string {
	return "stand-in:" + userID
}

// ensureStandIn makes sure the database ctx is routed to holds a's stand-in:
// a row under a's id, with its own subject (standInSubject) and reserved
// address (standInEmail) and a's name, marked stand_in. It fails only when a
// row that is not a stand-in holds that id there.
func (h *handler) ensureStandIn(ctx context.Context, a *actor) error {
	key := tenantdb.TenantFrom(ctx) + "/" + a.UserID
	h.standInMu.Lock()
	if last, ok := h.standIns[key]; ok && time.Since(last) < standInRefresh {
		h.standInMu.Unlock()
		return nil
	}
	h.standInMu.Unlock()
	var standIn bool
	err := h.db.QueryRow(ctx, `
		INSERT INTO identity."user" (id, keycloak_sub, email, display_name, stand_in)
		VALUES ($1::uuid, $2, $3, $4, TRUE)
		ON CONFLICT (id) DO UPDATE
		SET keycloak_sub = EXCLUDED.keycloak_sub, email = EXCLUDED.email, display_name = EXCLUDED.display_name
		WHERE identity."user".stand_in
		RETURNING stand_in`, a.UserID, standInSubject(a.UserID), standInEmail(a.UserID), a.Name).Scan(&standIn)
	if err != nil {
		return err
	}
	if !standIn {
		return errors.New("a row that is not a stand-in holds the platform account's id")
	}
	h.standInMu.Lock()
	if h.standIns == nil || len(h.standIns) > 50000 {
		h.standIns = map[string]time.Time{}
	}
	h.standIns[key] = time.Now()
	h.standInMu.Unlock()
	return nil
}

// isStandIn reports whether userID is a platform admin's stand-in in the
// database ctx is routed to.
func (h *handler) isStandIn(ctx context.Context, userID string) bool {
	var standIn bool
	_ = h.db.QueryRow(ctx, `SELECT stand_in FROM identity."user" WHERE id::text = $1`, userID).Scan(&standIn)
	return standIn
}

// dedicatedGrantErr refuses, in a dedicated tenant's database, a role that
// would not be the tenant's to hold. platform_admin is held only in the
// control plane: one granted in a tenant's database made an administrator of
// the platform the control plane could not see or demote, since the actor's
// roles came from the database a request was routed to (2026-09-30). And
// developer or tenant_admin without a workspace, on an account with no tenant
// of its own (accountCustomer ""), is platform-wide there (isGlobalBuilder),
// with powers over the tenant's other members; given in a workspace it is the
// tenant's. Nil in the control plane and with a single database.
func dedicatedGrantErr(ctx context.Context, role, workspaceID, accountCustomer string) error {
	if tenantdb.TenantFrom(ctx) == "" {
		return nil
	}
	switch {
	case role == "platform_admin":
		return errors.New("forbidden: platform_admin is held only in the control plane, not in a tenant's database")
	case workspaceID == "" && accountCustomer == "" && (role == "developer" || role == "tenant_admin"):
		return fmt.Errorf("the %q role needs a workspace here: without one, an account with no tenant of its own "+
			"would build or administer as if for every tenant", role)
	}
	return nil
}

// personalCtx is where a's own settings live — preferences, AI provider
// settings and key: the control plane for an actor resolved from it
// (onControl), whose row in the routed tenant's database is a stand-in;
// otherwise ctx. They used to be read from the stand-in, and written there:
// a platform admin's settings changed with the tenant being worked on, and a
// personal key landed in a tenant's database (2026-09-30).
func personalCtx(ctx context.Context, a *actor) context.Context {
	if a != nil && a.onControl {
		return controlCtx(ctx)
	}
	return ctx
}

// platformReachOf reports which of subs the control plane holds with
// platform reach — a platform admin or a platform-wide builder — read fresh,
// since it decides who may change an account. In a tenant's database such a
// person's own row (their account as the tenant's person) says nothing of
// it, and the tenant's administrators and SCIM token treated it as any of
// theirs: a SCIM update changed a platform admin's sign-in address
// (2026-10-01). Empty with a single database, and in the control plane,
// where the account's own rows say it.
func (h *handler) platformReachOf(ctx context.Context, subs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if h.db.Router() == nil || tenantdb.TenantFrom(ctx) == "" || len(subs) == 0 {
		return out, nil
	}
	rows, err := h.db.Control().Query(ctx, `
		SELECT u.keycloak_sub FROM identity."user" u
		WHERE u.keycloak_sub = ANY($1) AND NOT u.stand_in
		  AND (EXISTS (SELECT 1 FROM identity.role_assignment pa WHERE pa.user_id = u.id AND pa.role = 'platform_admin')
		       OR `+platformWideBuilderSQL("u.id")+`)`, subs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sub string
		if err := rows.Scan(&sub); err != nil {
			return nil, err
		}
		out[sub] = true
	}
	return out, rows.Err()
}
