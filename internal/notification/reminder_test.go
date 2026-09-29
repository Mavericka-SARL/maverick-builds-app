package notification

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/internal/workflow/assignee/assigneetest"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// reminderFixture is one approval step, assigned to business_admin, that is
// already past its due time — the situation a reminder exists for.
type reminderFixture struct {
	store    *Store
	pool     *pgxpool.Pool
	customer string
	approver string // holds business_admin
	other    string // holds no role the step names
	stepID   string
	instance string
}

func setupReminder(t *testing.T, dueOffset string, testRun bool) reminderFixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	ex := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	one := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	customer := one(`INSERT INTO core.customer (name, plan) VALUES ('Reminders Inc', 'standard') RETURNING id::text`)
	ws := one(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Finance') RETURNING id::text`, customer)
	app := one(`INSERT INTO core.application (workspace_id, customer_id, name, mode)
	            VALUES ($1::uuid, $2::uuid, 'Planning', 'planning') RETURNING id::text`, ws, customer)

	// Business roles are held in a workspace: an unscoped one counts
	// nowhere (TestReminderKeepsTheWorkspaceBoundary).
	approver := one(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
	                 VALUES ('rem-approver', 'ann@example.test', 'Ann Approver', $1::uuid) RETURNING id::text`, customer)
	other := one(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
	              VALUES ('rem-other', 'bob@example.test', 'Bob Bystander', $1::uuid) RETURNING id::text`, customer)
	ex(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_admin', $2::uuid)`, approver, ws)
	ex(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, other, ws)
	def := one(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps)
	            VALUES ($1::uuid, 'Budget Approval', 'manual',
	                    '[{"id":"s1","name":"Finance Review","type":"approval","assignee_roles":["business_admin"]}]'::jsonb)
	            RETURNING id::text`, app)
	instance := one(`INSERT INTO workflow.workflow_instance (workflow_def_id, started_by, test_run)
	                 VALUES ($1::uuid, $2::uuid, $3) RETURNING id::text`, def, other, testRun)
	step := one(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status, due_at)
	             VALUES ($1::uuid, 's1', 'in_progress', now() + $2::interval) RETURNING id::text`, instance, dueOffset)

	return reminderFixture{store: NewStore(pool), pool: pool, customer: customer, approver: approver, other: other, stepID: step, instance: instance}
}

func (f reminderFixture) reminders(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT recipient_user_id::text FROM notification.notification WHERE template_id=$1 AND channel='in_app'`, ReminderTemplate)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func TestRemindsAssigneesOfAnOverdueTaskOnce(t *testing.T) {
	ctx := context.Background()
	f := setupReminder(t, "-2 hours", false)
	if _, err := f.store.UpdateSettings(ctx, f.customer, Settings{RemindersEnabled: true}); err != nil {
		t.Fatal(err)
	}
	r := &Reminder{Store: f.store, Log: logger.New("test")}

	n, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reminded about %d steps, want 1", n)
	}
	got := f.reminders(t)
	if len(got) != 1 || got[0] != f.approver {
		t.Fatalf("reminder went to %v; only the assignee role should be reminded", got)
	}

	var subject, message string
	if err := f.pool.QueryRow(ctx, `
		SELECT template_vars->>'subject', template_vars->>'message'
		FROM notification.notification WHERE template_id=$1`, ReminderTemplate).Scan(&subject, &message); err != nil {
		t.Fatal(err)
	}
	if subject != "Reminder: Finance Review" || message == "" {
		t.Fatalf("reminder text = %q / %q", subject, message)
	}

	// A step that stays overdue must not be reminded about again.
	if n, err := r.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second pass reminded %d times (%v)", n, err)
	}
	if len(f.reminders(t)) != 1 {
		t.Fatalf("a second reminder was sent: %v", f.reminders(t))
	}
}

func TestReminderRespectsSettingsDueTimeAndTestRuns(t *testing.T) {
	ctx := context.Background()

	t.Run("off by default", func(t *testing.T) {
		f := setupReminder(t, "-2 hours", false)
		r := &Reminder{Store: f.store, Log: logger.New("test")}
		if n, err := r.RunOnce(ctx); err != nil || n != 0 {
			t.Fatalf("reminders fired while turned off (%d, %v)", n, err)
		}
	})

	t.Run("not yet due", func(t *testing.T) {
		f := setupReminder(t, "6 hours", false)
		if _, err := f.store.UpdateSettings(ctx, f.customer, Settings{RemindersEnabled: true}); err != nil {
			t.Fatal(err)
		}
		r := &Reminder{Store: f.store, Log: logger.New("test")}
		if n, err := r.RunOnce(ctx); err != nil || n != 0 {
			t.Fatalf("a task due in six hours was reminded (%d, %v)", n, err)
		}
		// With a lead time that covers it, the same task is reminded.
		if _, err := f.store.UpdateSettings(ctx, f.customer, Settings{RemindersEnabled: true, ReminderLeadHours: 8}); err != nil {
			t.Fatal(err)
		}
		if n, err := r.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("the lead time did not bring the task forward (%d, %v)", n, err)
		}
	})

	t.Run("test runs never page anyone", func(t *testing.T) {
		f := setupReminder(t, "-2 hours", true)
		if _, err := f.store.UpdateSettings(ctx, f.customer, Settings{RemindersEnabled: true}); err != nil {
			t.Fatal(err)
		}
		r := &Reminder{Store: f.store, Log: logger.New("test")}
		if n, err := r.RunOnce(ctx); err != nil || n != 0 {
			t.Fatalf("a test run sent reminders (%d, %v)", n, err)
		}
	})

	t.Run("a completed step is not reminded", func(t *testing.T) {
		f := setupReminder(t, "-2 hours", false)
		if _, err := f.store.UpdateSettings(ctx, f.customer, Settings{RemindersEnabled: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE workflow.workflow_step SET status='completed' WHERE id=$1::uuid`, f.stepID); err != nil {
			t.Fatal(err)
		}
		r := &Reminder{Store: f.store, Log: logger.New("test")}
		if n, err := r.RunOnce(ctx); err != nil || n != 0 {
			t.Fatalf("a completed task was reminded (%d, %v)", n, err)
		}
	})
}

