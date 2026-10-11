package tenantdb

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/pkg/migrate"
	"github.com/mavericks-engine/mavericks/pkg/wake"
)

// Config tunes the Router. Zero values take the defaults documented on each
// field.
type Config struct {
	// MaxConnsPerTenant caps each tenant pool. Small on purpose: hundreds of
	// tenants must not each hold a full pool open. Default 5.
	MaxConnsPerTenant int32
	// IdleTimeout closes a tenant pool that has not been used for this long;
	// the next request reopens it. Default 10 minutes; <0 disables.
	IdleTimeout time.Duration
	// RetryUnreached is how often a tenant whose start-up migration could
	// not reach its database is tried again. Default 30 seconds.
	RetryUnreached time.Duration
	// Migrations and MigrationsDir are applied to every new tenant database
	// (and by MigrateAll at start-up).
	Migrations    embed.FS
	MigrationsDir string
	// AdminURL is a connection string with CREATEDB privilege used only to
	// CREATE/DROP tenant databases. Empty means "the control-plane
	// credentials", which is right for the dev stack and the shipped
	// Kubernetes manifests (the application role owns the server).
	AdminURL string
	// Log for lifecycle messages; zero is a no-op logger.
	Log zerolog.Logger
}

type entry struct {
	pool     *pgxpool.Pool
	lastUsed time.Time
	// held: a background loop started by Watch keeps this pool for its whole
	// life and never comes back through Pool, so lastUsed says nothing about
	// it. The janitor leaves held pools open; only Forget and Close end them.
	held bool
}

// Router owns the tenant pools. It opens them lazily from the catalog, closes
// idle ones, and answers "which pool for this tenant".
type Router struct {
	control *pgxpool.Pool
	cfg     Config
	catalog *Catalog

	mu    sync.Mutex
	pools map[string]*entry // customer id → pool
	stop  chan struct{}
	once  sync.Once

	waker atomic.Pointer[Waker]

	// unreached holds tenants whose start-up migration could not reach
	// their database: refused by Pool, and migrated again in the background.
	unreached map[string]Tenant
	retrying  bool
}

// SetWaker lets this process's background loops sleep while their tenant is
// quiet (see Waker). Without one, Gate is a plain timer.
func (r *Router) SetWaker(w *Waker) { r.waker.Store(w) }

// Touch records activity in a tenant: a request, or a platform
// administrator acting on it.
func (r *Router) Touch(ctx context.Context, customerID string) {
	if w := r.waker.Load(); w != nil {
		w.Touch(ctx, customerID)
	}
}

// Gate is the wake.Gate for a tenant's background loops holding pool.
func (r *Router) Gate(customerID string, pool *pgxpool.Pool) wake.Gate {
	if w := r.waker.Load(); w != nil {
		return w.Gate(customerID, pool)
	}
	return wake.Timer{}
}

// New builds a Router over the control-plane pool. It does not open any
// tenant database until one is asked for.
func New(control *pgxpool.Pool, cfg Config) *Router {
	if cfg.MaxConnsPerTenant <= 0 {
		cfg.MaxConnsPerTenant = 5
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 10 * time.Minute
	}
	if cfg.RetryUnreached <= 0 {
		cfg.RetryUnreached = 30 * time.Second
	}
	if cfg.MigrationsDir == "" {
		cfg.MigrationsDir = "."
	}
	r := &Router{control: control, cfg: cfg, catalog: NewCatalog(control), pools: map[string]*entry{}, stop: make(chan struct{})}
	if cfg.IdleTimeout > 0 {
		go r.janitor()
	}
	return r
}

// Catalog exposes the control-plane catalog the router reads.
func (r *Router) Catalog() *Catalog { return r.catalog }

// Control is the control-plane pool.
func (r *Router) Control() *pgxpool.Pool { return r.control }

// Pool returns the pool for a ready tenant, opening it on first use.
func (r *Router) Pool(ctx context.Context, customerID string) (*pgxpool.Pool, error) {
	r.mu.Lock()
	if _, ok := r.unreached[customerID]; ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("tenant %s database is not migrated yet: it was unreachable at start-up", customerID)
	}
	if e, ok := r.pools[customerID]; ok {
		e.lastUsed = time.Now()
		r.mu.Unlock()
		return e.pool, nil
	}
	r.mu.Unlock()

	t, err := r.catalog.Get(ctx, customerID)
	if err != nil {
		return nil, err
	}
	if t.Status != StatusReady {
		return nil, fmt.Errorf("tenant %s database is %s", customerID, t.Status)
	}
	pool, err := r.open(ctx, t.Database)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.pools[customerID]; ok { // lost a race; keep the first
		pool.Close()
		e.lastUsed = time.Now()
		return e.pool, nil
	}
	r.pools[customerID] = &entry{pool: pool, lastUsed: time.Now()}
	return pool, nil
}

