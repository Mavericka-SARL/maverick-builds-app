package gateway

// A second model in the same app used to make the first one's data vanish:
// resolveDemoModelID always picked the NEWEST model, so a request scoped to
// a revision of the older model resolved to the wrong model and every list
// came back empty/foreign (found live: creating model "solo" hid model
// "test"). Now a revision_id query param pins the model — but only within
// the already-authorized app; a foreign app's revision changes nothing.

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

func TestResolveModelFollowsRequestedRevision(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	custID := q(`INSERT INTO core.customer (name) VALUES ('C') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, wsID, custID)
	oldModel := q(`INSERT INTO core.model (application_id, name, created_at) VALUES ($1::uuid, 'test', now() - interval '1 day') RETURNING id::text`, appID)
	newModel := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'solo') RETURNING id::text`, appID)
	oldRev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, oldModel)
	userID := q(`INSERT INTO identity.user (keycloak_sub, email, customer_id) VALUES ('dev-sub', 'dev@t.com', $1::uuid) RETURNING id::text`, custID)
	q(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid) RETURNING id::text`, userID, wsID)

	// Foreign app + model + revision: naming ITS revision must never
	// switch the resolution outside the authorized app.
	otherApp := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Other', 'planning') RETURNING id::text`, wsID, custID)
	otherModel := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'other') RETURNING id::text`, otherApp)
	otherRev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'W') RETURNING id::text`, otherModel)

	h := &handler{db: tenantdb.NewHandle(pool, nil), log: logger.New("test"), devMode: true}

	resolve := func(revParam string) string {
		t.Helper()
		r := httptest.NewRequestWithContext(
			context.WithValue(ctx, appIDCtxKey, appID),
			"GET", "/api/developer/model"+revParam, nil)
		r.Header.Set("X-Dev-User", "dev-sub")
		id, err := h.resolveDemoModelID(r.Context(), r)
		if err != nil {
			t.Fatalf("resolve (%s): %v", revParam, err)
		}
		return id
	}

	if got := resolve(""); got != newModel {
		t.Errorf("no revision param: resolved %s, want newest model %s", got, newModel)
	}
	if got := resolve("?revision_id=" + oldRev); got != oldModel {
		t.Errorf("old model's revision: resolved %s, want the OLD model %s — its data must not vanish", got, oldModel)
	}
	if got := resolve("?revision_id=" + otherRev); got != newModel {
		t.Errorf("foreign app's revision: resolved %s, want fallback to newest in-app model %s", got, newModel)
	}
}