// A reminder goes to the step's assignees inside the workspace boundary of
// workflow assignment (package assignee), the people who see the task. It
// used to go to every holder of a named platform role in every tenant, and to
// a named business role's members in every workspace of the tenant — with the
// workflow's and the step's names. A step naming no role reminds nobody.
func TestReminderKeepsTheWorkspaceBoundary(t *testing.T) {
	ctx := context.Background()
	f := assigneetest.New(t)
	store := NewStore(f.Pool)
	for _, cust := range []string{f.Cust1, f.Cust2} {
		if _, err := store.UpdateSettings(ctx, cust, Settings{RemindersEnabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reminder{Store: store, Log: logger.New("test")}
	for _, c := range assigneetest.Cases {
		t.Run(c.Name, func(t *testing.T) {
			step := f.Step(t, c.App(f), c.Roles...)
			if n, err := r.RunOnce(ctx); err != nil || n != 1 {
				t.Fatalf("reminded about %d steps (%v), want 1", n, err)
			}
			rows, err := f.Pool.Query(ctx, `
				SELECT DISTINCT n.recipient_user_id::text
				FROM notification.notification n
				JOIN workflow.workflow_step ws ON ws.instance_id::text = n.resource_id
				WHERE n.template_id = $1 AND ws.id = $2::uuid`, ReminderTemplate, step)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				got = append(got, id)
			}
			rows.Close()
			want := c.Want
			if len(c.Roles) == 0 {
				want = []string{}
			}
			if names := f.Names(got); !slices.Equal(names, want) {
				t.Errorf("reminded %v, want %v", names, want)
			}
		})
	}
}

// A reminder is a notification like any other, so an enabled channel carries
// it out of the platform too.
func TestReminderGoesOutOnEnabledChannels(t *testing.T) {
	ctx := context.Background()
	f := setupReminder(t, "-1 hour", false)
	if _, err := f.store.UpdateSettings(ctx, f.customer, Settings{RemindersEnabled: true, EmailEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Reminder{Store: f.store, Log: logger.New("test")}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	mailer := &fakeMailer{}
	if _, err := (&Dispatcher{Store: f.store, Mailer: mailer, Log: logger.New("test")}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sent := mailer.all()
	if len(sent) != 1 || sent[0].To != "ann@example.test" || sent[0].Subject != "Reminder: Finance Review" {
		t.Fatalf("the reminder did not reach the assignee by e-mail: %+v", sent)
	}
}
