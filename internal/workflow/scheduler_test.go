// Tests for the scheduled (cron) automation trigger: internal/workflow/
// scheduler.go's PollDueSchedules/TriggerScheduledRule, and the schedule
// config fields on AutomationRule. Uses the same testcontainers harness as
// store_test.go (setupDB/insertFixtures, same workflow_test package).
package workflow_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// createDueScheduleRule creates a published workflow def plus a
// schedule-triggered automation rule wired to it, then force-backdates the
// rule's next_fire_at directly (CreateAutomationRule always computes a
// future one) so it is immediately due for a test's PollDueSchedules call.
func createDueScheduleRule(t *testing.T, store *workflow.Store, appID, userID string, dueBy time.Duration, sched workflow.ScheduleConfig) *workflow.AutomationRule {
	t.Helper()
	ctx := context.Background()

	steps := []*workflowv1.WorkflowStepDef{
		{Id: "only-step", Name: "Solo", Type: workflowv1.StepType_STEP_TYPE_TASK},
	}
	def, err := store.CreateWorkflowDef(ctx, appID, "Scheduled Flow", "manual", steps)
	if err != nil {
		t.Fatalf("CreateWorkflowDef: %v", err)
	}
	if _, err := store.PublishWorkflowDef(ctx, def.Id, userID); err != nil {
		t.Fatalf("PublishWorkflowDef: %v", err)
	}

	rule, err := store.CreateAutomationRule(ctx, appID, "", "sched-rule", "", "schedule", "Scheduled Flow", def.Id, "", "", &sched)
	if err != nil {
		t.Fatalf("CreateAutomationRule: %v", err)
	}
	if rule.NextFireAt == nil {
		t.Fatal("expected next_fire_at to be computed for a schedule rule")
	}

	due := time.Now().Add(-dueBy)
	if _, err := store.Pool().Exec(ctx,
		`UPDATE workflow.automation_rule SET next_fire_at = $2 WHERE id = $1::uuid`,
		rule.ID, due,
	); err != nil {
		t.Fatalf("backdate next_fire_at: %v", err)
	}
	rule.NextFireAt = &due
	return rule
}

