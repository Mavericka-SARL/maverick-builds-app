package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// Tenant routing.
//
// With dedicated databases, a request has to know which one it belongs to
// before any tenant data — including the user row behind the actor — can be
// read. That question is answered here, once per request, from three sources
// in order:
//
//  1. X-Tenant-Id, for a platform admin acting on a tenant that is not their
//     own (the console sends it from the tenant whose card is being used);
//  2. X-App-Id, resolved through the application directory, which covers
//     every business and developer request;
//  3. the caller's own membership in the user directory.
//
// Nothing found means the control plane, which is where platform admins and
// every tenant that predates dedicated databases live. In shared mode (no
// router) the middleware does nothing at all.
const tenantHeader = "X-Tenant-Id"

// subjectOf returns the identity behind a request the same way actor
// resolution does, without touching a database — the middleware needs it
// before it knows which database to touch.
func (h *handler) subjectOf(r *http.Request) string {
	if h.devMode {
		persona := r.Header.Get("X-Dev-User")
		if persona == "" {
			persona = "dept_head"
		}
		if sub, ok := devPersonas[persona]; ok {
			return sub
		}
		return persona // a raw keycloak_sub, which resolveDevActor also accepts
	}
	if h.jwks == nil {
		return ""
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		return ""
	}
	claims, err := h.jwks.Validate(token)
	if err != nil {
		return ""
	}
	return claims.Subject
}

// tenantRouting resolves the tenant for each request and routes its queries
// to that tenant's database. Failures are never fatal: an unresolvable
// tenant leaves the request on the control plane, where the existing
// authorization checks answer 401/403/404 as they always did.
func (h *handler) tenantRouting(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router := h.db.Router()
		if router == nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		sub := h.subjectOf(r)
		member, _ := router.Catalog().TenantsForUser(ctx, sub)

		target := ""
		if want := r.Header.Get(tenantHeader); want != "" {
			// Only a member, or a caller with no tenant of their own (a
			// platform admin), may aim at a tenant explicitly. A member of
			// tenant A cannot read tenant B by sending its id: the actor
			// behind the request would not exist there anyway, but refusing
			// here keeps the boundary in one place.
			if len(member) == 0 || memberOf(member, want) {
				target = want
			}
		}
		if target == "" {
			if appID, _ := ctx.Value(appIDCtxKey).(string); appID != "" {
				if owner, err := router.Catalog().TenantForApplication(ctx, appID); err == nil {
					if len(member) == 0 || memberOf(member, owner) {
						target = owner
					}
				}
			}
		}
		if target == "" && len(member) > 0 {
			target = member[0]
		}
		if target != "" {
			if pool, err := router.Pool(ctx, target); err == nil {
				r = r.WithContext(tenantdb.WithScope(ctx, tenantdb.Scope{CustomerID: target, Pool: pool}))
			} else {
				h.log.Warn().Err(err).Str("tenant", target).Msg("tenant database unavailable — falling back to the control plane")
			}
		}
		next.ServeHTTP(w, r)
	})
}

func memberOf(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// tenantCtx routes ctx to one tenant's database, for handlers that act on a
// tenant other than the caller's own (platform administration). A tenant
// without its own database, or one that is not ready, leaves ctx untouched
// so the query runs where it always did.
func (h *handler) tenantCtx(ctx context.Context, customerID string) context.Context {
	router := h.db.Router()
	if router == nil || customerID == "" {
		return ctx
	}
	if cur, ok := tenantdb.ScopeFrom(ctx); ok && cur.CustomerID == customerID {
		return ctx
	}
	pool, err := router.Pool(ctx, customerID)
	if err != nil {
		return ctx
	}
	return tenantdb.WithScope(ctx, tenantdb.Scope{CustomerID: customerID, Pool: pool})
}

// tenantCtxForApplication is tenantCtx for a request that knows only an
// application id.
func (h *handler) tenantCtxForApplication(ctx context.Context, applicationID string) context.Context {
	router := h.db.Router()
	if router == nil || applicationID == "" {
		return ctx
	}
	owner, err := router.Catalog().TenantForApplication(ctx, applicationID)
	if err != nil {
		return ctx
	}
	return h.tenantCtx(ctx, owner)
}

// noteApplication records which tenant database holds a new application, so
// later requests carrying only X-App-Id can be routed to it.
func (h *handler) noteApplication(ctx context.Context, applicationID string) {
	router := h.db.Router()
	customerID := tenantdb.TenantFrom(ctx)
	if router == nil || customerID == "" || applicationID == "" {
		return
	}
	if err := router.Catalog().AddApplication(ctx, applicationID, customerID); err != nil {
		h.log.Error().Err(err).Str("application", applicationID).Str("tenant", customerID).
			Msg("application directory write failed — requests carrying only X-App-Id will not route to this tenant")
	}
}

// forgetApplication removes a deleted application from the directory.
func (h *handler) forgetApplication(ctx context.Context, applicationID string) {
	if router := h.db.Router(); router != nil && applicationID != "" {
		_ = router.Catalog().RemoveApplication(ctx, applicationID)
	}
}

// noteUser records that an identity has a user row in the tenant ctx is
// routed to, which is how that person's later requests find their database.
func (h *handler) noteUser(ctx context.Context, keycloakSub, email string) {
	router := h.db.Router()
	customerID := tenantdb.TenantFrom(ctx)
	if router == nil || customerID == "" || keycloakSub == "" {
		return
	}
	if err := router.Catalog().AddUser(ctx, keycloakSub, customerID, email); err != nil {
		h.log.Error().Err(err).Str("tenant", customerID).
			Msg("user directory write failed — this user's requests will not route to their tenant")
	}
}

// forgetUser removes a deleted user's membership.
func (h *handler) forgetUser(ctx context.Context, keycloakSub string) {
	router := h.db.Router()
	customerID := tenantdb.TenantFrom(ctx)
	if router == nil || customerID == "" || keycloakSub == "" {
		return
	}
	_ = router.Catalog().RemoveUser(ctx, keycloakSub, customerID)
}

// catalogTenants returns the dedicated tenants an actor may see: every one
// for a platform admin, their own memberships otherwise. Empty in shared
// mode.
func (h *handler) catalogTenants(ctx context.Context, a *actor, sub string) ([]tenantdb.Tenant, error) {
	router := h.db.Router()
	if router == nil {
		return nil, nil
	}
	all, err := router.Catalog().List(ctx)
	if err != nil {
		return nil, err
	}
	if a.hasRole("platform_admin") {
		return all, nil
	}
	member, err := router.Catalog().TenantsForUser(ctx, sub)
	if err != nil || len(member) == 0 {
		return nil, err
	}
	var out []tenantdb.Tenant
	for _, t := range all {
		if memberOf(member, t.CustomerID) {
			out = append(out, t)
		}
	}
	return out, nil
}

// provisionTenant gives a new tenant its own database. Returns the customer
// and default workspace ids, exactly like the shared-database path.
func (h *handler) provisionTenant(ctx context.Context, name, plan string) (customerID, workspaceID string, err error) {
	router := h.db.Router()
	if router == nil {
		return "", "", errors.New("tenant provisioning is not configured")
	}
	p, err := router.Provision(ctx, name, plan)
	if err != nil {
		return "", "", err
	}
	return p.Tenant.CustomerID, p.WorkspaceID, nil
}
