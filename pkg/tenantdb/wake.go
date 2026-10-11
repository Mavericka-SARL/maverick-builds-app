package tenantdb

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// NextDue reports when a tenant next has scheduled background work, as a
// time on the tenant database's own clock; the zero time when nothing is
// scheduled. Each process registers the ones its loops act on.
type NextDue func(ctx context.Context, pool *pgxpool.Pool) (time.Time, error)

// WakeConfig tunes a Waker. Zero values take the defaults on each field.
type WakeConfig struct {
	// ActiveFor is how long a tenant stays awake after its last activity.
	// Default 10 minutes.
	ActiveFor time.Duration
	// Poll is how often activity recorded by other processes is read from
	// the control plane, and so how late a wake-up can be. Default 2 seconds.
	Poll time.Duration
	// MaxSleep is the longest a quiet tenant goes without a visit: the
	// safety net for work no NextDue reports. Default 1 hour.
	MaxSleep time.Duration
	// Retry is how soon a quiet tenant is visited again when its next due
	// time could not be read, or had already passed. Default 1 minute.
	Retry time.Duration
	// StartSpread spreads every tenant's first visit over this long, so a
	// start does not open every tenant database at once. Its next due time
	// is read before that, so a schedule is still kept. Default 10 minutes.
	StartSpread time.Duration
	// Settle is how long after a visit the next due time is read again, so
	// the loops it woke have had their pass. Default 30 seconds.
	Settle time.Duration
	// Record is how often this process writes one tenant's activity to the
	// control plane at most. Default 30 seconds.
	Record time.Duration
	Log    zerolog.Logger
}

// Waker lets one process's background loops sleep while their tenant is
// quiet (see pkg/wake). A tenant is active for ActiveFor after anything
// happens in it: a request, a platform administrator acting on it, or a
// background pass that found work (Touch). Activity is written to
// platform.tenant_database.last_active_at, so the other processes wake too.
// While a tenant is quiet its loops wait, and the Waker visits it — wakes
// each loop for one pass — when its next scheduled item comes due, and at
// least every MaxSleep.
type Waker struct {
	control *pgxpool.Pool
	cfg     WakeConfig
	sources []NextDue

	mu        sync.Mutex
	tenants   map[string]*sleeper
	computing bool
}

type sleeper struct {
	pool       *pgxpool.Pool // set once a loop holds a gate for the tenant
	lastActive time.Time     // latest activity, seen here or read from the control plane
	recorded   time.Time     // when this process last wrote activity for it
	nextDue    time.Time     // when to visit while quiet
	firstVisit time.Time     // the visit after a start, for the one-off passes; zero once woken
	stale      bool          // nextDue must be read again (after activity or a visit)
	settleAt   time.Time     // not before this
	gen        uint64        // bumped by activity and visits; a read started before is dropped
	wake       chan struct{} // closed to wake the tenant's waiting loops
}

// NewWaker returns a Waker reading activity from control and next due times
// from sources. Start it before any Gate is used.
func NewWaker(control *pgxpool.Pool, cfg WakeConfig, sources ...NextDue) *Waker {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.ActiveFor, 10*time.Minute)
	def(&cfg.Poll, 2*time.Second)
	def(&cfg.MaxSleep, time.Hour)
	def(&cfg.Retry, time.Minute)
	def(&cfg.StartSpread, 10*time.Minute)
	def(&cfg.Settle, 30*time.Second)
	def(&cfg.Record, 30*time.Second)
	return &Waker{control: control, cfg: cfg, sources: sources, tenants: map[string]*sleeper{}}
}

// Start reads current activity once, so tenants already active are awake
// from the first pass, then keeps reading it every Poll until ctx ends.
func (w *Waker) Start(ctx context.Context) {
	w.readActivity(ctx)
	go func() {
		t := time.NewTicker(w.cfg.Poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.tick(ctx)
			}
		}
	}()
}

// Gate is the wake.Gate of a tenant's background loops in this process;
// pool is the tenant pool the loops hold, used to read its next due time.
func (w *Waker) Gate(customerID string, pool *pgxpool.Pool) *Gate {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.sleeper(customerID)
	if s.pool == nil {
		s.pool = pool
		// Read its next due time at once, one tenant at a time; the first
		// visit, which runs every loop once, is spread out.
		s.stale = true
		if now := time.Now(); !w.awake(s, now) {
			s.firstVisit = now.Add(rand.N(w.cfg.StartSpread)) //nolint:gosec // spreading load, not security
		}
	}
	return &Gate{w: w, id: customerID}
}

// Touch records activity in a tenant: it is awake here at once, and in the
// other processes once the write reaches the control plane. The write does
// not wait for, or end with, ctx.
func (w *Waker) Touch(ctx context.Context, customerID string) {
	if w == nil || customerID == "" {
		return
	}
	now := time.Now()
	w.mu.Lock()
	s := w.sleeper(customerID)
	w.activeLocked(s, now, now)
	record := now.Sub(s.recorded) >= w.cfg.Record
	if record {
		s.recorded = now
	}
	w.mu.Unlock()
	if record {
		go w.record(context.WithoutCancel(ctx), customerID)
	}
}

func (w *Waker) record(ctx context.Context, customerID string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := w.control.Exec(ctx,
		`UPDATE platform.tenant_database SET last_active_at = now() WHERE customer_id = $1::uuid`,
		customerID); err != nil {
		w.cfg.Log.Warn().Err(err).Str("tenant", customerID).Msg("tenant wake: record activity")
	}
}

