package gateway

// What an account opens, decided once.
//
// An application or a model is opened by a request (actorCanAccessApp,
// actorCanAccessModel), listed by the console (appsIn, tenantsIn) and chosen
// as the one a request works in (resolveDemoModelID, headerModelInApp,
// pinModelForRevision) by the functions below, which answer for many ids at
// once. The lists and the resolution used to restate the rule in SQL of
// their own and drifted from the gate: a tenant admin who is also a
// developer, narrowed by application and model grants it had given itself,
// opened every application of its tenant — the admin arm, which grants
// never narrow — while Developer › Developer Models and Tenant admin ›
// Applications left one of them out, and the model list left out its model
// (2026-10-31). A list now asks the gate, so it shows what the gate opens on
// the list's own route and nothing else.

import (
	"context"

	"github.com/google/uuid"
)

// openableApps reports which of appIDs a opens on the route ctx arrived
// through, keyed by the ids as given. The rule is actorCanAccessApp's: a
// platform admin every application, and a platform-wide builder every one
// outside the administrator-only routes; a tenant admin every application of
// a tenant it administers (builderAdminScope narrows that on a
// developer-only route); beyond them, what a's roles open on the route,
// narrowed by its application and model grants (rolesReachApps).
func (h *handler) openableApps(ctx context.Context, a *actor, appIDs []string) (map[string]bool, error) {
	open, _, err := h.openableAppsBy(ctx, a, appIDs)
	return open, err
}

// openableAppsBy is openableApps, with the applications a opens by its scope
// alone — platform reach or tenant administration, which no grant narrows —
// told apart from those its roles open.
func (h *handler) openableAppsBy(ctx context.Context, a *actor, appIDs []string) (open, byScope map[string]bool, err error) {
	return h.openable(ctx, a, "app", appIDs)
}

// openableModels is openableApps for models.
func (h *handler) openableModels(ctx context.Context, a *actor, modelIDs []string) (map[string]bool, error) {
	open, _, err := h.openable(ctx, a, "model", modelIDs)
	return open, err
}

// openable answers openableAppsBy for kind "app" and openableModels for kind
// "model".
func (h *handler) openable(ctx context.Context, a *actor, kind string, given []string) (open, byScope map[string]bool, err error) {
	open, byScope = map[string]bool{}, map[string]bool{}
	if len(given) == 0 {
		return open, byScope, nil
	}
	// A platform-wide builder is a developer of every tenant, which opens
	// nothing on an administrator-only route (adminTiers).
	if a.hasRole("platform_admin") || (!onAdminRoute(ctx) && h.isGlobalBuilder(ctx, a)) {
		for _, id := range given {
			open[id], byScope[id] = true, true
		}
		return open, byScope, nil
	}
	ids := canonicalIDs(given)
	scoped := map[string]bool{}
	if a.hasRole("tenant_admin") {
		if err := h.adminScopeOpens(ctx, a, kind, ids.list, scoped); err != nil {
			return nil, nil, err
		}
	}
	reach := h.rolesReachApps
	if kind == "model" {
		reach = h.rolesReachModels
	}
	reached, err := reach(ctx, a, notIn(ids.list, scoped))
	if err != nil {
		return nil, nil, err
	}
	ids.mark(scoped, byScope)
	ids.mark(scoped, open)
	ids.mark(reached, open)
	return open, byScope, nil
}

