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

	// A NEWER instance without metric scope supersedes for its members and,
	// awaiting its approval, locks everything at CA.
	allID := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, status, context, started_at, started_by, steps_snapshot)
	            VALUES ($1::uuid, 'running', '{"country":"CA"}'::jsonb, now() + interval '1 minute', $2::uuid,
	                    '[{"id":"r1","type":"approval"}]'::jsonb) RETURNING id::text`, defID, f.userID)
	q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status) VALUES ($1::uuid, 'r1', 'in_progress') RETURNING id::text`, allID)
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

// TestWorkflowLocksOnlyWhileAwaitingApproval: a running instance locks its
// scope while an approval step is open, not while it is at a task — a
// correction round's tasks change the data they are about (found running a
// sales target round scoped to a region, 2026-10-05). An approved instance
// stays locked; a running one past an earlier approval, back at a task (a
// rework), does not.
func TestWorkflowLocksOnlyWhileAwaitingApproval(t *testing.T) {
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
	dimID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'sales_region') RETURNING id::text`, f.modelID, f.revisionID)
	euID := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'EU', 'Europe') RETURNING id::text`, dimID)
	var appID string
	if err := pool.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, f.modelID).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	schema := `[{"key":"region","data_type":"Dimension member","dimension_id":"` + dimID + `"}]`
	steps := `[{"id":"correct","type":"task"},{"id":"check","type":"condition"},{"id":"review","type":"approval"}]`
	defID := q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, context_schema, status)
	            VALUES ($1::uuid, 'Target round', 'manual', $2::jsonb, $3::jsonb, 'published') RETURNING id::text`, appID, steps, schema)
	inst := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, status, context, started_by, steps_snapshot)
	           VALUES ($1::uuid, 'running', '{"region":"EU"}'::jsonb, $2::uuid, $3::jsonb) RETURNING id::text`, defID, f.userID, steps)
	correct := q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status) VALUES ($1::uuid, 'correct', 'in_progress') RETURNING id::text`, inst)
	review := q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status) VALUES ($1::uuid, 'review', 'pending') RETURNING id::text`, inst)
	locked := func() bool {
		t.Helper()
		l, _, err := WorkflowLockReason(ctx, pool, f.modelID, []string{euID})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	if locked() {
		t.Error("EU is locked while the round is at its correction task")
	}
	exec(`UPDATE workflow.workflow_step SET status='completed', decision='complete', completed_at=now() WHERE id=$1::uuid`, correct)
	exec(`UPDATE workflow.workflow_step SET status='in_progress' WHERE id=$1::uuid`, review)
	if !locked() {
		t.Error("EU is not locked while its review awaits a decision")
	}
	// Rejected: back to the task for another round — editable again.
	exec(`UPDATE workflow.workflow_step SET status='completed', decision='reject', completed_at=now() WHERE id=$1::uuid`, review)
	q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status) VALUES ($1::uuid, 'correct', 'in_progress') RETURNING id::text`, inst)
	if locked() {
		t.Error("EU is locked after the review sent the round back to its task")
	}
	// Approved and completed: locked.
	exec(`UPDATE workflow.workflow_step SET status='completed', decision='complete', completed_at=now() WHERE instance_id=$1::uuid AND status='in_progress'`, inst)
	q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status, decision, completed_at) VALUES ($1::uuid, 'review', 'completed', 'approve', now() + interval '1 second') RETURNING id::text`, inst)
	exec(`UPDATE workflow.workflow_instance SET status='completed' WHERE id=$1::uuid`, inst)
	if !locked() {
		t.Error("EU is not locked after its round was approved")
	}
}