// sleeper returns the tenant's state, creating it. Caller holds w.mu.
func (w *Waker) sleeper(customerID string) *sleeper {
	s, ok := w.tenants[customerID]
	if !ok {
		s = &sleeper{wake: make(chan struct{})}
		w.tenants[customerID] = s
	}
	return s
}

// activeLocked notes activity at `at`, waking the loops if the tenant was
// quiet. Caller holds w.mu.
func (w *Waker) activeLocked(s *sleeper, at, now time.Time) {
	if !at.After(s.lastActive) {
		return
	}
	wasAwake := w.awake(s, now)
	s.lastActive = at
	s.stale = true
	s.gen++
	if !wasAwake && w.awake(s, now) {
		w.wakeLocked(s)
	}
}

func (w *Waker) awake(s *sleeper, now time.Time) bool {
	return !s.lastActive.IsZero() && now.Sub(s.lastActive) < w.cfg.ActiveFor
}

func (w *Waker) wakeLocked(s *sleeper) {
	close(s.wake)
	s.wake = make(chan struct{})
	s.firstVisit = time.Time{}
}

// readActivity takes in activity other processes recorded.
func (w *Waker) readActivity(ctx context.Context) {
	rows, err := w.control.Query(ctx, `
		SELECT customer_id::text, last_active_at FROM platform.tenant_database
		WHERE status = 'ready' AND last_active_at > now() - make_interval(secs => $1)`,
		w.cfg.ActiveFor.Seconds())
	if err != nil {
		if ctx.Err() == nil {
			w.cfg.Log.Warn().Err(err).Msg("tenant wake: read activity")
		}
		return
	}
	defer rows.Close()
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	for rows.Next() {
		var id string
		var at time.Time
		if rows.Scan(&id, &at) == nil {
			w.activeLocked(w.sleeper(id), at, now)
		}
	}
}

// tick visits quiet tenants whose time has come and reads the next due time
// of those that need it.
func (w *Waker) tick(ctx context.Context) {
	w.readActivity(ctx)
	now := time.Now()
	var stale []string
	w.mu.Lock()
	for id, s := range w.tenants {
		if s.pool == nil || w.awake(s, now) {
			continue
		}
		switch {
		case s.stale:
			if !now.Before(s.settleAt) {
				stale = append(stale, id)
			}
		case !now.Before(s.nextDue):
			// A visit: each loop runs one pass. The tenant is not marked
			// active; a pass that finds work touches it.
			w.wakeLocked(s)
			s.stale = true
			s.settleAt = now.Add(w.cfg.Settle)
			s.gen++
		}
	}
	start := len(stale) > 0 && !w.computing
	if start {
		w.computing = true
	}
	w.mu.Unlock()
	if start {
		go w.readDue(ctx, stale)
	}
}

// readDue reads the next due time of each tenant in turn, one database at
// a time.
func (w *Waker) readDue(ctx context.Context, ids []string) {
	defer func() {
		w.mu.Lock()
		w.computing = false
		w.mu.Unlock()
	}()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		w.mu.Lock()
		s := w.tenants[id]
		pool, gen := s.pool, s.gen
		w.mu.Unlock()
		qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		d, err := w.untilDue(qctx, pool)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.cfg.Log.Warn().Err(err).Str("tenant", id).Msg("tenant wake: read next due time")
			d = w.cfg.Retry
		}
		w.mu.Lock()
		quiet := s.gen == gen
		if quiet {
			s.nextDue = time.Now().Add(d)
			if !s.firstVisit.IsZero() && s.firstVisit.Before(s.nextDue) {
				s.nextDue = s.firstVisit
			}
			s.stale = false
		}
		w.mu.Unlock()
		if quiet {
			// Going to sleep: close its connections now rather than after
			// the pool's idle time. A start reads every tenant in turn, and
			// connections left idle that long add up to PgBouncer's client
			// limit across hundreds of tenants.
			pool.Reset()
		}
	}
}

// untilDue is how long until the tenant's next scheduled item, measured on
// the database's clock so a skewed local clock does not wake it early.
func (w *Waker) untilDue(ctx context.Context, pool *pgxpool.Pool) (time.Duration, error) {
	var dbNow time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		return 0, err
	}
	var next time.Time
	for _, src := range w.sources {
		t, err := src(ctx, pool)
		if err != nil {
			return 0, err
		}
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	if next.IsZero() {
		return w.cfg.MaxSleep, nil
	}
	d := next.Sub(dbNow)
	if d <= 0 {
		// Due already and still there after a visit: a loop is on it, or
		// it cannot be done yet. Look again soon rather than at once.
		return w.cfg.Retry, nil
	}
	// A second past due, so the loop's "due by now()" sees it.
	return min(d+time.Second, w.cfg.MaxSleep), nil
}

// Gate is one tenant's wake.Gate in this process.
type Gate struct {
	w  *Waker
	id string
}

// Wait implements wake.Gate.
func (g *Gate) Wait(ctx context.Context, d time.Duration) bool {
	var elapsed <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		elapsed = t.C
	}
	for {
		g.w.mu.Lock()
		s := g.w.tenants[g.id]
		awake, woken := g.w.awake(s, time.Now()), s.wake
		g.w.mu.Unlock()
		switch {
		case !awake:
			select {
			case <-ctx.Done():
				return false
			case <-woken:
				return true
			}
		case elapsed == nil:
			return ctx.Err() == nil
		default:
			select {
			case <-ctx.Done():
				return false
			case <-elapsed:
				// Run the pass if the tenant is still active; otherwise
				// wait for it to wake.
				elapsed = nil
			}
		}
	}
}

// Touch implements wake.Gate.
func (g *Gate) Touch() { g.w.Touch(context.Background(), g.id) }
