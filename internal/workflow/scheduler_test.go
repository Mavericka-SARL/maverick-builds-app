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
	// Advanced past the tick that was due, and past the moment it fired.
	// Not "after now": with an every-minute cron the next tick can be the
	// very next second, which a slow run has already reached.
	if len(rules) != 1 || rules[0].NextFireAt == nil || !rules[0].NextFireAt.After(*rule.NextFireAt) ||
		(rules[0].LastFireAt != nil && !rules[0].NextFireAt.After(*rules[0].LastFireAt)) {
		t.Fatalf("expected next_fire_at advanced past the due tick, got %+v", rules[0])
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

	polledAt := time.Now()
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
	// Past the poll, not "after now": the next every-minute tick can be the
	// very next second, which a slow run has already reached.
	if rules[0].NextFireAt == nil || !rules[0].NextFireAt.After(polledAt) {
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

// NextScheduleAt is what a quiet tenant's Waker visits it for: the earliest
// enabled schedule rule's next fire time, and nothing once none is enabled.
func TestNextScheduleAt(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, appID := insertFixtures(t, store)
	ctx := context.Background()

	if next, err := store.NextScheduleAt(ctx); err != nil || !next.IsZero() {
		t.Fatalf("no rules: next = %v, err = %v; want the zero time", next, err)
	}
	rule := createDueScheduleRule(t, store, appID, userID, -90*time.Minute,
		workflow.ScheduleConfig{CronExpr: "* * * * *"})
	next, err := store.NextScheduleAt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := next.Sub(*rule.NextFireAt); d < -time.Millisecond || d > time.Millisecond {
		t.Fatalf("next = %v, want the rule's next_fire_at %v", next, *rule.NextFireAt)
	}
	if _, err := store.Pool().Exec(ctx, `UPDATE workflow.automation_rule SET enabled = false WHERE id = $1::uuid`, rule.ID); err != nil {
		t.Fatal(err)
	}
	if next, err := store.NextScheduleAt(ctx); err != nil || !next.IsZero() {
		t.Fatalf("rule disabled: next = %v, err = %v; want the zero time", next, err)
	}
}

// Only the live revision's schedule rules fire (decided 2026-10-06): a rule
// of a revision that is not live is disarmed and does not fire however due
// it was, and one of the live revision with no next fire (a revision copy's)
// is armed from now. Making another revision live swaps them.
func TestOnlyTheLiveRevisionsScheduleRulesFire(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)
	one := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := store.Pool().QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	modelID := one(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Plan') RETURNING id::text`, appID)
	live := one(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Live') RETURNING id::text`, modelID)
	next := one(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Next') RETURNING id::text`, modelID)
	one(`UPDATE core.model SET active_revision_id = $2::uuid WHERE id = $1::uuid RETURNING id::text`, modelID, live)

	// The live revision's rule, and its copy in the next revision as a
	// revision copy leaves it: unarmed — here backdated as if a save had
	// armed it, to prove it is disarmed rather than fired.
	liveRule := createDueScheduleRule(t, store, appID, userID, 2*time.Second, workflow.ScheduleConfig{CronExpr: "0 6 * * *"})
	one(`UPDATE workflow.automation_rule SET revision_id = $2::uuid WHERE id = $1::uuid RETURNING id::text`, liveRule.ID, live)
	nextRule := &workflow.AutomationRule{ID: one(`
		INSERT INTO workflow.automation_rule (application_id, revision_id, name, trigger_type, workflow_name, workflow_def_id,
		                                      enabled, cron_expr, timezone, misfire_policy, next_fire_at)
		SELECT application_id, $2::uuid, name, trigger_type, workflow_name, workflow_def_id,
		       enabled, cron_expr, timezone, misfire_policy, next_fire_at
		FROM workflow.automation_rule WHERE id = $1::uuid RETURNING id::text`, liveRule.ID, next)}
	armed := func(id string) bool {
		t.Helper()
		return one(`SELECT (next_fire_at IS NOT NULL)::text FROM workflow.automation_rule WHERE id = $1::uuid`, id) == "true"
	}
	poll := func() {
		t.Helper()
		if err := store.PollDueSchedules(ctx, zerolog.Nop(), "worker-1", 5*time.Second); err != nil {
			t.Fatalf("PollDueSchedules: %v", err)
		}
	}

	poll()
	if n := countExecutions(t, store, liveRule.ID); n != 1 {
		t.Errorf("the live revision's due rule fired %d times, want 1", n)
	}
	if n := countExecutions(t, store, nextRule.ID); n != 0 || armed(nextRule.ID) {
		t.Errorf("the next revision's rule fired %d times, armed=%v; want 0, disarmed", n, armed(nextRule.ID))
	}

	// Go-live of the next revision: its rule is armed for its next tick
	// (not fired for the past), the old one disarmed.
	one(`UPDATE core.model SET active_revision_id = $2::uuid WHERE id = $1::uuid RETURNING id::text`, modelID, next)
	poll()
	if !armed(nextRule.ID) || armed(liveRule.ID) {
		t.Errorf("after go-live: next armed=%v, old armed=%v; want true, false", armed(nextRule.ID), armed(liveRule.ID))
	}
	if n := countExecutions(t, store, nextRule.ID); n != 0 {
		t.Errorf("going live fired the next revision's rule %d times for a tick before it was live", n)
	}
	if got := one(`SELECT (next_fire_at > now())::text FROM workflow.automation_rule WHERE id = $1::uuid`, nextRule.ID); got != "true" {
		t.Errorf("the next revision's rule is armed for a past tick")
	}
}
