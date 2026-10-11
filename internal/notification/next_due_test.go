package notification

import (
	"context"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// NextRetryAt is what a quiet tenant's Waker visits it for: the next pending
// outbound delivery. In-app notifications are never delivered, so never
// count.
func TestNextRetryAt(t *testing.T) {
	ctx := context.Background()
	store, pool, userID := setupStore(t)

	if _, err := store.Notify(ctx, userID, "workflow_step_notification",
		map[string]string{"subject": "S", "message": "M"}, "workflow_instance", "wf-1"); err != nil {
		t.Fatal(err)
	}
	if next, err := store.NextRetryAt(ctx); err != nil || !next.IsZero() {
		t.Fatalf("in-app only: next = %v, err = %v; want the zero time", next, err)
	}

	if _, err := store.UpdateSettings(ctx, tenantOf(t, store, userID), Settings{EmailEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Notify(ctx, userID, "workflow_step_notification",
		map[string]string{"subject": "S", "message": "M"}, "workflow_instance", "wf-2"); err != nil {
		t.Fatal(err)
	}
	var want time.Time
	if err := pool.QueryRow(ctx, `
		UPDATE notification.notification SET next_attempt_at = now() + interval '10 minutes'
		WHERE channel = 'email' RETURNING next_attempt_at`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	next, err := store.NextRetryAt(ctx)
	if err != nil || !next.Equal(want) {
		t.Fatalf("pending e-mail: next = %v, err = %v; want %v", next, err, want)
	}

	if _, err := pool.Exec(ctx, `UPDATE notification.notification SET status = 'failed' WHERE channel = 'email'`); err != nil {
		t.Fatal(err)
	}
	if next, err := store.NextRetryAt(ctx); err != nil || !next.IsZero() {
		t.Fatalf("nothing pending: next = %v, err = %v; want the zero time", next, err)
	}
}

// Reminder.NextAt is a step's due time less the reminder lead, while that is
// still ahead. A step already past it (reminded, or left while reminders are
// off) and a test run's step never wake a quiet tenant.
func TestReminderNextAt(t *testing.T) {
	ctx := context.Background()

	f := setupReminder(t, "6 hours", false)
	if _, err := f.store.UpdateSettings(ctx, f.customer, Settings{RemindersEnabled: true, ReminderLeadHours: 2}); err != nil {
		t.Fatal(err)
	}
	r := &Reminder{Store: f.store, Log: logger.New("test")}
	var due time.Time
	if err := f.pool.QueryRow(ctx, `SELECT due_at FROM workflow.workflow_step WHERE id = $1::uuid`, f.stepID).Scan(&due); err != nil {
		t.Fatal(err)
	}
	next, err := r.NextAt(ctx)
	if err != nil || !next.Equal(due.Add(-2*time.Hour)) {
		t.Fatalf("next = %v, err = %v; want due less the 2h lead, %v", next, err, due.Add(-2*time.Hour))
	}

	overdue := setupReminder(t, "-2 hours", false)
	r = &Reminder{Store: overdue.store, Log: logger.New("test")}
	if next, err := r.NextAt(ctx); err != nil || !next.IsZero() {
		t.Fatalf("overdue step: next = %v, err = %v; want the zero time", next, err)
	}

	testRun := setupReminder(t, "6 hours", true)
	r = &Reminder{Store: testRun.store, Log: logger.New("test")}
	if next, err := r.NextAt(ctx); err != nil || !next.IsZero() {
		t.Fatalf("test run: next = %v, err = %v; want the zero time", next, err)
	}
}
