package tenantdb

import (
	"context"
	"embed"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/pkg/migrate"
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

// open connects to database on the control plane's server with the control
// plane's credentials, capped for a single tenant.
func (r *Router) open(ctx context.Context, database string) (*pgxpool.Pool, error) {
	cfg := r.control.Config().Copy()
	cfg.ConnConfig.Database = database
	cfg.MaxConns = r.cfg.MaxConnsPerTenant
	cfg.MinConns = 0
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
				if now.Sub(e.lastUsed) > r.cfg.IdleTimeout {
					e.pool.Close()
					delete(r.pools, id)
				}
			}
			r.mu.Unlock()
		}
	}
}

// Each runs fn against every ready tenant. The first error stops the walk.
func (r *Router) Each(ctx context.Context, fn func(t Tenant, pool *pgxpool.Pool) error) error {
	tenants, err := r.catalog.List(ctx)
	if err != nil {
		return err
	}
	for _, t := range tenants {
		if t.Status != StatusReady {
			continue
		}
		pool, err := r.Pool(ctx, t.CustomerID)
		if err != nil {
			return err
		}
		if err := fn(t, pool); err != nil {
			return err
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
		pool, err := r.Pool(ctx, t.CustomerID)
		if err == nil {
			err = migrate.Run(ctx, pool, r.cfg.Migrations, r.cfg.MigrationsDir)
		}
		if err != nil {
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
			pool, err := r.Pool(ctx, t.CustomerID)
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
