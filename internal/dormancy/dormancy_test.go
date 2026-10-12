package dormancy_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/dormancy"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

type mail struct{ to, subject, body string }

type mailbox struct {
	mu   sync.Mutex
	sent []mail
}

func (m *mailbox) Send(_ context.Context, to, _, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, mail{to, subject, body})
	return nil
}

func (m *mailbox) all() []mail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mail(nil), m.sent...)
}

type fixture struct {
	control *pgxpool.Pool
	router  *tenantdb.Router
	box     *mailbox
	deleted []string
	now     time.Time
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	control := testdb.New(t, migrationfs.FS, ".")
	r := tenantdb.New(control, tenantdb.Config{
		Migrations: migrationfs.FS, MigrationsDir: ".", AdminURL: testdb.AdminDSN(t), IdleTimeout: -1,
	})
	t.Cleanup(r.Close)
	// The free plan retires unused workspaces: told at 30 days, deleted at 45.
	if _, err := control.Exec(ctx, `UPDATE platform.plan SET dormant_warn_days = 30, dormant_delete_days = 45 WHERE key = 'community'`); err != nil {
		t.Fatal(err)
	}
	return &fixture{control: control, router: r, box: &mailbox{}, now: time.Now()}
}

func (f *fixture) deps() dormancy.Deps {
	return dormancy.Deps{
		Control: f.control, Router: f.router, Mailer: f.box,
		Delete: func(ctx context.Context, id string) error {
			f.deleted = append(f.deleted, id)
			return f.router.Deprovision(ctx, id)
		},
		ConsoleURL: "https://app.example.test", ContactEmail: "legal@example.test", BackupRetentionDays: 14,
		Log: zerolog.Nop(), Now: func() time.Time { return f.now },
	}
}

