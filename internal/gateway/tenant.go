package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// Tenant routing.
//
// With dedicated databases, a request has to know which one it belongs to
// before any tenant data — including the user row behind the actor — can be
// read. That question is answered here, once per request, from three sources
// in order:
//
//  1. X-Tenant-Id, for a platform admin acting on a tenant (the console sends
//     it from the tenant whose card or users are being worked on), or a
//     person addressing one of their homes; a tenant without a database of
//     its own names the control plane;
//  2. X-App-Id, resolved through the application directory, which covers
//     every business and developer request; an application missing from it
//     is the control plane's;
//  3. the caller's own membership in the user directory.
//
// Nothing found means the control plane, which is where platform admins and
// every tenant that predates dedicated databases live. A person the control
// plane holds is addressed there by one of its tenants or applications,
// even when dedicated tenants hold them too; one with platform reach there
// (a platform admin, a platform-wide builder) is routed as having no tenant
// of their own. In shared mode (no router) the middleware does nothing at
// all.
const tenantHeader = "X-Tenant-Id"

// subjectOf returns the identity behind a request the same way actor
// resolution does, without touching a database — the middleware needs it
// before it knows which database to touch.
func (h *handler) subjectOf(r *http.Request) string {
	if sub, ok := delegatedSubject(r.Context()); ok {
		return sub // a connector's read (mcp.go), verified at /mcp
	}
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
	claims, err := h.restClaims(token)
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
		catalog := router.Catalog()
		// member: the dedicated tenants holding an active account of the
		// caller's, oldest first. A home whose account is deactivated is no
		// home: it was the default route, and one tenant deactivating its
		// row answered 401 to every request the person made elsewhere.
		member, _ := catalog.TenantsForUser(ctx, sub)
		member = h.activeHomes(ctx, sub, member)
		// platformLevel: platform reach in the control plane (a platform
		// admin or a platform-wide builder, whose roles are the control
		// plane's even when a tenant also holds them): any tenant may be
		// addressed, and nothing addressed is the control plane. It used to
		// be anyone with no membership too — a first sign-in through one
		// tenant's identity provider, aimed at another tenant, was served
		// there with the account its provider's tenant had just made, and
		// passed that tenant's administrator gates (2026-10-01). inControl:
		// the control plane holds the caller, so it is one of their homes,
		// addressed through one of its tenants or applications.
		p := h.controlPresenceOf(ctx, sub)
		platformLevel, inControl := p.platformReach, p.row

		target, toControl := "", false
		if want := r.Header.Get(tenantHeader); want != "" {
			// Only a member, or someone platform-level, may aim at a tenant
			// explicitly. A member of tenant A cannot read tenant B by
			// sending its id: the actor behind the request would not exist
			// there anyway, but refusing here keeps the boundary in one place.
			switch _, err := catalog.Get(ctx, want); {
			case want == controlPlaneAddress:
				toControl = platformLevel || inControl
			case errors.Is(err, tenantdb.ErrNotFound):
				// A tenant without a database of its own lives in the
				// control plane.
				toControl = platformLevel || inControl
			case err == nil && (platformLevel || memberOf(member, want)):
				target = want
			}
		}
		// A platform-level caller's administration is addressed by
		// X-Tenant-Id, never by the application last opened (X-App-Id):
		// the platform console's lists, and the deployment's notification
		// settings, followed it into one tenant.
		adminByApp := !platformLevel ||
			(!strings.HasPrefix(r.URL.Path, "/api/admin/") && r.URL.Path != "/api/notifications/settings")
		if target == "" && !toControl && adminByApp {
			if appID, _ := ctx.Value(appIDCtxKey).(string); appID != "" {
				owner, err := catalog.TenantForApplication(ctx, appID)
				switch {
				case err == nil && (platformLevel || memberOf(member, owner)):
					target = owner
				case errors.Is(err, tenantdb.ErrNotFound) && inControl:
					// Not a dedicated tenant's application: the control
					// plane's, for a caller it holds.
					toControl = true
				}
			}
		}
		if target == "" && !toControl && !platformLevel && len(member) > 0 {
			target = member[0]
		}
		if target != "" {
			if pool, err := router.Pool(ctx, target); err == nil {
				ctx = tenantdb.WithScope(ctx, tenantdb.Scope{CustomerID: target, Pool: pool})
				// Someone platform-level, addressing a tenant: their account
				// is in the control plane (platformActorOnControl).
				if platformLevel {
					ctx = context.WithValue(ctx, actorOnControlKey{}, true)
				}
				r = r.WithContext(ctx)
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

// controlPresence is what the control plane holds of a subject: a row, and
// platform reach (platform_admin, or a platform-wide builder).
type controlPresence struct{ row, platformReach bool }

// controlPresenceTTL is how long controlPresenceOf trusts what it read. It
// decides routing only: the actor's roles are read again where the request
// lands, so a demotion shows here within this time and is enforced at once.
const controlPresenceTTL = 30 * time.Second

type presenceEntry struct {
	p  controlPresence
	at time.Time
}

// controlPresenceOf reads, through a short cache, what the control plane
// holds of sub. Asked only for callers with dedicated memberships. An error
// reads as nothing held: such a caller is routed as before.
func (h *handler) controlPresenceOf(ctx context.Context, sub string) controlPresence {
	if sub == "" {
		return controlPresence{}
	}
	h.presenceMu.Lock()
	if e, ok := h.presence[sub]; ok && time.Since(e.at) < controlPresenceTTL {
		h.presenceMu.Unlock()
		return e.p
	}
	h.presenceMu.Unlock()
	var p controlPresence
	if err := h.db.Control().QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM identity."user" u
		               WHERE u.keycloak_sub = $1 AND u.disabled_at IS NULL AND NOT u.stand_in),
		       EXISTS (SELECT 1 FROM identity."user" u
		               WHERE u.keycloak_sub = $1 AND u.disabled_at IS NULL AND NOT u.stand_in
		                 AND (EXISTS (SELECT 1 FROM identity.role_assignment pa
		                              WHERE pa.user_id = u.id AND pa.role = 'platform_admin')
		                      OR `+platformWideBuilderSQL("u.id")+`))`, sub).Scan(&p.row, &p.platformReach); err != nil {
		h.log.Warn().Err(err).Msg("control-plane presence not read; routing by tenant memberships only")
		return controlPresence{}
	}
	h.presenceMu.Lock()
	if h.presence == nil || len(h.presence) > 50000 {
		h.presence = map[string]presenceEntry{}
	}
	h.presence[sub] = presenceEntry{p: p, at: time.Now()}
	h.presenceMu.Unlock()
	return p
}

// home is one database that holds a person, and their account there.
type home struct {
	// TenantID is the dedicated tenant whose database this is; "" for the
	// control plane.
	TenantID string
	// Scope routes a request's context to the home's database
	// (tenantdb.WithScope(ctx, hm.Scope)); the zero Scope is the control
	// plane, or the one database.
	Scope tenantdb.Scope
	Actor *actor
}

// routedScope is the scope ctx is routed to, the zero Scope for the control
// plane or the one database.
func routedScope(ctx context.Context) tenantdb.Scope {
	s, _ := tenantdb.ScopeFrom(ctx)
	return s
}

// homesOf resolves the caller separately in each database that holds them:
// every dedicated tenant the directory lists, and the control plane when it
// holds them. A list that spans a person's homes runs once per home, with
// that home's context and that home's actor, and tags its rows so that what
// the console does next with a row is addressed to the row's home and
// re-resolved there. Lists used to read only the database a request was
// routed to: a member of a second tenant never saw its applications, tasks
// or notifications — and one list carried the routed database's roles into
// the others (2026-09-30).
//
// Rules: homes come from the directory and the control plane, never from
// the request; an actor is looked up by subject in each database
// (actorByKeycloakSub, which skips stand-ins and disabled rows and never
// honours a tenant's platform_admin), never carried from another; nothing
// here creates or changes an account; a database that cannot be reached is
// skipped. With a single database it is the routed one, with routed.
func (h *handler) homesOf(ctx context.Context, r *http.Request, routed *actor) []home {
	router := h.db.Router()
	if router == nil {
		return []home{{Scope: routedScope(ctx), Actor: routed}}
	}
	sub := h.subjectOf(r)
	if sub == "" {
		return nil
	}
	member, err := router.Catalog().TenantsForUser(ctx, sub)
	if err != nil {
		h.log.Warn().Err(err).Msg("tenant memberships not read; listing the control plane only")
	}
	var out []home
	for _, id := range member {
		pool, err := router.Pool(ctx, id)
		if err != nil {
			h.log.Warn().Err(err).Str("tenant", id).Msg("tenant database unavailable — left out of the person's lists")
			continue
		}
		hctx := tenantdb.WithScope(ctx, tenantdb.Scope{CustomerID: id, Pool: pool})
		if a, err := h.actorByKeycloakSub(hctx, sub); err == nil {
			out = append(out, home{TenantID: id, Scope: tenantdb.Scope{CustomerID: id, Pool: pool}, Actor: a})
		}
	}
	if len(member) == 0 || h.controlPresenceOf(ctx, sub).row {
		cctx := controlCtx(ctx)
		if a, err := h.actorByKeycloakSub(cctx, sub); err == nil {
			out = append(out, home{Actor: a})
		}
	}
	return out
}

// platformHomes is every database, for a platform-level caller a (a platform
// admin or a platform-wide builder): the control plane, then each dedicated
// tenant in the catalog whose database is ready; the others are returned in
// unavailable. a is marked as resolved from the control plane, so
// isGlobalBuilder answers from it in every database. The control plane is
// read whatever the request was routed to: a platform admin's views used to
// follow the application last opened (X-App-Id) into one tenant's database.
func (h *handler) platformHomes(ctx context.Context, a *actor) (homes []home, unavailable []tenantdb.Tenant) {
	pa := *a
	if !pa.onControl {
		pa.platformWide = h.isGlobalBuilder(controlCtx(ctx), a)
		pa.onControl = true
	}
	router := h.db.Router()
	if router == nil {
		return []home{{Scope: routedScope(ctx), Actor: &pa}}, nil
	}
	homes = append(homes, home{Actor: &pa})
	tenants, err := router.Catalog().List(ctx)
	if err != nil {
		h.log.Warn().Err(err).Msg("tenant catalog not read; listing the control plane only")
		return homes, nil
	}
	for _, t := range tenants {
		if t.Status != tenantdb.StatusReady {
			unavailable = append(unavailable, t)
			continue
		}
		pool, err := router.Pool(ctx, t.CustomerID)
		if err != nil {
			t.Status, t.Error = tenantdb.StatusFailed, err.Error()
			unavailable = append(unavailable, t)
			continue
		}
		homes = append(homes, home{TenantID: t.CustomerID, Scope: tenantdb.Scope{CustomerID: t.CustomerID, Pool: pool}, Actor: &pa})
	}
	return homes, unavailable
}

// isPlatformLevel reports whether a sees every database: a platform admin
// or a platform-wide builder.
func (h *handler) isPlatformLevel(ctx context.Context, a *actor) bool {
	return a.hasRole("platform_admin") || h.isGlobalBuilder(ctx, a)
}

// controlPlaneAddress is the X-Tenant-Id that names the control plane, for a
// caller it holds: what the console sends to act on a row a list across
// databases found there (homeAddress). It is no tenant's id.
const controlPlaneAddress = "control-plane"

// homeAddress is how the console addresses a home: its tenant's id,
// controlPlaneAddress for the control plane, "" with a single database.
func (h *handler) homeAddress(tenantID string) string {
	if tenantID != "" || h.db.Router() == nil {
		return tenantID
	}
	return controlPlaneAddress
}

// tenantNames maps each dedicated tenant's id to its name; empty with a
// single database.
func (h *handler) tenantNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	if router := h.db.Router(); router != nil {
		if all, err := router.Catalog().List(ctx); err == nil {
			for _, t := range all {
				out[t.CustomerID] = t.Name
			}
		}
	}
	return out
}

// activeHomeTTL is how long activeHomes trusts what it read, for routing
// only, as controlPresenceTTL.
const activeHomeTTL = controlPresenceTTL

type activeEntry struct {
	active bool
	at     time.Time
}

// activeHomes keeps, of the dedicated tenants member lists for sub, those
// whose database holds an active account of theirs (not deactivated, not a
// stand-in), through a short cache. A tenant whose database cannot be read
// is left out.
func (h *handler) activeHomes(ctx context.Context, sub string, member []string) []string {
	router := h.db.Router()
	if router == nil || len(member) == 0 {
		return member
	}
	out := member[:0:0]
	for _, id := range member {
		key := id + "/" + sub
		h.presenceMu.Lock()
		e, ok := h.active[key]
		h.presenceMu.Unlock()
		if !ok || time.Since(e.at) >= activeHomeTTL {
			e = activeEntry{at: time.Now()}
			if pool, err := router.Pool(ctx, id); err == nil {
				if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identity."user"
					WHERE keycloak_sub = $1 AND disabled_at IS NULL AND NOT stand_in)`, sub).Scan(&e.active); err != nil {
					h.log.Warn().Err(err).Str("tenant", id).Msg("account state not read; tenant left out of routing")
				}
			}
			h.presenceMu.Lock()
			if h.active == nil || len(h.active) > 50000 {
				h.active = map[string]activeEntry{}
			}
			h.active[key] = e
			h.presenceMu.Unlock()
		}
		if e.active {
			out = append(out, id)
		}
	}
	return out
}
