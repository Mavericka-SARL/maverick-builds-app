package tenantdb

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Handle is what the gateway holds instead of a bare *pgxpool.Pool. It has
// the same four methods the handlers use, each dispatching on the request
// context: a routed request runs against its tenant's database, anything else
// against the control plane. For places that need a real pool (package
// constructors, the audit log), For(ctx) returns the same choice.
//
// Routing is decided by the context alone: a Handle over a nil Router is the
// shared-database behaviour the platform had before dedicated tenants,
// because nothing sets a scope then. (A test may still inject a scope to
// point one request at a pool of its own.)
type Handle struct {
	control *pgxpool.Pool
	router  *Router
}

// NewHandle wraps the control-plane pool; router may be nil.
func NewHandle(control *pgxpool.Pool, router *Router) *Handle {
	return &Handle{control: control, router: router}
}

// Control is the control-plane pool, regardless of ctx.
func (h *Handle) Control() *pgxpool.Pool { return h.control }

// Router is the tenant router, or nil in shared mode.
func (h *Handle) Router() *Router { return h.router }

// Dedicated reports whether tenant routing is configured at all.
func (h *Handle) Dedicated() bool { return h != nil && h.router != nil }

// For returns the pool a request should use: the tenant's when ctx is
// routed, the control plane otherwise.
func (h *Handle) For(ctx context.Context) *pgxpool.Pool {
	if s, ok := ScopeFrom(ctx); ok {
		return s.Pool
	}
	return h.control
}

func (h *Handle) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return h.For(ctx).QueryRow(ctx, sql, args...)
}

func (h *Handle) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return h.For(ctx).Query(ctx, sql, args...)
}

func (h *Handle) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return h.For(ctx).Exec(ctx, sql, args...)
}

func (h *Handle) Begin(ctx context.Context) (pgx.Tx, error) {
	return h.For(ctx).Begin(ctx)
}

func (h *Handle) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	return h.For(ctx).BeginTx(ctx, opts)
}