// A company-wide round (no member in its context) locks its revision while
// its approval is open — any write, any member, any scenario — and only then:
// at a task, after a rejection and after approval the revision is writable
// (user's decision 2026-10-16), another revision is never locked, and a
// record approval locks no grid values.
func TestCompanyWideRoundLocksRevisionWhileAwaitingApproval(t *testing.T) {
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	var appID string
	if err := pool.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, f.modelID).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	otherRev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Draft') RETURNING id::text`, f.modelID)
	dimID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'cost_type') RETURNING id::text`, f.modelID, f.revisionID)
	memberID := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'BONUS', 'Bonus') RETURNING id::text`, dimID)
	steps := `[{"id":"plan","type":"task","name":"Plan"},{"id":"review","type":"approval","name":"Management review"}]`
	defID := q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, context_schema, status)
	            VALUES ($1::uuid, 'HR Planning Round', 'manual', $2::jsonb, '[]'::jsonb, 'published') RETURNING id::text`, appID, steps)
	inst := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, status, context, started_by, steps_snapshot)
	           VALUES ($1::uuid, 'running', jsonb_build_object('model_id', $2::text, 'revision_id', $3::text), $4::uuid, $5::jsonb) RETURNING id::text`,
		defID, f.modelID, f.revisionID, f.userID, steps)
	plan := q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status) VALUES ($1::uuid, 'plan', 'in_progress') RETURNING id::text`, inst)
	review := q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status) VALUES ($1::uuid, 'review', 'pending') RETURNING id::text`, inst)
	write := func(rev string, members []string) string {
		t.Helper()
		reason, err := CheckWriteMetrics(ctx, pool, f.modelID, rev, f.userID, members, nil)
		if err != nil {
			t.Fatal(err)
		}
		return reason
	}
	if r := write(f.revisionID, []string{memberID}); r != "" {
		t.Errorf("locked at the round's planning task: %s", r)
	}
	exec(`UPDATE workflow.workflow_step SET status='completed', decision='complete', completed_at=now() WHERE id=$1::uuid`, plan)
	exec(`UPDATE workflow.workflow_step SET status='in_progress' WHERE id=$1::uuid`, review)
	if r := write(f.revisionID, []string{memberID}); !strings.Contains(r, "HR Planning Round") || !strings.Contains(r, "Management review") {
		t.Errorf("a member write during the open approval = %q, want a lock naming the round and its step", r)
	}
	if r := write(f.revisionID, nil); r == "" {
		t.Error("a dimensionless write (a setup value) during the open approval was allowed")
	}
	if r := write(otherRev, []string{memberID}); r != "" {
		t.Errorf("another revision is locked: %s", r)
	}
	// A record approval waiting for a decision locks no grid values.
	recDef := q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, context_schema, status)
	             VALUES ($1::uuid, 'Expense approval', 'manual', $2::jsonb, '[]'::jsonb, 'published') RETURNING id::text`, appID, `[{"id":"ok","type":"approval"}]`)
	recInst := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, status, context, started_by)
	              VALUES ($1::uuid, 'running', jsonb_build_object('record_id', gen_random_uuid()::text, 'revision_id', $2::text), $3::uuid) RETURNING id::text`, recDef, otherRev, f.userID)
	q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status) VALUES ($1::uuid, 'ok', 'in_progress') RETURNING id::text`, recInst)
	if r := write(otherRev, []string{memberID}); r != "" {
		t.Errorf("a record approval locked the revision: %s", r)
	}
	// Approved: writable again.
	exec(`UPDATE workflow.workflow_step SET status='completed', decision='approve', completed_at=now() WHERE id=$1::uuid`, review)
	exec(`UPDATE workflow.workflow_instance SET status='completed' WHERE id=$1::uuid`, inst)
	if r := write(f.revisionID, []string{memberID}); r != "" {
		t.Errorf("still locked after approval: %s", r)
	}
}

// TestWorkflowLockIsTheCrossingOfItsFields: an approval with a Region and a
// Version field locks Europe's budget — not France's actuals (refused until
// 2026-10-25: the fields were OR-ed, so a budget approval stopped the
// monthly actuals), nor another region's budget. A field left empty does
// not narrow; a write without the Version dimension (a France driver) is
// inside Europe's budget; a write with neither dimension is outside. The
// grid gets the same scope (WorkflowLockScopes), with the reason by label.
func TestWorkflowLockIsTheCrossingOfItsFields(t *testing.T) {
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
	member := func(dimID, code, label, parentID string) string {
		if parentID == "" {
			return q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, $2, $3) RETURNING id::text`, dimID, code, label)
		}
		return q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id) VALUES ($1::uuid, $2, $3, $4::uuid) RETURNING id::text`, dimID, code, label, parentID)
	}
	regionID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'region') RETURNING id::text`, f.modelID, f.revisionID)
	versionID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'version') RETURNING id::text`, f.modelID, f.revisionID)
	europe := member(regionID, "EUROPE", "Europe", "")
	france := member(regionID, "FR", "France", europe)
	member(regionID, "DE", "Germany", europe)
	us := member(regionID, "US", "United States", member(regionID, "AMER", "Americas", ""))
	budget := member(versionID, "BUDGET", "Budget", "")
	actual := member(versionID, "ACTUAL", "Actual", "")
	unitsID := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'units', true) RETURNING id::text`, f.modelID, f.revisionID)

	var appID string
	if err := pool.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, f.modelID).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	schema := `[{"key":"region","data_type":"Dimension member","dimension_id":"` + regionID + `"},{"key":"version","data_type":"Dimension member","dimension_id":"` + versionID + `"}]`
	defID := q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, context_schema, status)
	            VALUES ($1::uuid, 'Budget approval', 'manual', '[]', $2::jsonb, 'published') RETURNING id::text`, appID, schema)
	instID := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, status, context, started_by)
	             VALUES ($1::uuid, 'completed', '{"region":"EUROPE","version":"BUDGET"}'::jsonb, $2::uuid) RETURNING id::text`, defID, f.userID)
	q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status, decision, completed_at)
	   VALUES ($1::uuid, 'a1', 'completed', 'approve', now()) RETURNING id::text`, instID)

	check := func(members ...string) (bool, string) {
		t.Helper()
		locked, reason, err := WorkflowLockReasonForMetrics(ctx, pool, f.modelID, members, []string{unitsID})
		if err != nil {
			t.Fatal(err)
		}
		return locked, reason
	}
	if locked, reason := check(france, actual); locked {
		t.Errorf("France · Actual refused by Europe's budget approval: %s", reason)
	}
	if locked, reason := check(france, budget); !locked || reason != "Europe · Budget is locked: a workflow about it is awaiting approval or was approved" {
		t.Errorf("France · Budget: locked=%v %q, want locked as Europe · Budget", locked, reason)
	}
	if locked, _ := check(us, budget); locked {
		t.Error("United States · Budget refused by Europe's approval")
	}
	if locked, _ := check(france); !locked {
		t.Error("a France value without a version is part of Europe's budget, and must be locked")
	}
	if locked, _ := check(); locked {
		t.Error("a value with neither region nor version is locked by a regional approval")
	}

	scopes, err := WorkflowLockScopes(ctx, pool, f.modelID, []LockMetric{{ID: unitsID, Name: "units"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || len(scopes[0].Fields) != 2 || len(scopes[0].MetricIDs) != 1 ||
		strings.Join(scopes[0].Fields[0][regionID], ",") != "DE,EUROPE,FR" || strings.Join(scopes[0].Fields[1][versionID], ",") != "BUDGET" ||
		!strings.HasPrefix(scopes[0].Reason, "Europe · Budget is locked") {
		t.Errorf("scopes = %+v", scopes)
	}

	// A newer instance with the Version field left empty locks every
	// version in Europe: an empty field does not narrow.
	q(`INSERT INTO workflow.workflow_instance (workflow_def_id, status, context, started_at, started_by, steps_snapshot)
	   VALUES ($1::uuid, 'running', '{"region":"EUROPE","version":""}'::jsonb, now() + interval '1 minute', $2::uuid, '[{"id":"r1","type":"approval"}]'::jsonb) RETURNING id::text`, defID, f.userID)
	q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status) SELECT id, 'r1', 'in_progress' FROM workflow.workflow_instance WHERE context->>'version' = '' RETURNING id::text`)
	if locked, reason := check(france, actual); !locked || !strings.HasPrefix(reason, "Europe is locked") {
		t.Errorf("France · Actual with Europe's all-version review open: locked=%v %q", locked, reason)
	}
}