// tenant provisions a workspace on plan, last used quietDays before f.now,
// with one administrator who has an address.
func (f *fixture) tenant(t *testing.T, name, plan string, quietDays int) string {
	t.Helper()
	ctx := context.Background()
	p, err := f.router.Provision(ctx, name, plan)
	if err != nil {
		t.Fatal(err)
	}
	id := p.Tenant.CustomerID
	pool, err := f.router.Pool(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var user string
	if err := pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ($1, $2, 'Ada Admin', $3::uuid) RETURNING id::text`,
		uuid.NewString(), strings.ToLower(name)+"-admin@example.test", id).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'tenant_admin')`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.Exec(ctx, `UPDATE platform.tenant_database SET last_used_at = $2 WHERE customer_id = $1::uuid`,
		id, f.now.AddDate(0, 0, -quietDays)); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) sweep(t *testing.T) dormancy.Result {
	t.Helper()
	r, err := dormancy.Sweep(context.Background(), f.deps())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A workspace quiet past its plan's warning period is told once; one on a
// plan that keeps workspaces, or not yet past it, is left alone.
func TestWarnsOnceAndOnlyPastThePlansPeriod(t *testing.T) {
	f := setup(t)
	quiet := f.tenant(t, "Quiet", "community", 31)
	f.tenant(t, "Recent", "community", 10)
	f.tenant(t, "Kept", "enterprise", 400)

	if r := f.sweep(t); r.Warned != 1 || r.Deleted != 0 {
		t.Fatalf("first sweep = %+v, want 1 warned", r)
	}
	sent := f.box.all()
	if len(sent) != 1 || sent[0].to != "quiet-admin@example.test" {
		t.Fatalf("mails = %+v, want one to Quiet's administrator", sent)
	}
	// Told 31 days after last use; deleted at 45, so in 14 days — no less
	// than the 15 days' notice the periods give, which wins.
	want := f.now.AddDate(0, 0, 15).UTC().Format("2 January 2006")
	if !strings.Contains(sent[0].subject, want) || !strings.Contains(sent[0].body, "https://app.example.test") ||
		!strings.Contains(sent[0].body, "legal@example.test") {
		t.Fatalf("warning mail = %q / %q, want deletion on %s, the sign-in link and where to ask for a restore", sent[0].subject, sent[0].body, want)
	}
	var warned bool
	if err := f.control.QueryRow(context.Background(), `SELECT dormancy_warned_at IS NOT NULL FROM platform.tenant_database WHERE customer_id = $1::uuid`, quiet).Scan(&warned); err != nil || !warned {
		t.Fatalf("notice not recorded (%v)", err)
	}

	f.now = f.now.Add(time.Hour)
	if r := f.sweep(t); r.Warned != 0 || len(f.box.all()) != 1 {
		t.Fatalf("second sweep = %+v with %d mails, want nothing new", r, len(f.box.all()))
	}
}

// Deletion needs both the plan's deletion period without use and the full
// notice since the warning; then the administrators are told it is gone.
func TestDeletesOnlyAfterTheFullNotice(t *testing.T) {
	f := setup(t)
	id := f.tenant(t, "Gone", "community", 60) // long past both periods, never told
	if r := f.sweep(t); r.Warned != 1 || r.Deleted != 0 {
		t.Fatalf("sweep at notice = %+v, want told and not deleted", r)
	}
	f.now = f.now.AddDate(0, 0, 14)
	if r := f.sweep(t); r.Deleted != 0 {
		t.Fatalf("deleted after 14 days' notice, want 15 (%+v)", r)
	}
	f.now = f.now.AddDate(0, 0, 1).Add(time.Minute)
	if r := f.sweep(t); r.Deleted != 1 || len(f.deleted) != 1 || f.deleted[0] != id {
		t.Fatalf("sweep after the notice = %+v, deleted %v", r, f.deleted)
	}
	sent := f.box.all()
	if last := sent[len(sent)-1]; !strings.Contains(last.subject, "was deleted") || !strings.Contains(last.body, "14 days") {
		t.Fatalf("last mail = %+v, want the deletion notice with the restore window", last)
	}
	if _, err := f.router.Catalog().Get(context.Background(), id); err == nil {
		t.Fatal("the deleted tenant is still in the catalog")
	}
}

// Use after the warning withdraws it: the workspace stays.
func TestUseWithdrawsTheNotice(t *testing.T) {
	f := setup(t)
	id := f.tenant(t, "Back", "community", 40)
	if r := f.sweep(t); r.Warned != 1 {
		t.Fatalf("sweep = %+v, want told", r)
	}
	f.router.Used(context.Background(), id)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var warned bool
		if err := f.control.QueryRow(context.Background(), `SELECT dormancy_warned_at IS NOT NULL FROM platform.tenant_database WHERE customer_id = $1::uuid`, id).Scan(&warned); err != nil {
			t.Fatal(err)
		}
		if !warned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("use did not withdraw the notice")
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.now = f.now.AddDate(0, 0, 20)
	if r := f.sweep(t); r.Deleted != 0 || len(f.deleted) != 0 {
		t.Fatalf("a workspace used after its notice was deleted (%+v)", r)
	}
}

// Without a mail relay nothing is retired: a workspace is never deleted
// without notice.
func TestNothingWithoutMail(t *testing.T) {
	f := setup(t)
	f.tenant(t, "Silent", "community", 100)
	d := f.deps()
	d.Mailer = nil
	if r, err := dormancy.Sweep(context.Background(), d); err != nil || r.Warned+r.Deleted != 0 {
		t.Fatalf("sweep without mail = %+v, %v; want nothing", r, err)
	}
}

// Two replicas: while one sweeps, the other stands aside.
func TestOneSweepAtATime(t *testing.T) {
	f := setup(t)
	f.tenant(t, "Once", "community", 31)
	ctx := context.Background()
	tx, err := f.control.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var held bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(731402118)`).Scan(&held); err != nil || !held {
		t.Fatalf("could not take the sweep lock (%v)", err)
	}
	if r := f.sweep(t); r.Warned != 0 {
		t.Fatalf("a sweep ran beside another (%+v)", r)
	}
	_ = tx.Rollback(ctx)
	if r := f.sweep(t); r.Warned != 1 {
		t.Fatalf("sweep after the other finished = %+v, want 1 warned", r)
	}
}

// The mail says the rule is the Playground's: it runs nowhere else.
func TestMailNamesThePlayground(t *testing.T) {
	f := setup(t)
	f.tenant(t, "Named", "community", 31)
	f.sweep(t)
	if sent := f.box.all(); len(sent) != 1 || !strings.Contains(sent[0].body, "Workspaces in the Playground that are not used are deleted") {
		t.Fatalf("mails = %+v, want the Playground named", sent)
	}
}
