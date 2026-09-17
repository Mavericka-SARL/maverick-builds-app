package writeguard

// Tests that internal/writeguard's core checks distinguish a genuine
// database error from a legitimate "no matching rule/row" result — the
// fail-open bug fixed here (HiddenAccess/MetricAccess used to collapse both
// cases into "unrestricted"; AncestorChain had no error return at all and
// silently truncated its walk on any error). A malformed (non-UUID-shaped)
// id string is used to force a real error: every query here casts with
// ::uuid, so a bad string produces a genuine *pgconn.PgError (SQLSTATE
// 22P02), never pgx.ErrNoRows — a clean way to distinguish the two without
// any test-only production code.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

func setupWriteguardDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	ctx := context.Background()

	pgc, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("mavericks"),
		tcpostgres.WithUsername("mavericks"),
		tcpostgres.WithPassword("mavericks"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}

	dsn, err := pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pgc.Terminate(ctx)
		t.Fatalf("connection string: %v", err)
	}
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		_ = pgc.Terminate(ctx)
		t.Fatalf("connect: %v", err)
	}
	if err := migrate.Run(ctx, pool, migrationfs.FS, "."); err != nil {
		pool.Close()
		_ = pgc.Terminate(ctx)
		t.Fatalf("migrate: %v", err)
	}

	return pool, func() {
		pool.Close()
		_ = pgc.Terminate(ctx)
	}
}

// fixture seeds one real user (for the legitimate no-rule case) and one
// real, non-system-managed revision (for CheckWrite's end-to-end test).
type fixture struct {
	userID, modelID, revisionID string
}

func seedFixture(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('T', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, appID)
	revisionID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	userID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-user', 'user@t.com', 'User', $1::uuid) RETURNING id::text`, custID)

	return fixture{userID: userID, modelID: modelID, revisionID: revisionID}
}

func TestHiddenAccessNoMatchingRuleIsUnrestricted(t *testing.T) {
	pool, cleanup := setupWriteguardDB(t)
	defer cleanup()
	f := seedFixture(t, pool)

	access, err := HiddenAccess(context.Background(), pool, f.userID, "00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("HiddenAccess: unexpected error for a legitimate no-row case: %v", err)
	}
	if access != "" {
		t.Errorf("access = %q, want \"\" (unrestricted)", access)
	}
}

func TestHiddenAccessRealErrorIsPropagated(t *testing.T) {
	pool, cleanup := setupWriteguardDB(t)
	defer cleanup()

	// A malformed userID forces a real ::uuid cast failure, not ErrNoRows.
	// Before the fix, this returned ("", nil) — indistinguishable from
	// "unrestricted" — instead of surfacing the error.
	_, err := HiddenAccess(context.Background(), pool, "not-a-uuid", "00000000-0000-0000-0000-000000000001")
	if err == nil {
		t.Fatal("expected HiddenAccess to return an error for a malformed userID, got nil")
	}
}

func TestMetricAccessRealErrorIsPropagated(t *testing.T) {
	pool, cleanup := setupWriteguardDB(t)
	defer cleanup()

	_, err := MetricAccess(context.Background(), pool, "not-a-uuid", "00000000-0000-0000-0000-000000000001")
	if err == nil {
		t.Fatal("expected MetricAccess to return an error for a malformed userID, got nil")
	}
}

func TestAncestorChainNoParentEndsChainWithoutError(t *testing.T) {
	pool, cleanup := setupWriteguardDB(t)
	defer cleanup()

	// A validly-shaped but nonexistent memberID legitimately ends the walk
	// immediately (pgx.ErrNoRows on the first hop) — not an error.
	chain, err := AncestorChain(context.Background(), pool, "00000000-0000-0000-0000-000000000001")
	if err != nil {
		t.Fatalf("AncestorChain: unexpected error for a legitimate no-row case: %v", err)
	}
	if len(chain) != 0 {
		t.Errorf("chain = %v, want empty", chain)
	}
}