// hold is Pool for a caller that keeps the pool for good (Watch's background
// loops): the pool is marked held so the janitor never closes it under them.
func (r *Router) hold(ctx context.Context, customerID string) (*pgxpool.Pool, error) {
	pool, err := r.Pool(ctx, customerID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.pools[customerID]
	if !ok || e.pool != pool {
		// Forgotten between the two locks: the pool is already closed.
		return nil, fmt.Errorf("tenant %s pool was closed while opening", customerID)
	}
	e.held = true
	// Opening the pool left its check connection idle. Watch takes every
	// tenant's pool at once when a process starts, and those connections
	// would add up to PgBouncer's client limit across hundreds of tenants
	// before their idle time ran out; the loops connect when they run.
	pool.Reset()
	return pool, nil
}

// open connects to database on the control plane's server with the control
// plane's credentials, capped for a single tenant.
func (r *Router) open(ctx context.Context, database string) (*pgxpool.Pool, error) {
	cfg := r.control.Config().Copy()
	cfg.ConnConfig.Database = database
	cfg.MaxConns = r.cfg.MaxConnsPerTenant
	cfg.MinConns = 0
	// A quiet tenant's background loops wait without querying (Waker), and a
	// visit's connections must not linger: across hundreds of tenants they
	// add up to PgBouncer's client limit. Closed after 30s idle, not pgx's
	// 30 minutes.
	cfg.MaxConnIdleTime = 30 * time.Second
	cfg.HealthCheckPeriod = 15 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open tenant database %s: %w", database, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping tenant database %s: %w", database, err)
	}
	return pool, nil
}

// Forget closes and drops the cached pool for a tenant (after deprovisioning
// or when the catalog row changed).
func (r *Router) Forget(customerID string) {
	r.mu.Lock()
	e, ok := r.pools[customerID]
	delete(r.pools, customerID)
	r.mu.Unlock()
	if ok {
		e.pool.Close()
	}
}

// Close releases every tenant pool. The control pool is the caller's.
func (r *Router) Close() {
	r.once.Do(func() { close(r.stop) })
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, e := range r.pools {
		e.pool.Close()
		delete(r.pools, id)
	}
}

func (r *Router) janitor() {
	t := time.NewTicker(r.cfg.IdleTimeout / 2)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case now := <-t.C:
			r.mu.Lock()
			for id, e := range r.pools {
				if !e.held && now.Sub(e.lastUsed) > r.cfg.IdleTimeout {
					e.pool.Close()
					delete(r.pools, id)
				}
			}
			r.mu.Unlock()
		}
	}
}

