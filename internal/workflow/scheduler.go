package workflow

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// ensureSystemUser returns the ID of a real identity.user row to attribute
// scheduler-started workflow instances to — workflow_instance.started_by is
// a NOT NULL foreign key, so a bare sentinel UUID with no backing row would
// fail that constraint. Upserted idempotently by keycloak_sub, so every
// call after the first just returns the same existing row.
func (s *Store) ensureSystemUser(ctx context.Context) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name)
		VALUES ('system-scheduler', 'scheduler@system.internal', 'Automation Scheduler')
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text
	`).Scan(&id)
	return id, err
}

// dueRule is one automation_rule row claimed as due by claimDueSchedules.
// fire is false when misfire_policy=skip decided this tick should be
// fast-forwarded past rather than actually fired.
type dueRule struct {
	rule         AutomationRule
	scheduledFor time.Time
	fire         bool
}

// RunScheduler polls for due schedule-triggered automation rules every
// pollInterval until ctx is cancelled. workerID identifies this process
// instance for the execution-ownership column (workflow.execution.
// claimed_by) — safe to run from every replica of the gateway
// simultaneously (see PollDueSchedules). misfireThreshold is how overdue a
// tick has to be before a rule's misfire_policy applies at all; a tick
// found only slightly late (within one normal poll interval) always fires
// regardless of policy — misfire_policy only governs genuine backlog, e.g.
// after the process was down.
func RunScheduler(ctx context.Context, store *Store, log zerolog.Logger, pollInterval, misfireThreshold time.Duration, workerID string) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := store.PollDueSchedules(ctx, log, workerID, misfireThreshold); err != nil {
				log.Warn().Err(err).Msg("scheduled automation poll failed")
			}
		}
	}
}

// PollDueSchedules is one poll pass: claim every enabled schedule-type rule
// whose next_fire_at has passed (advancing each past any backlog per its
// misfire_policy), then fire the ones that need firing through
// TriggerScheduledRule. Safe to call concurrently from multiple gateway
// instances — FOR UPDATE SKIP LOCKED here, plus workflow.execution's
// partial unique index in TriggerScheduledRule, make firing idempotent
// even if two instances both wake up for the same due tick.
func (s *Store) PollDueSchedules(ctx context.Context, log zerolog.Logger, workerID string, misfireThreshold time.Duration) error {
	due, err := s.claimDueSchedules(ctx, log, misfireThreshold)
	if err != nil {
		return err
	}
	for _, d := range due {
		if !d.fire {
			s.auditScheduleEvent(ctx, d.rule, auditlog.EventAutomationRuleScheduledMisfireSkipped, workerID, "")
			continue
		}
		exec, err := s.TriggerScheduledRule(ctx, d.rule, d.scheduledFor, workerID)
		if errors.Is(err, ErrAlreadyClaimed) {
			continue
		}
		if err != nil {
			s.auditScheduleEvent(ctx, d.rule, auditlog.EventAutomationRuleScheduledFireFailed, workerID, err.Error())
			log.Warn().Err(err).Str("rule_id", d.rule.ID).Msg("scheduled automation fire failed")
			continue
		}
		s.auditScheduleEvent(ctx, d.rule, auditlog.EventAutomationRuleScheduledFire, workerID, exec.ID)
	}
	return nil
}

// claimDueSchedules selects due schedule-type rules with FOR UPDATE SKIP
// LOCKED — so two concurrently-polling instances never even attempt the
// same rule row in the same instant — advances each one's next_fire_at/
// last_fire_at past its due tick (and any further backlog) in the same
// short transaction, then commits, releasing the row lock quickly rather
// than holding it for the duration of actually firing.
func (s *Store) claimDueSchedules(ctx context.Context, log zerolog.Logger, misfireThreshold time.Duration) ([]dueRule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id::text, application_id::text, COALESCE(revision_id::text,''), name, workflow_name,
		       COALESCE(workflow_def_id::text,''), enabled,
		       cron_expr, timezone, misfire_policy, max_retries, retry_backoff_seconds, next_fire_at
		FROM workflow.automation_rule
		WHERE enabled AND trigger_type = 'schedule' AND next_fire_at <= now()
		FOR UPDATE SKIP LOCKED
	`)
	if err != nil {
		return nil, err
	}
	type rawDue struct {
		rule    AutomationRule
		dueTick time.Time
	}
	var raw []rawDue
	for rows.Next() {
		var r AutomationRule
		var dueTick time.Time
		if err := rows.Scan(&r.ID, &r.ApplicationID, &r.RevisionID, &r.Name, &r.WorkflowName,
			&r.WorkflowDefID, &r.Enabled, &r.CronExpr, &r.Timezone, &r.MisfirePolicy,
			&r.MaxRetries, &r.RetryBackoffSeconds, &dueTick); err != nil {
			rows.Close()
			return nil, err
		}
		raw = append(raw, rawDue{rule: r, dueTick: dueTick})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	now := time.Now()
	due := make([]dueRule, 0, len(raw))
	for _, rd := range raw {
		r, dueTick := rd.rule, rd.dueTick

		// Fast-forward past this tick and any further backlog to the first
		// tick still in the future — this happens regardless of
		// misfire_policy; the policy only decides whether we ALSO fire for
		// a tick found this late.
		newNext, ferr := nextFireTime(r.CronExpr, r.Timezone, dueTick)
		if ferr != nil {
			log.Warn().Err(ferr).Str("rule_id", r.ID).Msg("invalid schedule config; leaving next_fire_at unchanged")
			continue
		}
		for !newNext.After(now) {
			n, nerr := nextFireTime(r.CronExpr, r.Timezone, newNext)
			if nerr != nil {
				break
			}
			newNext = n
		}

		if _, err := tx.Exec(ctx, `
			UPDATE workflow.automation_rule SET next_fire_at = $2, last_fire_at = $3 WHERE id = $1::uuid
		`, r.ID, newNext, now); err != nil {
			return nil, err
		}

		fire := true
		if now.Sub(dueTick) > misfireThreshold && r.MisfirePolicy == "skip" {
			fire = false
		}
		due = append(due, dueRule{rule: r, scheduledFor: dueTick, fire: fire})
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return due, nil
}

// auditScheduleEvent records a scheduler-originated event via pkg/auditlog.
// internal/workflow has no HTTP-request-scoped actor to attribute this to
// (unlike internal/gateway/handler.go's call sites) since the scheduler runs
// with no request in flight; detail carries the execution ID (fire/skip) or
// error message (failure) as metadata.
func (s *Store) auditScheduleEvent(ctx context.Context, rule AutomationRule, eventType auditlog.EventType, workerID, detail string) {
	auditlog.Log(ctx, s.pool, s.log, auditlog.Fields{
		Category:      auditlog.CategoryDataChange,
		EventType:     eventType,
		ApplicationID: rule.ApplicationID,
		ResourceType:  "automation_rule",
		ResourceID:    rule.ID,
		RevisionID:    rule.RevisionID,
		Metadata: map[string]string{
			"worker_id": workerID,
			"rule_name": rule.Name,
			"detail":    detail,
		},
	})
}