func TestAncestorChainRealErrorIsPropagated(t *testing.T) {
	pool, cleanup := setupWriteguardDB(t)
	defer cleanup()

	// Before the fix, AncestorChain had no error return at all and silently
	// truncated the walk on any error, including a real one like this.
	chain, err := AncestorChain(context.Background(), pool, "not-a-uuid")
	if err == nil {
		t.Fatalf("expected AncestorChain to return an error for a malformed memberID, got chain=%v", chain)
	}
}

func TestCheckWriteFailsClosedOnRealError(t *testing.T) {
	pool, cleanup := setupWriteguardDB(t)
	defer cleanup()
	f := seedFixture(t, pool)

	// A malformed userID must cause CheckWrite to fail closed (return an
	// error) rather than treating the write as unrestricted — end-to-end
	// proof that HiddenAccess's fix is actually reachable through the
	// aggregate function every write path calls.
	reason, err := CheckWrite(context.Background(), pool, f.modelID, f.revisionID, "not-a-uuid", []string{"00000000-0000-0000-0000-000000000001"})
	if err == nil {
		t.Fatalf("expected CheckWrite to fail closed for a malformed userID, got reason=%q, nil error", reason)
	}
}

func TestCheckWriteAllowsUnrestrictedWrite(t *testing.T) {
	pool, cleanup := setupWriteguardDB(t)
	defer cleanup()
	f := seedFixture(t, pool)

	// Regression check: a real user with no access rules and a real,
	// non-system-managed revision must still be allowed to write — the
	// fail-closed fix must not have made the legitimate/common case deny.
	reason, err := CheckWrite(context.Background(), pool, f.modelID, f.revisionID, f.userID, []string{"00000000-0000-0000-0000-000000000001"})
	if err != nil {
		t.Fatalf("CheckWrite: unexpected error: %v", err)
	}
	if reason != "" {
		t.Errorf("reason = %q, want \"\" (write allowed)", reason)
	}
}

