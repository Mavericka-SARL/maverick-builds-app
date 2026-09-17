package plan

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// Evaluate is pure: the state a tenant is in follows from its row, its plan
// and the clock, so every branch is checked against a fixed clock.
func TestEvaluate(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	future := now.Add(36 * time.Hour)
	past := now.Add(-time.Hour)
	p := Plan{Key: "trial", Name: "Trial", TrialDays: 14}

	t.Run("no trial, within limits", func(t *testing.T) {
		st := Evaluate(p, true, Tenant{LimitState: "ok"}, now)
		if st.Trial || st.ReadOnly || st.Code != "" || st.DaysLeft != 0 {
			t.Fatalf("state = %+v", st)
		}
	})
	t.Run("trial with a day and a half left counts two days", func(t *testing.T) {
		st := Evaluate(p, true, Tenant{TrialEndsAt: &future, LimitState: "ok"}, now)
		if !st.Trial || st.ReadOnly || st.DaysLeft != 2 {
			t.Fatalf("state = %+v", st)
		}
	})
	t.Run("expired trial is read-only and says when it ended", func(t *testing.T) {
		st := Evaluate(p, true, Tenant{TrialEndsAt: &past, LimitState: "ok"}, now)
		if !st.ReadOnly || st.Code != CodeTrialExpired || st.DaysLeft != 0 || !strings.Contains(st.Reason, "17 September 2026") {
			t.Fatalf("state = %+v", st)
		}
	})
	t.Run("over a limit is read-only with the sweep's reason", func(t *testing.T) {
		st := Evaluate(p, true, Tenant{LimitState: "over", LimitReason: "too many models"}, now)
		if !st.ReadOnly || st.Code != CodeOverLimit || !strings.Contains(st.Reason, "too many models") {
			t.Fatalf("state = %+v", st)
		}
	})
	t.Run("an expired trial outranks a limit", func(t *testing.T) {
		st := Evaluate(p, true, Tenant{TrialEndsAt: &past, LimitState: "over", LimitReason: "x"}, now)
		if st.Code != CodeTrialExpired {
			t.Fatalf("code = %q", st.Code)
		}
	})
}

func TestValidate(t *testing.T) {
	good := Plan{Key: "team-10", Name: "Team", TrialDays: 30}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Plan{
		{Key: "Bad Key", Name: "x"},
		{Key: "ok", Name: ""},
		{Key: "ok", Name: "x", TrialDays: 400},
		{Key: "ok", Name: "x", Limits: Limits{MaxUsers: -1}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("plan %+v validated", bad)
		}
	}
}

// The catalog, the request-time checks and the sweep against a real
// database: the seeded plans, a tenant on the trial plan meeting each
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
	var trial Plan
	for _, p := range plans {
		if p.Key == "trial" {
			trial = p
		}
	}
	if trial.Key == "" || !trial.SelfService || trial.TrialDays != 14 || trial.Limits.MaxModels != 3 {
		t.Fatalf("seeded trial plan = %+v", trial)
	}
	if ss, ok, err := SelfService(ctx, pool); err != nil || !ok || ss.Key != "trial" {
		t.Fatalf("self-service plan = %+v ok=%v err=%v", ss, ok, err)
	}
	if _, err := Get(ctx, pool, "nope"); !errors.Is(err, ErrUnknownPlan) {
		t.Fatalf("unknown plan err = %v", err)
	}
	saved, err := Upsert(ctx, pool, Plan{Key: "team", Name: " Team ", TrialDays: 0, Limits: Limits{MaxUsers: 10}, SortOrder: 25})
	if err != nil || saved.Name != "Team" || saved.Limits.MaxUsers != 10 {
		t.Fatalf("upsert = %+v err=%v", saved, err)
	}

	// A tenant on the trial plan with two users, one app and one model.
	cust := q(`INSERT INTO core.customer (name, plan, trial_ends_at) VALUES ('Trialist', 'trial', now() + interval '10 days') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, cust)
	app := q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, cust, ws)
	model := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, app)
	rev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, model)
	q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('plan-u1', 'u1@trial.test', 'U1', $1::uuid) RETURNING id::text`, cust)
	u2 := q(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('plan-u2', 'u2@trial.test', 'U2') RETURNING id::text`)
	q(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid) RETURNING id::text`, u2, ws)

	e := NewEnforcer(pool)
	st, err := e.State(ctx, pool, cust)
	if err != nil || !st.Trial || st.DaysLeft != 10 || st.ReadOnly || !st.PlanKnown {
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
	if err := e.CheckApplications(ctx, pool, cust, 2); err == nil || !IsLimit(err) || !strings.Contains(err.Error(), "Trial plan allows 2 applications; this tenant has 1") {
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

	// SetPlan moves the tenant and the cache follows an Invalidate.
	if err := SetPlan(ctx, pool, cust, "team", nil); err != nil {
		t.Fatal(err)
	}
	e.Invalidate(cust)
	if st, _ := e.State(ctx, pool, cust); st.Plan.Key != "team" || st.Trial {
		t.Fatalf("state after SetPlan = %+v", st)
	}
}
