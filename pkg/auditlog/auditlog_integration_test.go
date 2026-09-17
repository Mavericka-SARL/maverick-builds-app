package auditlog_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type fixture struct {
	pool                          *pgxpool.Pool
	userID, appID, modelID, revID string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	f := &fixture{pool: pool}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}

	custID := q(`INSERT INTO core.customer (name) VALUES ('Test Co') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'WS') RETURNING id::text`, custID)
	f.appID = q(`INSERT INTO core.application (workspace_id, name, mode) VALUES ($1::uuid, 'App', 'planning') RETURNING id::text`, wsID)
	f.modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Model') RETURNING id::text`, f.appID)
	f.revID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'initial') RETURNING id::text`, f.modelID)
	f.userID = q(`INSERT INTO identity.user (keycloak_sub, email) VALUES ('sub-1', 'u1@test.dev') RETURNING id::text`)

	return f
}

func countAuditRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM audit.audit_event`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestLogRoundTripsAllFields(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	log := logger.New("test")

	auditlog.Log(ctx, f.pool, log, auditlog.Fields{
		Category:      auditlog.CategoryModelChange,
		EventType:     auditlog.EventMetricCreated,
		ActorUserID:   f.userID,
		ActorRole:     "developer",
		ApplicationID: f.appID,
		ResourceType:  "metric",
		ResourceID:    "metric-123",
		RevisionID:    f.revID,
		Metadata:      map[string]string{"name": "Revenue"},
	})

	var category, eventType, actorRole, resourceType, resourceID string
	var actorUserID, applicationID, revisionID *string
	var metadata []byte
	err := f.pool.QueryRow(ctx, `
		SELECT category::text, event_type, actor_user_id::text, actor_role,
		       application_id::text, resource_type, resource_id, revision_id::text, metadata
		FROM audit.audit_event
	`).Scan(&category, &eventType, &actorUserID, &actorRole, &applicationID, &resourceType, &resourceID, &revisionID, &metadata)
	if err != nil {
		t.Fatalf("select: %v", err)
	}

	if category != "model_change" || eventType != "metric.created" || actorRole != "developer" ||
		resourceType != "metric" || resourceID != "metric-123" {
		t.Fatalf("unexpected row: category=%s event_type=%s actor_role=%s resource_type=%s resource_id=%s",
			category, eventType, actorRole, resourceType, resourceID)
	}
	if actorUserID == nil || *actorUserID != f.userID {
		t.Fatalf("actor_user_id = %v, want %s", actorUserID, f.userID)
	}
	if applicationID == nil || *applicationID != f.appID {
		t.Fatalf("application_id = %v, want %s", applicationID, f.appID)
	}
	if revisionID == nil || *revisionID != f.revID {
		t.Fatalf("revision_id = %v, want %s", revisionID, f.revID)
	}
	var meta map[string]string
	if err := json.Unmarshal(metadata, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta["name"] != "Revenue" {
		t.Fatalf("metadata[name] = %q, want %q", meta["name"], "Revenue")
	}
}

func TestLogWithNoActorAppOrRevisionStoresNulls(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	log := logger.New("test")

	auditlog.Log(ctx, f.pool, log, auditlog.Fields{
		Category:     auditlog.CategoryDataChange,
		EventType:    auditlog.EventAutomationRuleScheduledFire,
		ResourceType: "automation_rule",
		ResourceID:   "rule-1",
	})

	var actorUserID, applicationID, revisionID *string
	err := f.pool.QueryRow(ctx, `
		SELECT actor_user_id::text, application_id::text, revision_id::text
		FROM audit.audit_event
	`).Scan(&actorUserID, &applicationID, &revisionID)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if actorUserID != nil || applicationID != nil || revisionID != nil {
		t.Fatalf("expected all NULL, got actor=%v app=%v revision=%v", actorUserID, applicationID, revisionID)
	}
}

func TestLogWithBogusApplicationIDIsRejectedByFK(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	log := logger.New("test")

	auditlog.Log(ctx, f.pool, log, auditlog.Fields{
		Category:      auditlog.CategoryAdmin,
		EventType:     auditlog.EventRoleUpdated,
		ApplicationID: "00000000-0000-0000-0000-000000000099",
		ResourceType:  "business_role",
		ResourceID:    "t-1",
	})

	if n := countAuditRows(t, f.pool); n != 0 {
		t.Fatalf("expected the FK-violating insert to be rejected (row never lands), got %d rows", n)
	}
}

func TestLogWithBogusRevisionIDIsRejectedByFK(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	log := logger.New("test")

	auditlog.Log(ctx, f.pool, log, auditlog.Fields{
		Category:     auditlog.CategoryModelChange,
		EventType:    auditlog.EventRevisionActivated,
		RevisionID:   "00000000-0000-0000-0000-000000000099",
		ResourceType: "revision",
		ResourceID:   "r-1",
	})

	if n := countAuditRows(t, f.pool); n != 0 {
		t.Fatalf("expected the FK-violating insert to be rejected (row never lands), got %d rows", n)
	}
}