// TestMetricScopedWorkflowLockCrossing pins the metric×member crossing
// semantics: an approved instance whose context declares a "Metric" var
// locks EXACTLY that metric at its member scope — the same member's other
// metrics stay writable, other members stay writable, and a caller that
// does not say which metric it writes (nil metricIDs) is treated
// conservatively as if it might write the locked one. An instance without
// Metric vars keeps locking every metric (the original behavior).
func TestMetricScopedWorkflowLockCrossing(t *testing.T) {
	pool, cleanup := setupWriteguardDB(t)
	defer cleanup()
	ctx := context.Background()
	f := seedFixture(t, pool)
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}

	dimID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'geography') RETURNING id::text`, f.modelID, f.revisionID)
	caID := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'CA', 'Canada') RETURNING id::text`, dimID)
	usID := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'US', 'United States') RETURNING id::text`, dimID)
	revenueID := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'revenue', true) RETURNING id::text`, f.modelID, f.revisionID)
	costID := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'cost', true) RETURNING id::text`, f.modelID, f.revisionID)

	var appID string
	if err := pool.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, f.modelID).Scan(&appID); err != nil {
		t.Fatalf("app id: %v", err)
	}
	schema := `[{"key":"country","data_type":"Dimension member","dimension_id":"` + dimID + `"},{"key":"metric","data_type":"Metric"}]`
	defID := q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, context_schema, status)
	            VALUES ($1::uuid, 'Metric approval', 'manual', '[]', $2::jsonb, 'published') RETURNING id::text`, appID, schema)
	// Approved instance scoped to (CA, revenue) — metric stored by NAME,
	// exactly what ResolveStartContext normalizes to.
	instID := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, status, context, started_by)
	             VALUES ($1::uuid, 'completed', '{"country":"CA","metric":"revenue"}'::jsonb, $2::uuid) RETURNING id::text`, defID, f.userID)
	q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status, decision, completed_at)
	   VALUES ($1::uuid, 'a1', 'completed', 'approve', now()) RETURNING id::text`, instID)

	check := func(members, metrics []string) (bool, string) {
		t.Helper()
		locked, reason, err := WorkflowLockReasonForMetrics(ctx, pool, f.modelID, members, metrics)
		if err != nil {
			t.Fatalf("lock check: %v", err)
		}
		return locked, reason
	}

	if locked, reason := check([]string{caID}, []string{revenueID}); !locked {
		t.Error("CA×revenue must be locked by the approved metric-scoped instance")
	} else if !strings.Contains(reason, "Revenue") && !strings.Contains(reason, "revenue") {
		t.Errorf("lock reason should name the metric, got %q", reason)
	}
	if locked, _ := check([]string{caID}, []string{costID}); locked {
		t.Error("CA×cost must stay writable — the approval covers only revenue")
	}
	if locked, _ := check([]string{usID}, []string{revenueID}); locked {
		t.Error("US×revenue must stay writable — the approval covers only CA")
	}
	// Metrics unknown → conservative: the metric-scoped lock still blocks.
	if locked, _ := check([]string{caID}, nil); !locked {
		t.Error("CA with unknown metrics must be treated as potentially locked (no bypass)")
	}
	// An id-shaped metric context value resolves to the same name scope.
	q(`UPDATE workflow.workflow_instance SET context = jsonb_set(context, '{metric}', to_jsonb($2::text)) WHERE id=$1::uuid RETURNING id::text`, instID, revenueID)
	if locked, _ := check([]string{caID}, []string{revenueID}); !locked {
		t.Error("a metric context value stored as an ID must lock the same crossing")
	}
	if locked, _ := check([]string{caID}, []string{costID}); locked {
		t.Error("id-shaped metric scope must still leave other metrics writable")
	}

	// A NEWER instance without metric scope supersedes for its members and
	// locks everything at CA (running → locked regardless of decision).
	allID := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, status, context, started_at, started_by)
	            VALUES ($1::uuid, 'running', '{"country":"CA"}'::jsonb, now() + interval '1 minute', $2::uuid) RETURNING id::text`, defID, f.userID)
	_ = allID
	if locked, _ := check([]string{caID}, []string{costID}); !locked {
		t.Error("a newer all-metric instance must lock CA×cost too")
	}
}

// TestCancelledApprovedInstanceUnlocks: an approved instance later CANCELLED
// must release its lock — the doc always promised "cancelled = unlocked",
// but the code checked only the decision, so an admin cancelling an
// already-approved approval left its scope locked forever (found live while
// seeding data over a stale test approval).
func TestCancelledApprovedInstanceUnlocks(t *testing.T) {
	pool, cleanup := setupWriteguardDB(t)
	defer cleanup()
	ctx := context.Background()
	f := seedFixture(t, pool)
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	dimID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'geography') RETURNING id::text`, f.modelID, f.revisionID)
	deID := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'DE', 'Germany') RETURNING id::text`, dimID)
	var appID string
	if err := pool.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, f.modelID).Scan(&appID); err != nil {
		t.Fatalf("app id: %v", err)
	}
	schema := `[{"key":"country","data_type":"Dimension member","dimension_id":"` + dimID + `"}]`
	defID := q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, context_schema, status)
	            VALUES ($1::uuid, 'Approval', 'manual', '[]', $2::jsonb, 'published') RETURNING id::text`, appID, schema)
	// APPROVED then CANCELLED — the exact live shape.
	instID := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, status, context, started_by)
	             VALUES ($1::uuid, 'cancelled', '{"country":"DE"}'::jsonb, $2::uuid) RETURNING id::text`, defID, f.userID)
	q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status, decision, completed_at)
	   VALUES ($1::uuid, 'a1', 'completed', 'approve', now()) RETURNING id::text`, instID)

	locked, _, err := WorkflowLockReason(ctx, pool, f.modelID, []string{deID})
	if err != nil {
		t.Fatalf("lock check: %v", err)
	}
	if locked {
		t.Error("a cancelled (formerly approved) instance still locks its scope — cancelled must unlock")
	}
}