// An approved instance stays locked whatever steps follow its approval: a
// notification step after it records "sent", a task "complete", and either
// used to be read as the instance's last decision, releasing the agreed
// numbers (decided 2026-10-06). A rejection after an approval — a second
// approval step — still releases them.
func TestApprovedInstanceStaysLockedAfterLaterSteps(t *testing.T) {
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
	dimID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'sales_region') RETURNING id::text`, f.modelID, f.revisionID)
	euID := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'EU', 'Europe') RETURNING id::text`, dimID)
	var appID string
	if err := pool.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, f.modelID).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	schema := `[{"key":"region","data_type":"Dimension member","dimension_id":"` + dimID + `"}]`
	steps := `[{"id":"review","type":"approval"},{"id":"tell","type":"notification"},{"id":"file","type":"task"},{"id":"board","type":"STEP_TYPE_APPROVAL"}]`
	defID := q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, context_schema, status)
	            VALUES ($1::uuid, 'Budget approval', 'manual', $2::jsonb, $3::jsonb, 'published') RETURNING id::text`, appID, steps, schema)
	inst := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, status, context, started_by, steps_snapshot)
	           VALUES ($1::uuid, 'completed', '{"region":"EU"}'::jsonb, $2::uuid, $3::jsonb) RETURNING id::text`, defID, f.userID, steps)
	step := func(id, decision string, after int) {
		t.Helper()
		q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status, decision, completed_at)
		   VALUES ($1::uuid, $2, 'completed', $3, now() + make_interval(secs => $4)) RETURNING id::text`, inst, id, decision, after)
	}
	locked := func() bool {
		t.Helper()
		l, _, err := WorkflowLockReason(ctx, pool, f.modelID, []string{euID})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	step("review", "approve", 1)
	step("tell", "sent", 2)
	step("file", "complete", 3)
	if !locked() {
		t.Error("EU is released by the notification and task steps after its approval")
	}
	step("board", "reject", 4)
	if locked() {
		t.Error("EU is still locked after a later approval step rejected it")
	}
}