func countExecutions(t *testing.T, store *workflow.Store, ruleID string) int {
	t.Helper()
	var n int
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT COUNT(*) FROM workflow.execution WHERE rule_id = $1::uuid`, ruleID,
	).Scan(&n); err != nil {
		t.Fatalf("count executions: %v", err)
	}
	return n
}

func TestCreateScheduleRuleComputesNextFireAt(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	_, appID := insertFixtures(t, store)

	before := time.Now()
	rule, err := store.CreateAutomationRule(ctx, appID, "", "every-5-min", "", "schedule", "Some Flow", "", "", "",
		&workflow.ScheduleConfig{CronExpr: "*/5 * * * *", Timezone: "UTC"})
	if err != nil {
		t.Fatalf("CreateAutomationRule: %v", err)
	}
	if rule.CronExpr != "*/5 * * * *" {
		t.Errorf("cron_expr = %q", rule.CronExpr)
	}
	if rule.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC (explicit)", rule.Timezone)
	}
	if rule.MisfirePolicy != "skip" {
		t.Errorf("misfire_policy = %q, want default skip", rule.MisfirePolicy)
	}
	if rule.NextFireAt == nil || !rule.NextFireAt.After(before) {
		t.Fatalf("next_fire_at = %v, want a time after %v", rule.NextFireAt, before)
	}
}

func TestCreateScheduleRuleRequiresCronExpr(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	_, appID := insertFixtures(t, store)

	if _, err := store.CreateAutomationRule(ctx, appID, "", "no-cron", "", "schedule", "Some Flow", "", "", "", nil); err == nil {
		t.Fatal("expected an error creating a schedule rule with no ScheduleConfig")
	}
}

func TestPollDueSchedulesFiresDueRuleOnce(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, appID := insertFixtures(t, store)

	// Due 2s ago with a 5s misfire threshold: a normal, on-time fire — must
	// always fire regardless of misfire_policy (defaulted to "skip" here),
	// which only governs ticks found *beyond* the threshold.
	rule := createDueScheduleRule(t, store, appID, userID, 2*time.Second,
		workflow.ScheduleConfig{CronExpr: "* * * * *"})

	if err := store.PollDueSchedules(context.Background(), zerolog.Nop(), "worker-1", 5*time.Second); err != nil {
		t.Fatalf("PollDueSchedules: %v", err)
	}

	if n := countExecutions(t, store, rule.ID); n != 1 {
		t.Fatalf("execution count = %d, want 1", n)
	}

	rules, err := store.ListAutomationRules(context.Background(), appID, "")
	if err != nil {
		t.Fatalf("ListAutomationRules: %v", err)
	}
	if len(rules) != 1 || rules[0].NextFireAt == nil || !rules[0].NextFireAt.After(time.Now()) {
		t.Fatalf("expected next_fire_at advanced into the future, got %+v", rules[0])
	}
	if rules[0].LastFireAt == nil {
		t.Fatal("expected last_fire_at to be set after firing")
	}

	// A second poll immediately after must not re-fire — next_fire_at is
	// now in the future, so the rule is no longer due.
	if err := store.PollDueSchedules(context.Background(), zerolog.Nop(), "worker-1", 5*time.Second); err != nil {
		t.Fatalf("second PollDueSchedules: %v", err)
	}
	if n := countExecutions(t, store, rule.ID); n != 1 {
		t.Fatalf("execution count after second poll = %d, want still 1", n)
	}
}

func TestPollDueSchedulesSkipsDisabledRule(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, appID := insertFixtures(t, store)

	rule := createDueScheduleRule(t, store, appID, userID, time.Minute,
		workflow.ScheduleConfig{CronExpr: "* * * * *"})

	disabled := false
	if _, err := store.UpdateAutomationRule(context.Background(), rule.ID, "", "", "", "", "", "", "", &disabled, nil); err != nil {
		t.Fatalf("UpdateAutomationRule (disable): %v", err)
	}

	if err := store.PollDueSchedules(context.Background(), zerolog.Nop(), "worker-1", 5*time.Second); err != nil {
		t.Fatalf("PollDueSchedules: %v", err)
	}
	if n := countExecutions(t, store, rule.ID); n != 0 {
		t.Fatalf("execution count = %d, want 0 for a disabled rule", n)
	}
}

func TestMisfirePolicySkipDoesNotFireButAdvances(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, appID := insertFixtures(t, store)

	// Due an hour ago with a 1-second misfire threshold: this is a clear
	// misfire, and misfire_policy=skip (the default) must not fire it.
	rule := createDueScheduleRule(t, store, appID, userID, time.Hour,
		workflow.ScheduleConfig{CronExpr: "* * * * *", MisfirePolicy: "skip"})

	if err := store.PollDueSchedules(context.Background(), zerolog.Nop(), "worker-1", time.Second); err != nil {
		t.Fatalf("PollDueSchedules: %v", err)
	}
	if n := countExecutions(t, store, rule.ID); n != 0 {
		t.Fatalf("execution count = %d, want 0 (skip policy must not fire a misfire)", n)
	}

	rules, err := store.ListAutomationRules(context.Background(), appID, "")
	if err != nil {
		t.Fatalf("ListAutomationRules: %v", err)
	}
	if rules[0].NextFireAt == nil || !rules[0].NextFireAt.After(time.Now()) {
		t.Fatalf("expected next_fire_at fast-forwarded past the missed tick, got %+v", rules[0].NextFireAt)
	}
}

func TestMisfirePolicyFireNowFiresOnce(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, appID := insertFixtures(t, store)

	rule := createDueScheduleRule(t, store, appID, userID, time.Hour,
		workflow.ScheduleConfig{CronExpr: "* * * * *", MisfirePolicy: "fire_now"})

	if err := store.PollDueSchedules(context.Background(), zerolog.Nop(), "worker-1", time.Second); err != nil {
		t.Fatalf("PollDueSchedules: %v", err)
	}
	if n := countExecutions(t, store, rule.ID); n != 1 {
		t.Fatalf("execution count = %d, want 1 (fire_now must fire once for the missed tick)", n)
	}
}

// TestTriggerScheduledRuleClaimIsIdempotent directly proves the core
// idempotency mechanism (the partial unique index on
// workflow.execution(rule_id, scheduled_for)): firing the exact same tick
// twice must succeed exactly once.
func TestTriggerScheduledRuleClaimIsIdempotent(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, appID := insertFixtures(t, store)

	rule := createDueScheduleRule(t, store, appID, userID, time.Minute,
		workflow.ScheduleConfig{CronExpr: "* * * * *"})
	scheduledFor := *rule.NextFireAt

	if _, err := store.TriggerScheduledRule(context.Background(), *rule, scheduledFor, "worker-a"); err != nil {
		t.Fatalf("first TriggerScheduledRule: %v", err)
	}
	_, err := store.TriggerScheduledRule(context.Background(), *rule, scheduledFor, "worker-b")
	if err != workflow.ErrAlreadyClaimed {
		t.Fatalf("second TriggerScheduledRule error = %v, want ErrAlreadyClaimed", err)
	}
	if n := countExecutions(t, store, rule.ID); n != 1 {
		t.Fatalf("execution count = %d, want exactly 1 despite two claim attempts", n)
	}
}

// TestPollDueSchedulesConcurrentInstancesFireOnce simulates two gateway
// replicas polling the same due rule at the same time — the realistic
// multi-instance scenario FOR UPDATE SKIP LOCKED (plus the unique-index
// claim in TriggerScheduledRule) is meant to guard.
func TestPollDueSchedulesConcurrentInstancesFireOnce(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, appID := insertFixtures(t, store)

	// Due 2s ago with a 5s misfire threshold: a normal, on-time fire (see
	// TestPollDueSchedulesFiresDueRuleOnce for why this must be within the
	// threshold, not beyond it).
	rule := createDueScheduleRule(t, store, appID, userID, 2*time.Second,
		workflow.ScheduleConfig{CronExpr: "* * * * *"})

	var wg sync.WaitGroup
	for _, workerID := range []string{"worker-a", "worker-b"} {
		wg.Add(1)
		go func(workerID string) {
			defer wg.Done()
			_ = store.PollDueSchedules(context.Background(), zerolog.Nop(), workerID, 5*time.Second)
		}(workerID)
	}
	wg.Wait()

	if n := countExecutions(t, store, rule.ID); n != 1 {
		t.Fatalf("execution count = %d, want exactly 1 across two concurrent pollers", n)
	}
}

// TestTriggerScheduledRuleRetriesThenFails points a rule at a workflow name
// that will never resolve, forcing every attempt to fail, and checks the
// bounded retry loop still lands on a single 'failed' execution row rather
// than looping forever or leaving nothing behind.
func TestTriggerScheduledRuleRetriesThenFails(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	_, appID := insertFixtures(t, store)

	created, err := store.CreateAutomationRule(context.Background(), appID, "", "doomed-rule", "", "schedule", "Does Not Exist", "", "", "",
		&workflow.ScheduleConfig{CronExpr: "* * * * *", MaxRetries: 2, RetryBackoffSeconds: 0})
	if err != nil {
		t.Fatalf("CreateAutomationRule: %v", err)
	}
	rule := *created

	_, err = store.TriggerScheduledRule(context.Background(), rule, time.Now(), "worker-1")
	if err == nil {
		t.Fatal("expected an error since the workflow can never resolve")
	}
	if err == workflow.ErrAlreadyClaimed {
		t.Fatal("did not expect ErrAlreadyClaimed on a fresh tick")
	}

	if n := countExecutions(t, store, rule.ID); n != 1 {
		t.Fatalf("execution count = %d, want exactly 1 (claimed once, retried in place, then failed)", n)
	}
	var status, execErr string
	if scanErr := store.Pool().QueryRow(context.Background(),
		`SELECT status::text, COALESCE(error,'') FROM workflow.execution WHERE rule_id = $1::uuid`, rule.ID,
	).Scan(&status, &execErr); scanErr != nil {
		t.Fatalf("scan execution: %v", scanErr)
	}
	if status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
	if execErr == "" {
		t.Error("expected a non-empty error message on the failed execution")
	}
}
