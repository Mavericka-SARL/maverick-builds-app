package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// AdoptionsUndone is what ReconcileAdoptedAccounts changed.
type AdoptionsUndone struct {
	// Removed are rows of platform admins and platform-wide builders,
	// deleted from a dedicated tenant's database with their directory entry.
	Removed int
	// MadeMembers are rows of anyone else another database holds, which the
	// tenant owned and now does not.
	MadeMembers int
	// LeftAlone are directory entries older than the control-plane account
	// under the same subject: there the control plane adopted the tenant's
	// person, not the reverse, and the tenant's row is theirs. The
	// control-plane account, when a shared-database tenant owned it, is made
	// a member's (counted in MadeMembers); one with platform reach is the
	// platform's grant, and routes them as platform-level (tenantRouting).
	LeftAlone int
	// PlatformRolesRemoved are platform_admin grants found in tenant
	// databases, where that role is never held (dedicatedGrantErr).
	PlatformRolesRemoved int
}

// ReconcileAdoptedAccounts undoes, with a database per tenant, what
// invitations and SCIM creations did before 2026-09-30 to people another
// database held: they found no account in the tenant's database and adopted
// the identity-provider account, giving the tenant a row it owned and a
// directory entry (identityHeldIn). It runs at every start, after the
// tenant databases are migrated; once it has run, nothing matches.
//
//   - A platform admin or a platform-wide builder the control plane holds
//     loses the tenant's row and its directory entry, which routed every
//     request of theirs to that tenant: off the platform console, and off
//     every tenant but that one. What they wrote there stays, authored by a
//     former user (migration 094); a platform admin acts on the tenant
//     through a stand-in now (platformActorOnControl).
//   - Anyone else the control plane holds keeps the tenant, as a member:
//     since the adoption the directory has routed them there, and that is
//     where they have worked. Removing them took it away (2026-09-30).
//   - Someone several dedicated tenants hold is at home in the one whose
//     directory entry is oldest: a takeover needs an account to take. In
//     every other, the row becomes a member's too.
//
// It also removes every platform_admin grant a tenant's database holds: that
// role is held only in the control plane.
//
// A member's row (addMemberFromElsewhere) has no tenant and is not
// SCIM-managed; its roles are kept. The tenant's administrators and its SCIM
// token can no longer rename, re-invite or delete it, or change the shared
// identity-provider account through it. That account is left as it is: its
// realm roles grant nothing (the gateway reads roles from its databases),
// and it is the person's. Each change is audited in the tenant's database for
// the platform only. A tenant whose database is not ready is skipped and
// tried at the next start.
func ReconcileAdoptedAccounts(ctx context.Context, router *tenantdb.Router, log zerolog.Logger) (AdoptionsUndone, error) {
	var done AdoptionsUndone
	if router == nil {
		return done, nil
	}
	control := router.Control()

	type entry struct{ sub, tenant string }
	collect := func(sql string) ([]entry, error) {
		rows, err := control.Query(ctx, sql)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, func(row pgx.CollectableRow) (entry, error) {
			var e entry
			return e, row.Scan(&e.sub, &e.tenant)
		})
	}
	// Platform admins' and platform-wide builders' entries, and everyone
	// else's the control plane holds.
	platformReach := `(EXISTS (SELECT 1 FROM identity.role_assignment pa WHERE pa.user_id = u.id AND pa.role = 'platform_admin')
		OR ` + platformWideBuilderSQL("u.id") + `)`
	// A tenant adopted a control-plane account only if that account is older
	// than the tenant's directory entry: a takeover needs an account to take.
	// The reverse — a creation in the control plane adopting a dedicated
	// tenant's person — leaves the tenant's row the person's own, and is
	// left alone (LeftAlone).
	platformInControl, err := collect(`
		SELECT d.keycloak_sub, d.customer_id::text FROM platform.user_directory d
		JOIN identity."user" u ON u.keycloak_sub = d.keycloak_sub
		WHERE u.created_at < d.created_at AND ` + platformReach + `
		ORDER BY d.keycloak_sub, d.created_at`)
	if err != nil {
		return done, fmt.Errorf("list platform accounts with tenant entries: %w", err)
	}
	othersInControl, err := collect(`
		SELECT d.keycloak_sub, d.customer_id::text FROM platform.user_directory d
		JOIN identity."user" u ON u.keycloak_sub = d.keycloak_sub
		WHERE u.created_at < d.created_at AND NOT ` + platformReach + `
		ORDER BY d.keycloak_sub, d.created_at`)
	if err != nil {
		return done, fmt.Errorf("list control-plane people with tenant entries: %w", err)
	}
	if err := control.QueryRow(ctx, `
		SELECT count(*) FROM platform.user_directory d
		JOIN identity."user" u ON u.keycloak_sub = d.keycloak_sub
		WHERE u.created_at >= d.created_at`).Scan(&done.LeftAlone); err != nil {
		return done, fmt.Errorf("count control-plane adoptions of tenants' people: %w", err)
	}
	if done.LeftAlone > 0 {
		log.Warn().Int("entries", done.LeftAlone).
			Msg("control-plane accounts created after a dedicated tenant's person under the same subject: " +
				"the tenant's rows are left alone; the control-plane ones a tenant owns become members'")
	}
	// The control plane's own adoptions: a row a shared-database tenant
	// owns, made after a dedicated tenant's person under the same subject,
	// becomes a member's there, as the reverse does in a tenant's database.
	controlAdopted, err := collect(`
		SELECT DISTINCT u.keycloak_sub, u.customer_id::text FROM identity."user" u
		JOIN platform.user_directory d ON d.keycloak_sub = u.keycloak_sub
		WHERE u.created_at >= d.created_at AND u.customer_id IS NOT NULL AND NOT ` + platformReach + `
		ORDER BY 1`)
	if err != nil {
		return done, fmt.Errorf("list control-plane adoptions of tenants' people: %w", err)
	}
	// Every entry but each subject's oldest, for subjects the control plane
	// does not hold.
	notHome, err := collect(`
		SELECT keycloak_sub, customer_id::text FROM (
		    SELECT d.keycloak_sub, d.customer_id,
		           row_number() OVER (PARTITION BY d.keycloak_sub ORDER BY d.created_at, d.customer_id) AS n
		    FROM platform.user_directory d
		    WHERE NOT EXISTS (SELECT 1 FROM identity."user" u WHERE u.keycloak_sub = d.keycloak_sub)
		) ranked
		WHERE n > 1
		ORDER BY keycloak_sub, customer_id`)
	if err != nil {
		return done, fmt.Errorf("list people several tenants hold: %w", err)
	}

	audit := func(tctx context.Context, pool *pgxpool.Pool, event auditlog.EventType, userID, email, change string) {
		auditlog.Log(tctx, pool, log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: event, ActorRole: "system",
			ResourceType: "identity_user", ResourceID: userID,
			Metadata: map[string]string{"email": email, "cause": "adopted_across_databases", "change": change,
				auditVisibilityKey: auditPlatformOnly},
		})
	}

	var failed []error
	for _, e := range platformInControl {
		pool, err := router.Pool(ctx, e.tenant)
		if err != nil {
			failed = append(failed, fmt.Errorf("tenant %s: %w", e.tenant, err))
			continue
		}
		tctx := tenantdb.WithScope(ctx, tenantdb.Scope{CustomerID: e.tenant, Pool: pool})
		rows, err := pool.Query(tctx, `
			DELETE FROM identity."user" WHERE keycloak_sub = $1 AND NOT stand_in
			RETURNING id::text, email`, e.sub)
		if err != nil {
			failed = append(failed, fmt.Errorf("tenant %s: remove a platform account's row: %w", e.tenant, err))
			continue
		}
		type gone struct{ id, email string }
		removed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (gone, error) {
			var g gone
			return g, row.Scan(&g.id, &g.email)
		})
		if err != nil {
			failed = append(failed, fmt.Errorf("tenant %s: remove a platform account's row: %w", e.tenant, err))
			continue
		}
		// Audited as soon as the row is gone: a failed directory write is
		// retried at the next start, when the row is already gone.
		for _, g := range removed {
			audit(tctx, pool, auditlog.EventUserDeleted, g.id, g.email, "removed: a platform account the control plane holds")
			done.Removed++
		}
		if err := router.Catalog().RemoveUser(ctx, e.sub, e.tenant); err != nil {
			failed = append(failed, fmt.Errorf("tenant %s: remove a platform account's directory entry: %w", e.tenant, err))
			continue
		}
		log.Warn().Str("tenant", e.tenant).Int("rows", len(removed)).
			Msg("undid an adoption of a platform account by a dedicated tenant: row and directory entry removed")
	}
	for _, e := range append(othersInControl, notHome...) {
		pool, err := router.Pool(ctx, e.tenant)
		if err != nil {
			failed = append(failed, fmt.Errorf("tenant %s: %w", e.tenant, err))
			continue
		}
		tctx := tenantdb.WithScope(ctx, tenantdb.Scope{CustomerID: e.tenant, Pool: pool})
		id, email, rescoped, err := makeMember(tctx, pool, e.sub, e.tenant)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			failed = append(failed, fmt.Errorf("tenant %s: make an adopted row a member's: %w", e.tenant, err))
			continue
		}
		change := "made a member: another database holds this person"
		if len(rescoped) > 0 {
			change += "; roles held without a workspace moved into each of the tenant's workspaces " +
				"(platform_admin dropped): " + strings.Join(rescoped, ", ")
		}
		audit(tctx, pool, auditlog.EventUserUpdated, id, email, change)
		done.MadeMembers++
		log.Warn().Str("tenant", e.tenant).Str("user_id", id).
			Msg("undid an adoption of someone another database holds: the row is now a member's, not the tenant's")
	}
	for _, e := range controlAdopted {
		cctx := controlCtx(ctx)
		id, email, rescoped, err := makeMember(cctx, control, e.sub, e.tenant)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			failed = append(failed, fmt.Errorf("control plane: make an adopted row a member's: %w", err))
			continue
		}
		change := "made a member: a dedicated tenant's database holds this person"
		if len(rescoped) > 0 {
			change += "; roles held without a workspace moved into each of the tenant's workspaces " +
				"(platform_admin dropped): " + strings.Join(rescoped, ", ")
		}
		audit(cctx, control, auditlog.EventUserUpdated, id, email, change)
		done.MadeMembers++
		log.Warn().Str("user_id", id).
			Msg("undid a control-plane adoption of a dedicated tenant's person: the row is now a member's")
	}
	// platform_admin is held only in the control plane: one a tenant's
	// database holds, written before 2026-09-30, grants nothing
	// (actorByKeycloakSub) and only showed a power the account lacks.
	if err := router.Each(ctx, func(t tenantdb.Tenant, pool *pgxpool.Pool) error {
		tctx := tenantdb.WithScope(ctx, tenantdb.Scope{CustomerID: t.CustomerID, Pool: pool})
		rows, err := pool.Query(tctx, `
			DELETE FROM identity.role_assignment ra WHERE ra.role = 'platform_admin'
			RETURNING ra.user_id::text, COALESCE((SELECT u.email FROM identity."user" u WHERE u.id = ra.user_id), '')`)
		if err != nil {
			failed = append(failed, fmt.Errorf("tenant %s: remove platform_admin grants: %w", t.CustomerID, err))
			return nil
		}
		type grant struct{ userID, email string }
		removed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (grant, error) {
			var g grant
			return g, row.Scan(&g.userID, &g.email)
		})
		if err != nil {
			failed = append(failed, fmt.Errorf("tenant %s: remove platform_admin grants: %w", t.CustomerID, err))
			return nil
		}
		for _, g := range removed {
			auditlog.Log(tctx, pool, log, auditlog.Fields{
				Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserRoleRevoked, ActorRole: "system",
				ResourceType: "identity_user", ResourceID: g.userID,
				Metadata: map[string]string{"email": g.email, "role": "platform_admin",
					"cause": "platform_admin_outside_control_plane", auditVisibilityKey: auditPlatformOnly},
			})
			done.PlatformRolesRemoved++
		}
		if len(removed) > 0 {
			log.Warn().Str("tenant", t.CustomerID).Int("grants", len(removed)).
				Msg("removed platform_admin grants from a tenant database: the role is held only in the control plane")
		}
		return nil
	}); err != nil {
		failed = append(failed, fmt.Errorf("walk tenant databases: %w", err))
	}
	return done, errors.Join(failed...)
}

