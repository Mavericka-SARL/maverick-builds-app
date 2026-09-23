package plan

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// Evaluate is pure: the state a tenant is in follows from its row and its
// plan, so every branch is checked directly.
func TestEvaluate(t *testing.T) {
	p := Plan{Key: "small", Name: "Small"}

	t.Run("within limits", func(t *testing.T) {
		st := Evaluate(p, true, Tenant{LimitState: "ok"})
		if st.ReadOnly || st.Code != "" || st.Reason != "" {
			t.Fatalf("state = %+v", st)
		}
	})
	t.Run("over a limit is read-only with the sweep's reason", func(t *testing.T) {
		st := Evaluate(p, true, Tenant{LimitState: "over", LimitReason: "too many models"})
		if !st.ReadOnly || st.Code != CodeOverLimit || !strings.Contains(st.Reason, "too many models") {
			t.Fatalf("state = %+v", st)
		}
	})
	t.Run("an unknown plan is reported, never enforced", func(t *testing.T) {
		st := Evaluate(Plan{Key: "gone", Name: "gone"}, false, Tenant{LimitState: "ok"})
		if st.PlanKnown || st.ReadOnly {
			t.Fatalf("state = %+v", st)
		}
	})
}

func TestValidate(t *testing.T) {
	good := Plan{Key: "team-10", Name: "Team"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Plan{
		{Key: "Bad Key", Name: "x"},
		{Key: "ok", Name: ""},
		{Key: "ok", Name: "x", Description: strings.Repeat("x", 301)},
		{Key: "ok", Name: "x", Limits: Limits{MaxUsers: -1}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("plan %+v validated", bad)
		}
	}
}

// The catalog, the request-time checks and the sweep against a real
// database: the seeded plans, a tenant on a small plan meeting each
// limit, an unknown plan never locking anyone out, and the sweep's verdict
// making the tenant read-only and back.
func TestCatalogChecksAndSweep(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}

	plans, err := List(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	var test Plan
	for _, p := range plans {
		if p.Key == "trial" {
			t.Fatalf("the trial plan is gone since 090, but the catalog has %+v", p)
		}
		if p.Key == "test" {
			test = p
		}
	}
	// Sign-up offers the test workspace: no end date, bounded by storage.
	if test.Key == "" || !test.SelfService || test.Limits.MaxStorageMB != 100 || test.Limits.MaxModels != 0 || !strings.Contains(test.LimitNote, "own infrastructure") {
		t.Fatalf("seeded test-workspace plan = %+v", test)
	}
	if ss, ok, err := SelfService(ctx, pool); err != nil || !ok || ss.Key != "test" {
		t.Fatalf("self-service plan = %+v ok=%v err=%v", ss, ok, err)
	}
	if _, err := Get(ctx, pool, "nope"); !errors.Is(err, ErrUnknownPlan) {
		t.Fatalf("unknown plan err = %v", err)
	}
	saved, err := Upsert(ctx, pool, Plan{Key: "team", Name: " Team ", Limits: Limits{MaxUsers: 10}, SortOrder: 25})
	if err != nil || saved.Name != "Team" || saved.Limits.MaxUsers != 10 {
		t.Fatalf("upsert = %+v err=%v", saved, err)
	}
	// A small plan with every object-count limit set, so each check has a
	// number to cross.
	if _, err := Upsert(ctx, pool, Plan{Key: "small", Name: "Small", SortOrder: 26, Limits: Limits{
		MaxUsers: 5, MaxApplications: 2, MaxModels: 3, MaxMetricsPerModel: 50, MaxMembersPerDimension: 500,
		MaxFactRowsPerModel: 100000, MaxAIMessagesPerDay: 100, MaxIntegrationRunsPerDay: 50}}); err != nil {
		t.Fatal(err)
	}

	// A tenant on the small plan with two users, one app and one model.
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Smallco', 'small') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, cust)
	app := q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, cust, ws)
	model := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, app)
	rev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, model)
	q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('plan-u1', 'u1@small.test', 'U1', $1::uuid) RETURNING id::text`, cust)
	u2 := q(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('plan-u2', 'u2@small.test', 'U2') RETURNING id::text`)
	q(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid) RETURNING id::text`, u2, ws)

	e := NewEnforcer(pool)
	st, err := e.State(ctx, pool, cust)
	if err != nil || st.ReadOnly || !st.PlanKnown || st.Plan.Key != "small" {
		t.Fatalf("state = %+v err=%v", st, err)
	}
	// Within: 2 users of 5, 1 app of 2, 1 model of 3.
	for name, err := range map[string]error{
		"users": e.CheckUsers(ctx, pool, cust, 1), "apps": e.CheckApplications(ctx, pool, cust, 1), "models": e.CheckModels(ctx, pool, cust, 1),
		"metrics": e.CheckMetrics(ctx, pool, cust, model, 50), "members": e.CheckMembers(ctx, pool, cust, "", 500),
		"facts": e.CheckFactRows(ctx, pool, cust, model, 100000), "ai": e.CheckAIMessages(ctx, pool, cust), "runs": e.CheckIntegrationRuns(ctx, pool, cust),
	} {
		if err != nil {
			t.Errorf("%s within limits refused: %v", name, err)
		}
	}
	// Crossing: a fourth user is fine, a fourth application is not.
	if err := e.CheckApplications(ctx, pool, cust, 2); err == nil || !IsLimit(err) || !strings.Contains(err.Error(), "Small plan allows 2 applications; this tenant has 1") {
		t.Fatalf("apps over: %v", err)
	}
	if err := e.CheckMetrics(ctx, pool, cust, model, 51); err == nil || !strings.Contains(err.Error(), "50 metrics per model") {
		t.Fatalf("metrics over: %v", err)
	}
	// Limits are per revision for metrics: 30 in one revision and 30 in
	// another is 30, not 60.
	for i := 0; i < 30; i++ {
		q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, $3, true, 'sum') RETURNING id::text`, model, rev, "m"+strconv.Itoa(i))
	}
	rev2 := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Draft') RETURNING id::text`, model)
	for i := 0; i < 30; i++ {
		q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, $3, true, 'sum') RETURNING id::text`, model, rev2, "m"+strconv.Itoa(i))
	}
	if err := e.CheckMetrics(ctx, pool, cust, model, 20); err != nil {
		t.Fatalf("30 per revision + 20 should fit in 50: %v", err)
	}
	if err := e.CheckMetrics(ctx, pool, cust, model, 21); err == nil {
		t.Fatal("30 + 21 should not fit in 50")
	}

	// The sweep: within limits → ok; then a fourth model directly in the
	// database (a path with no request-time check) → over, read-only; then
	// removing it → ok again.
	if tt, err := e.Sweep(ctx, pool, cust); err != nil || tt.LimitState != "ok" || tt.UsageCheckedAt == nil {
		t.Fatalf("sweep within = %+v err=%v", tt, err)
	}
	extra := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		extra = append(extra, q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, app, "X"+strconv.Itoa(i)))
	}
	tt, err := e.Sweep(ctx, pool, cust)
	if err != nil || tt.LimitState != "over" || !strings.Contains(tt.LimitReason, "3 models; this tenant has 4") {
		t.Fatalf("sweep over = %+v err=%v", tt, err)
	}
	if st, _ := e.State(ctx, pool, cust); !st.ReadOnly || st.Code != CodeOverLimit {
		t.Fatalf("state after sweep = %+v", st)
	}
	for _, id := range extra {
		if _, err := pool.Exec(ctx, `DELETE FROM core.model WHERE id = $1::uuid`, id); err != nil {
			t.Fatal(err)
		}
	}
	if tt, err := e.Sweep(ctx, pool, cust); err != nil || tt.LimitState != "ok" {
		t.Fatalf("sweep back = %+v err=%v", tt, err)
	}

	// A tenant naming a plan with no row is unlimited, and says so.
	odd := q(`INSERT INTO core.customer (name, plan) VALUES ('Odd', 'legacy-gold') RETURNING id::text`)
	st, err = e.State(ctx, pool, odd)
	if err != nil || st.PlanKnown || st.ReadOnly || st.Plan.Limits.Any() {
		t.Fatalf("unknown plan state = %+v err=%v", st, err)
	}
	if err := e.CheckModels(ctx, pool, odd, 1000); err != nil {
		t.Fatalf("unknown plan must not limit: %v", err)
	}
	if tt, err := e.Sweep(ctx, pool, odd); err != nil || tt.LimitState != "ok" {
		t.Fatalf("unknown plan sweep = %+v err=%v", tt, err)
	}

	// Storage: the test database is shared, so the tenant's bytes are an
	// estimate — its data rows times the table's bytes per row (200 while
	// there are no statistics). A 1 MB plan holds 5242 such rows; 20000 is over on any statistics.
	if _, err := Upsert(ctx, pool, Plan{Key: "tiny", Name: "Tiny", Limits: Limits{MaxStorageMB: 1}, SortOrder: 26}); err != nil {
		t.Fatal(err)
	}
	if err := SetPlan(ctx, pool, cust, "tiny"); err != nil {
		t.Fatal(err)
	}
	e.Invalidate(cust)
	if b, err := StorageBytes(ctx, pool, cust); err != nil || b != 0 {
		t.Fatalf("empty tenant storage = %d err=%v", b, err)
	}
	if err := e.CheckStorage(ctx, pool, cust); err != nil {
		t.Fatalf("empty tenant refused: %v", err)
	}
	u1 := q(`SELECT id::text FROM identity.user WHERE keycloak_sub = 'plan-u1'`)
	metric := q(`SELECT id::text FROM model.metric_def WHERE model_id = $1::uuid LIMIT 1`, model)
	if _, err := pool.Exec(ctx, `INSERT INTO runtime.fact_input (model_id, revision_id, dim_members, metric_id, value, entered_by)
		SELECT $1::uuid, $2::uuid, jsonb_build_object('i', i), $3::uuid, i, $4::uuid FROM generate_series(1, 20000) i`, model, rev, metric, u1); err != nil {
		t.Fatal(err)
	}
	if b, err := StorageBytes(ctx, pool, cust); err != nil || b < 20000*100 {
		t.Fatalf("storage after 20000 rows = %d err=%v", b, err)
	}
	if err := e.CheckStorage(ctx, pool, cust); err == nil || !IsLimit(err) || !strings.Contains(err.Error(), "Tiny plan allows 1 MB of storage; this tenant uses") || !strings.HasSuffix(err.Error(), "Change the plan to add more.") {
		t.Fatalf("storage over: %v", err)
	}
	// A plan with a note says that instead of "change the plan", in every
	// refusal and in the read-only reason.
	if _, err := Upsert(ctx, pool, Plan{Key: "tiny", Name: "Tiny", Limits: Limits{MaxStorageMB: 1}, LimitNote: "Run it yourself for more.", SortOrder: 26}); err != nil {
		t.Fatal(err)
	}
	e.InvalidateAll()
	if err := e.CheckStorage(ctx, pool, cust); err == nil || !strings.HasSuffix(err.Error(), "MB. Run it yourself for more.") {
		t.Fatalf("storage over with note: %v", err)
	}
	if tt, err := e.Sweep(ctx, pool, cust); err != nil || !strings.HasSuffix(tt.LimitReason, "Run it yourself for more.") {
		t.Fatalf("sweep reason with note = %+v err=%v", tt, err)
	}
	if st, _ := e.State(ctx, pool, cust); !strings.Contains(st.Reason, "Run it yourself for more.") || strings.Contains(st.Reason, "plan is changed") {
		t.Fatalf("state reason with note = %q", st.Reason)
	}
	if tt, err := e.Sweep(ctx, pool, cust); err != nil || tt.LimitState != "over" || !strings.Contains(tt.LimitReason, "1 MB of storage") {
		t.Fatalf("storage sweep over = %+v err=%v", tt, err)
	}
	// Deleting rows archives them (081): the tenant is no smaller for it.
	if _, err := pool.Exec(ctx, `DELETE FROM runtime.fact_input WHERE model_id = $1::uuid`, model); err != nil {
		t.Fatal(err)
	}
	if n, _ := countRow(ctx, pool, `SELECT count(*) FROM runtime.fact_input_history WHERE model_id = $1::uuid`, model); n != 20000 {
		t.Fatalf("archived rows = %d, want 20000", n)
	}
	if tt, err := e.Sweep(ctx, pool, cust); err != nil || tt.LimitState != "over" {
		t.Fatalf("storage sweep after row delete = %+v err=%v", tt, err)
	}
	// Deleting the model takes its history with it (089): the way to make
	// room on a storage-bounded plan.
	if _, err := pool.Exec(ctx, `DELETE FROM core.model WHERE id = $1::uuid`, model); err != nil {
		t.Fatal(err)
	}
	if n, _ := countRow(ctx, pool, `SELECT count(*) FROM runtime.fact_input_history WHERE model_id = $1::uuid`, model); n != 0 {
		t.Fatalf("history after model delete = %d, want 0", n)
	}
	if tt, err := e.Sweep(ctx, pool, cust); err != nil || tt.LimitState != "ok" {
		t.Fatalf("storage sweep back = %+v err=%v", tt, err)
	}
	// The dedicated measurer sees a whole database: the migrated schema
	// alone is real bytes.
	if b, err := dedicatedStorageBytes(ctx, pool); err != nil || b < 1<<20 {
		t.Fatalf("dedicated storage of the test database = %d err=%v", b, err)
	}
	if megabytes(0) != 0 || megabytes(1) != 1 || megabytes(1<<20) != 1 || megabytes(1<<20+1) != 2 {
		t.Fatal("megabytes rounds up")
	}

	// SetPlan moves the tenant and the cache follows an Invalidate.
	if err := SetPlan(ctx, pool, cust, "team"); err != nil {
		t.Fatal(err)
	}
	e.Invalidate(cust)
	if st, _ := e.State(ctx, pool, cust); st.Plan.Key != "team" {
		t.Fatalf("state after SetPlan = %+v", st)
	}
}
