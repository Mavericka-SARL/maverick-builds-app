// Package tenantdb gives every tenant its own PostgreSQL database.
//
// The database a deployment starts with is the control plane: it holds the
// tenant catalog and user directory (schema "platform") and, for tenants that
// were created before dedicated databases existed or with TENANT_DB_MODE=shared,
// their data too. A dedicated tenant lives in "tenant_<id>" on the same
// server, carrying the full schema (every migration) and exactly one
// core.customer row, so the SQL the platform already runs works unchanged —
// what changes is which pool a request uses.
//
// The routing decision is made once per request (internal/gateway's tenant
// middleware) and carried in the context; Handle dispatches on it. Nothing
// here parses tokens or knows about roles.
package tenantdb

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ctxKey struct{}

// Scope is what a request carries once its tenant is known: the tenant and
// the pool that reaches its database. Storing the pool (not just the id) means
// opening it can fail loudly in the middleware instead of silently inside a
// handler that has no way to report it.
type Scope struct {
	CustomerID string
	Pool       *pgxpool.Pool
}

// WithScope returns ctx routed to the given tenant. Values survive
// context.WithoutCancel, so background work spawned from a request keeps
// writing to the right database.
func WithScope(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// ScopeFrom returns the tenant scope carried by ctx, if any.
func ScopeFrom(ctx context.Context) (Scope, bool) {
	s, ok := ctx.Value(ctxKey{}).(Scope)
	return s, ok && s.Pool != nil
}

// TenantFrom returns the tenant id carried by ctx, or "" for the control
// plane.
func TenantFrom(ctx context.Context) string {
	s, ok := ScopeFrom(ctx)
	if !ok {
		return ""
	}
	return s.CustomerID
}