// makeMember turns the row tenant owns under sub, in the database pool
// reaches, into a member's, in one transaction: no tenant, not SCIM-managed.
// A role it held without a workspace would be platform-wide on a row with no
// tenant — a developer so held builds as if for every tenant and acts on the
// tenant's other members (isGlobalBuilder) — so each is held in every one of
// that tenant's workspaces instead, which is what it reached before;
// platform_admin is dropped (a tenant's database never grants it,
// dedicatedGrantErr, and a tenant never granted it in the control plane). It
// returns pgx.ErrNoRows when the tenant owns no row under sub.
func makeMember(ctx context.Context, pool *pgxpool.Pool, sub, tenant string) (id, email string, rescoped []string, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", "", nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.QueryRow(ctx, `
		UPDATE identity."user" SET customer_id = NULL, scim_managed = FALSE, updated_at = now()
		WHERE keycloak_sub = $1 AND customer_id = $2::uuid AND NOT stand_in
		RETURNING id::text, email`, sub, tenant).Scan(&id, &email); err != nil {
		return "", "", nil, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id, assigned_by)
		SELECT ra.user_id, ra.role, w.id, ra.assigned_by
		FROM identity.role_assignment ra JOIN core.workspace w ON w.customer_id = $2::uuid
		WHERE ra.user_id = $1::uuid AND ra.workspace_id IS NULL AND ra.role <> 'platform_admin'
		ON CONFLICT DO NOTHING`, id, tenant); err != nil {
		return "", "", nil, err
	}
	rows, err := tx.Query(ctx, `
		DELETE FROM identity.role_assignment WHERE user_id = $1::uuid AND workspace_id IS NULL
		RETURNING role::text`, id)
	if err != nil {
		return "", "", nil, err
	}
	if rescoped, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return "", "", nil, err
	}
	return id, email, rescoped, tx.Commit(ctx)
}
