package tenantdb_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
	"github.com/mavericks-engine/mavericks/pkg/wake"
)

// fastWake is WakeConfig at test speed: a tenant stays active for 400ms.
func fastWake() tenantdb.WakeConfig {
	return tenantdb.WakeConfig{
		ActiveFor:   400 * time.Millisecond,
		Poll:        20 * time.Millisecond,
		MaxSleep:    time.Hour,
		Retry:       time.Hour,
		StartSpread: time.Millisecond,
		Settle:      50 * time.Millisecond,
		Record:      time.Millisecond,
	}
}

// readyTenant adds a catalog row; its "database" is the control plane
// itself, which is all a gate needs to read next due times from.
func readyTenant(t *testing.T, control *pgxpool.Pool) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := control.Exec(context.Background(), `
		INSERT INTO platform.tenant_database (customer_id, name, database, status)
		VALUES ($1::uuid, 'T', 'tenant_'||replace($1::text, '-', ''), 'ready')`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// passes runs a wake.Loop every 20ms and counts its passes.
func passes(ctx context.Context, g wake.Gate) *atomic.Int64 {
	var n atomic.Int64
	go wake.Loop(ctx, g, 20*time.Millisecond, func(context.Context) bool {
		n.Add(1)
		return false
	})
	return &n
}

// A quiet tenant's loop stops polling; activity recorded by another process
// wakes it within a poll, and it sleeps again once ActiveFor has passed.
func TestWakerSleepsWhileQuietAndWakesOnActivityElsewhere(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := testdb.New(t, migrationfs.FS, ".")
	id := readyTenant(t, control)

	here := tenantdb.NewWaker(control, fastWake())
	elsewhere := tenantdb.NewWaker(control, fastWake())
	here.Start(ctx)
	elsewhere.Start(ctx)
	n := passes(ctx, here.Gate(id, control))

	// The first visit runs one pass; nothing is due, so it sleeps.
	time.Sleep(300 * time.Millisecond)
	settled := n.Load()
	if settled < 1 {
		t.Fatal("the first visit ran no pass")
	}
	time.Sleep(400 * time.Millisecond)
	if got := n.Load(); got != settled {
		t.Fatalf("a quiet tenant's loop ran %d passes while it should sleep", got-settled)
	}

	elsewhere.Touch(ctx, id)
	time.Sleep(300 * time.Millisecond)
	if got := n.Load() - settled; got < 5 {
		t.Fatalf("after activity elsewhere: %d passes in 300ms, want the 20ms cadence", got)
	}
	time.Sleep(600 * time.Millisecond) // past ActiveFor
	quiet := n.Load()
	time.Sleep(300 * time.Millisecond)
	if got := n.Load(); got != quiet {
		t.Fatalf("%d passes after the tenant went quiet again", got-quiet)
	}
}

// A quiet tenant is visited when its next scheduled item comes due, not
// before, and goes back to sleep when nothing more is due.
func TestWakerVisitsAtNextDue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := testdb.New(t, migrationfs.FS, ".")
	id := readyTenant(t, control)

	// Due 1.5s from now, and reported until it has passed.
	var at time.Time
	if err := control.QueryRow(ctx, `SELECT now() + interval '1500 milliseconds'`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	due := func(ctx context.Context, pool *pgxpool.Pool) (time.Time, error) {
		var now time.Time
		if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil || !now.Before(at) {
			return time.Time{}, err
		}
		return at, nil
	}
	w := tenantdb.NewWaker(control, fastWake(), due)
	w.Start(ctx)

	var visits []time.Time
	visited := make(chan time.Time, 10)
	go wake.Loop(ctx, w.Gate(id, control), 20*time.Millisecond, func(context.Context) bool {
		visited <- time.Now()
		return false
	})
	deadline := time.After(5 * time.Second)
	for len(visits) < 2 {
		select {
		case at := <-visited:
			visits = append(visits, at)
		case <-deadline:
			t.Fatalf("visits = %v, want the start visit and one at the due time", visits)
		}
	}
	// The start visit comes at once (StartSpread 1ms); the next a second
	// past due, never at the loop's 20ms cadence.
	if gap := visits[1].Sub(visits[0]); gap < 2*time.Second || gap > 3500*time.Millisecond {
		t.Fatalf("visit came %v after the start visit, want about 2.5s", gap)
	}
	select {
	case extra := <-visited:
		t.Fatalf("another pass %v after the due visit, with nothing due", extra.Sub(visits[1]))
	case <-time.After(time.Second):
	}
}

// A tenant active when the process starts is awake from the first pass, and
// Touch records the activity in the control plane.
func TestWakerStartsAwakeAndRecordsActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control := testdb.New(t, migrationfs.FS, ".")
	active, quiet := readyTenant(t, control), readyTenant(t, control)
	if _, err := control.Exec(ctx, `UPDATE platform.tenant_database SET last_active_at = now() WHERE customer_id = $1::uuid`, active); err != nil {
		t.Fatal(err)
	}
	cfg := fastWake()
	cfg.StartSpread = time.Hour // the quiet tenant's first visit is far off
	w := tenantdb.NewWaker(control, cfg)
	w.Start(ctx)

	wctx, wcancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer wcancel()
	if !w.Gate(active, control).Wait(wctx, 0) {
		t.Fatal("a tenant active at start must run its first pass at once")
	}
	if w.Gate(quiet, control).Wait(wctx, 0) {
		t.Fatal("a quiet tenant must wait for its first visit")
	}

	w.Touch(ctx, quiet)
	deadline := time.Now().Add(2 * time.Second)
	for {
		var recorded bool
		if err := control.QueryRow(ctx, `SELECT last_active_at IS NOT NULL FROM platform.tenant_database WHERE customer_id = $1::uuid`, quiet).Scan(&recorded); err != nil {
			t.Fatal(err)
		}
		if recorded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Touch did not record activity in the control plane")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Without a Waker the router's gate is a plain timer: shared-database
// deployments and single tenants keep their loops' intervals.
func TestRouterGateWithoutWakerIsTimer(t *testing.T) {
	r, control := newRouter(t)
	if _, ok := r.Gate("x", control).(wake.Timer); !ok {
		t.Fatal("Gate without a Waker must be wake.Timer")
	}
	r.Touch(context.Background(), "x") // no Waker: nothing to do, must not panic
}
