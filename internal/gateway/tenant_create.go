package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// createTenantRequest is POST /api/admin/tenants' body.
type createTenantRequest struct {
	Name string `json:"name"`
	// Plan is the edition's key; absent means commercial, which carries no
	// limits.
	Plan string `json:"plan"`
	// GettingStarted installs the "Getting started" application, as
	// sign-up does (installGettingStarted); absent means yes.
	GettingStarted *bool `json:"getting_started"`
	// Join makes the tenant the creator's own, holding every tenant role in
	// it (grantEveryTenantRole) beside platform_admin — what a self-hosted
	// deployment's first administrator needs to work in it as a Playground
	// sign-up does. Only for someone with no tenant of their own, in a
	// single-database deployment (joinNewTenantRefusal).
	Join bool `json:"join"`
}

// errJoinTaken: the creator gained a tenant of their own between the check
// and the write.
var errJoinTaken = errors.New("your account already belongs to a tenant")

// joinNewTenantRefusal says why a tenant a can create cannot be their own,
// or "" when it can.
func (h *handler) joinNewTenantRefusal(a *actor) string {
	switch {
	case !a.hasRole("platform_admin"):
		return "only a platform administrator creates tenants"
	case h.db.Dedicated():
		// A platform administrator's roles are read from the control plane
		// in every dedicated tenant (platformActorOnControl): roles held in
		// the tenant's own database would never apply.
		return "with a database per tenant, a platform administrator works in every tenant as platform administrator; invite a separate account to hold the tenant's roles"
	case a.CustomerID != "":
		return "your account already belongs to a tenant"
	}
	return ""
}

// createTenant creates a tenant (its own database in dedicated mode) with a
// default workspace and, unless asked not to, the "Getting started"
// application; with Join, the creator holds every tenant role in it. All of
// it or none. status is the HTTP status of a failure.
func (h *handler) createTenant(ctx context.Context, act *actor, body createTenantRequest) (out map[string]string, status int, err error) {
	if body.Plan == "" {
		body.Plan = "commercial"
	}
	withStarters := body.GettingStarted == nil || *body.GettingStarted
	if body.Join {
		if reason := h.joinNewTenantRefusal(act); reason != "" {
			return nil, http.StatusBadRequest, fmt.Errorf("cannot make the new tenant your own: %s", reason)
		}
	}

	var id, wsID string
	var gs gettingStarted
	tctx := ctx
	if h.db.Dedicated() {
		// Dedicated mode: the tenant gets its own database, carrying the
		// customer row and default workspace.
		if id, wsID, err = h.provisionTenant(ctx, body.Name, body.Plan); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		if withStarters {
			tctx = h.tenantCtx(ctx, id)
			// The starters name their importer through foreign keys to
			// identity.user: the creator's stand-in there (ensureStandIn).
			err = h.ensureStandIn(tctx, act)
			if err == nil {
				err = pgx.BeginFunc(tctx, h.db.For(tctx), func(tx pgx.Tx) error {
					var ierr error
					gs, ierr = installGettingStarted(tctx, tx, id, wsID, act.UserID)
					return ierr
				})
			}
			if err != nil {
				if derr := h.db.Router().Deprovision(context.WithoutCancel(ctx), id); derr != nil {
					h.log.Error().Err(derr).Str("tenant", id).Msg("tenant creation rollback: tenant database not dropped")
				}
				return nil, http.StatusInternalServerError, fmt.Errorf("getting started application: %w", err)
			}
		}
	} else {
		err = pgx.BeginFunc(ctx, h.db.For(ctx), func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx,
				`INSERT INTO core.customer (name, plan) VALUES ($1, $2) RETURNING id::text`,
				body.Name, body.Plan).Scan(&id); err != nil {
				return err
			}
			// A tenant needs at least one workspace before workspace-scoped
			// roles can be assigned to its users — create a default one right
			// away so a fresh tenant is immediately usable.
			if err := tx.QueryRow(ctx,
				`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`,
				id).Scan(&wsID); err != nil {
				return err
			}
			if body.Join {
				tag, err := tx.Exec(ctx,
					`UPDATE identity.user SET customer_id = $2::uuid, updated_at = now() WHERE id = $1::uuid AND customer_id IS NULL`,
					act.UserID, id)
				if err != nil {
					return err
				}
				if tag.RowsAffected() != 1 {
					return errJoinTaken
				}
				if err := grantEveryTenantRole(ctx, tx, act.UserID, wsID, act.UserID); err != nil {
					return err
				}
			}
			if withStarters {
				var err error
				gs, err = installGettingStarted(ctx, tx, id, wsID, act.UserID)
				return err
			}
			return nil
		})
		if errors.Is(err, errJoinTaken) {
			return nil, http.StatusConflict, fmt.Errorf("cannot make the new tenant your own: %w", err)
		}
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
	}

	out = map[string]string{"id": id, "workspace_id": wsID}
	meta := map[string]string{"name": body.Name, "plan": body.Plan, "getting_started": strconv.FormatBool(withStarters), "joined": strconv.FormatBool(body.Join)}
	if withStarters {
		h.noteApplication(tctx, gs.appID)
		out["application_id"], out["model_id"] = gs.appID, gs.landingModelID()
		meta["application_id"], meta["model_id"], meta["guide_model_ids"] = gs.appID, gs.landingModelID(), strings.Join(gs.guideModelIDs(), ",")
	}
	// In the control plane, as before the application existed: its id is
	// metadata only, since that database does not hold it in dedicated mode.
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventTenantCreated,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "tenant", ResourceID: id,
		Metadata: meta,
	})
	if body.Join {
		for _, role := range everyTenantRole {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserRoleGranted,
				ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
				ResourceType: "user", ResourceID: act.UserID,
				Metadata: map[string]string{"role": role, "tenant": id, "reason": "took the new tenant as their own"},
			})
			// The identity provider mirrors the roles, as it does for a
			// sign-up; the gateway reads them from the database alone, so a
			// failure here changes nothing anyone can do.
			if h.kc != nil {
				if sub := h.subjectOfUser(ctx, act.UserID); sub != "" {
					if kerr := h.kc.AssignRealmRole(ctx, sub, role); kerr != nil {
						h.log.Warn().Err(kerr).Str("role", role).Msg("new tenant's roles not mirrored in the identity provider")
					}
				}
			}
		}
	}
	if withStarters {
		h.recalcGettingStarted(tctx, gs)
	}
	return out, http.StatusOK, nil
}

// subjectOfUser is the identity-provider subject of userID, "" when unknown.
func (h *handler) subjectOfUser(ctx context.Context, userID string) string {
	var sub string
	_ = h.db.QueryRow(ctx, `SELECT keycloak_sub FROM identity."user" WHERE id = $1::uuid`, userID).Scan(&sub)
	return sub
}