// firstOpenableModel is the model of appID a request works in when it names
// none: the first model of the application that a opens, in the order every
// resolution uses — the default, else the newest, else name then id (models
// made in one transaction, sign-up's starter models, share a created_at), so
// the same request never resolves two different models. "" when a opens
// none of them.
func (h *handler) firstOpenableModel(ctx context.Context, a *actor, appID string) (string, error) {
	rows, err := h.db.Query(ctx, `
		SELECT m.id::text FROM core.model m
		WHERE m.application_id = $1::uuid
		ORDER BY (m.id = (SELECT default_model_id FROM core.application WHERE id = $1::uuid)) IS TRUE DESC,
		         m.created_at DESC, m.name, m.id`, appID)
	if err != nil {
		return "", err
	}
	var ordered []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		ordered = append(ordered, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	open, err := h.openableModels(ctx, a, ordered)
	if err != nil {
		return "", err
	}
	for _, id := range ordered {
		if open[id] {
			return id, nil
		}
	}
	return "", nil
}

// adminScopeOpens marks in open the applications or models (kind "app" or
// "model") of ids that a's admin scope opens on the route ctx arrived
// through: those of a tenant it is tenant admin of — on a developer-only
// route, only of the tenants its developer grants reach too
// (builderAdminScope), matched by the application's own tenant.
func (h *handler) adminScopeOpens(ctx context.Context, a *actor, kind string, ids []string, open map[string]bool) error {
	if len(ids) == 0 {
		return nil
	}
	if !onDeveloperRoute(ctx) {
		t, err := h.loadAdminTiers(ctx, a)
		if err != nil {
			return err
		}
		for id, customerID := range h.ownersOf(ctx, kind, ids) {
			if t.tenantAdminOf(customerID) {
				open[id] = true
			}
		}
		return nil
	}
	all, customerIDs, err := h.builderAdminScope(ctx, a)
	if err != nil {
		return err
	}
	if all {
		for _, id := range ids {
			open[id] = true
		}
		return nil
	}
	if len(customerIDs) == 0 {
		return nil
	}
	query := `SELECT id::text FROM core.application WHERE id = ANY($1::uuid[]) AND customer_id::text = ANY($2)`
	if kind == "model" {
		query = `SELECT m.id::text FROM core.model m JOIN core.application app ON app.id = m.application_id
		         WHERE m.id = ANY($1::uuid[]) AND app.customer_id::text = ANY($2)`
	}
	return h.collectIDs(ctx, open, query, ids, customerIDs)
}

// rolesReachApps reports which of appIDs (canonical) a's roles open on the
// route ctx arrived through (reachSQL), narrowed by its application and model
// grants: an application with models opens only when one of them may be
// opened. One with no model (an execution-mode application, made to run
// workflows) opens for a role that reaches it, unless the account is narrowed
// to models — until 2026-10-26 it opened for no role, so the tenant's
// developers could not build the workflows it exists for (decided
// 2026-10-06).
func (h *handler) rolesReachApps(ctx context.Context, a *actor, appIDs []string) (map[string]bool, error) {
	reached := map[string]bool{}
	if len(appIDs) == 0 {
		return reached, nil
	}
	return reached, h.collectIDs(ctx, reached, `
		SELECT app.id::text
		FROM core.application app
		LEFT JOIN core.workspace aw ON aw.id = app.workspace_id
		WHERE app.id = ANY($1::uuid[])
		  AND `+reachSQL(ctx, "app", "aw", "$2")+`
		  AND (
		      NOT EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$2::uuid)
		      OR EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$2::uuid AND application_id=app.id)
		  )
		  AND (EXISTS (
		      SELECT 1
		      FROM core.model m
		      WHERE m.application_id=app.id
		        AND (
		            NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid)
		            OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid AND model_id=m.id)
		        )
		  ) OR (
		      NOT EXISTS (SELECT 1 FROM core.model m0 WHERE m0.application_id=app.id)
		      AND NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid)
		  ))`, appIDs, a.UserID)
}

// rolesReachModels is rolesReachApps for models.
func (h *handler) rolesReachModels(ctx context.Context, a *actor, modelIDs []string) (map[string]bool, error) {
	reached := map[string]bool{}
	if len(modelIDs) == 0 {
		return reached, nil
	}
	return reached, h.collectIDs(ctx, reached, `
		SELECT m.id::text
		FROM core.model m
		JOIN core.application app ON app.id=m.application_id
		LEFT JOIN core.workspace aw ON aw.id = app.workspace_id
		WHERE m.id = ANY($1::uuid[])
		  AND `+reachSQL(ctx, "app", "aw", "$2")+`
		  AND (
		      NOT EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$2::uuid)
		      OR EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$2::uuid AND application_id=app.id)
		  )
		  AND (
		      NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid)
		      OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid AND model_id=m.id)
		  )`, modelIDs, a.UserID)
}

// ownersOf is the tenant of each application or model (kind "app" or
// "model") of ids (canonical), directly or through its application's
// workspace — what customerOfApplication and customerOfModel answer one at a
// time, from the same cache.
func (h *handler) ownersOf(ctx context.Context, kind string, ids []string) map[string]string {
	owners := make(map[string]string, len(ids))
	var missing []string
	for _, id := range ids {
		if cid, ok := h.ownerFromCache(kind, id); ok {
			owners[id] = cid
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return owners
	}
	query := `SELECT a.id::text, COALESCE(a.customer_id::text, w.customer_id::text, '')
	          FROM core.application a LEFT JOIN core.workspace w ON w.id = a.workspace_id
	          WHERE a.id = ANY($1::uuid[])`
	if kind == "model" {
		query = `SELECT m.id::text, COALESCE(a.customer_id::text, w.customer_id::text, '')
		         FROM core.model m JOIN core.application a ON a.id = m.application_id
		         LEFT JOIN core.workspace w ON w.id = a.workspace_id
		         WHERE m.id = ANY($1::uuid[])`
	}
	rows, err := h.db.Query(ctx, query, missing)
	if err != nil {
		return owners
	}
	defer rows.Close()
	for rows.Next() {
		var id, cid string
		if rows.Scan(&id, &cid) == nil {
			owners[id] = cid
			h.ownerToCache(kind, id, cid)
		}
	}
	return owners
}

// collectIDs runs query and marks each id its rows return in into.
func (h *handler) collectIDs(ctx context.Context, into map[string]bool, query string, args ...any) error {
	rows, err := h.db.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		into[id] = true
	}
	return rows.Err()
}

// idSet is the ids a caller asked about, each in the canonical form the
// database returns (lowercase, hyphenated), once. An id that is no UUID names
// no application or model — and cast in a query would fail it whole.
type idSet struct {
	list  []string          // canonical, once each
	canon map[string]string // as given → canonical
}

func canonicalIDs(given []string) idSet {
	s := idSet{canon: make(map[string]string, len(given))}
	seen := make(map[string]bool, len(given))
	for _, id := range given {
		u, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		c := u.String()
		s.canon[id] = c
		if !seen[c] {
			seen[c] = true
			s.list = append(s.list, c)
		}
	}
	return s
}

// mark sets into[id] for every id as given whose canonical form found holds.
func (s idSet) mark(found, into map[string]bool) {
	for given, c := range s.canon {
		if found[c] {
			into[given] = true
		}
	}
}

// notIn is the ids that are not keys of done.
func notIn(ids []string, done map[string]bool) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !done[id] {
			out = append(out, id)
		}
	}
	return out
}