// Each runs fn against every ready tenant, one database at a time. A tenant
// whose pool is open already uses it; any other gets a pool for the call
// only, closed straight after, so a walk over hundreds of tenants holds one
// connection rather than one per tenant until their idle time runs out. fn
// must not keep the pool. The first error stops the walk.
func (r *Router) Each(ctx context.Context, fn func(t Tenant, pool *pgxpool.Pool) error) error {
	tenants, err := r.catalog.List(ctx)
	if err != nil {
		return err
	}
	for _, t := range tenants {
		if t.Status != StatusReady {
			continue
		}
		r.mu.Lock()
		_, unreached := r.unreached[t.CustomerID]
		e, open := r.pools[t.CustomerID]
		if open {
			e.lastUsed = time.Now()
		}
		r.mu.Unlock()
		switch {
		case unreached:
			continue // not migrated yet: as good as not ready
		case open:
			if err := fn(t, e.pool); err != nil {
				return err
			}
		default:
			pool, err := r.open(ctx, t.Database)
			if err != nil {
				return err
			}
			err = fn(t, pool)
			pool.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// MigrateAll applies pending migrations to every ready tenant database.
// Called at gateway start-up right after the control plane is migrated, so a
// deploy upgrades every tenant before serving. A tenant whose migration
// fails is marked failed (and therefore not routed to) rather than aborting
// the start-up of every other tenant.
func (r *Router) MigrateAll(ctx context.Context) error {
	tenants, err := r.catalog.List(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, t := range tenants {
		if t.Status != StatusReady {
			continue
		}
		err := r.migrateOne(ctx, t)
		switch {
		case err == nil:
		case unreachable(err):
			// Nothing ran: a failure to reach the database is not a failed
			// migration, and must not disable the tenant for good.
			r.cfg.Log.Error().Err(err).Str("tenant", t.CustomerID).Str("database", t.Database).Msg("tenant database unreachable for its migration — held back and retried")
			r.holdBack(ctx, t)
			if firstErr == nil {
				firstErr = err
			}
			continue
		default:
			r.cfg.Log.Error().Err(err).Str("tenant", t.CustomerID).Str("database", t.Database).Msg("tenant database migration failed — tenant disabled until fixed")
			_ = r.catalog.SetStatus(ctx, t.CustomerID, StatusFailed, "migration: "+err.Error())
			r.Forget(t.CustomerID)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		r.cfg.Log.Info().Str("tenant", t.CustomerID).Str("database", t.Database).Msg("tenant database migrated")
	}
	return firstErr
}

// migrateOne migrates one tenant database through a pool of its own, closed
// straight after: keeping every tenant's pool open would hold one connection
// per tenant database at once.
func (r *Router) migrateOne(ctx context.Context, t Tenant) error {
	pool, err := r.open(ctx, t.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	return migrate.Run(ctx, pool, r.cfg.Migrations, r.cfg.MigrationsDir)
}

// unreachable is a failure to reach a database at all — no connection, a
// pooler's wait timeout, a server at its connection limit — as opposed to a
// statement that ran and failed.
func unreachable(err error) bool {
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return true
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		// 08: connection exception (PgBouncer's query_wait_timeout is
		// 08P01); 53300: too many connections; 57P03: cannot connect now.
		return strings.HasPrefix(pe.Code, "08") || pe.Code == "53300" || pe.Code == "57P03"
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// holdBack keeps a tenant whose migration could not reach its database out
// of service in this process, and migrates it again every RetryUnreached
// until it succeeds or fails for a reason other than reaching it.
func (r *Router) holdBack(ctx context.Context, t Tenant) {
	r.Forget(t.CustomerID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unreached == nil {
		r.unreached = map[string]Tenant{}
	}
	r.unreached[t.CustomerID] = t
	if !r.retrying {
		r.retrying = true
		// The retries outlive start-up: they end with the router, not ctx.
		go r.retryUnreached(context.WithoutCancel(ctx))
	}
}

func (r *Router) retryUnreached(base context.Context) {
	tk := time.NewTicker(r.cfg.RetryUnreached)
	defer tk.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-tk.C:
		}
		r.mu.Lock()
		held := make([]Tenant, 0, len(r.unreached))
		for _, t := range r.unreached {
			held = append(held, t)
		}
		if len(held) == 0 {
			r.retrying = false
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		for _, t := range held {
			ctx, cancel := context.WithTimeout(base, 2*time.Minute)
			err := r.migrateOne(ctx, t)
			switch {
			case err == nil:
				r.cfg.Log.Info().Str("tenant", t.CustomerID).Str("database", t.Database).Msg("tenant database migrated after being unreachable")
			case unreachable(err):
				cancel()
				continue // still out of reach: next round
			default:
				r.cfg.Log.Error().Err(err).Str("tenant", t.CustomerID).Str("database", t.Database).Msg("tenant database migration failed — tenant disabled until fixed")
				_ = r.catalog.SetStatus(ctx, t.CustomerID, StatusFailed, "migration: "+err.Error())
			}
			cancel()
			r.mu.Lock()
			delete(r.unreached, t.CustomerID)
			r.mu.Unlock()
		}
	}
}

// Watch calls start once for every ready tenant, now and whenever a new one
// appears, until ctx ends. It is how per-database background loops (the
// workflow scheduler, the integration worker) cover every tenant: each gets
// its own goroutine with its own pool. start must return promptly and run its
// loop on its own goroutine, honouring ctx.
func (r *Router) Watch(ctx context.Context, interval time.Duration, start func(t Tenant, pool *pgxpool.Pool)) {
	seen := map[string]bool{}
	tick := func() {
		tenants, err := r.catalog.List(ctx)
		if err != nil {
			r.cfg.Log.Warn().Err(err).Msg("tenant watch: list catalog")
			return
		}
		for _, t := range tenants {
			if t.Status != StatusReady || seen[t.CustomerID] {
				continue
			}
			pool, err := r.hold(ctx, t.CustomerID)
			if err != nil {
				r.cfg.Log.Warn().Err(err).Str("tenant", t.CustomerID).Msg("tenant watch: open pool")
				continue
			}
			seen[t.CustomerID] = true
			start(t, pool)
		}
	}
	tick()
	go func() {
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				tick()
			}
		}
	}()
}
