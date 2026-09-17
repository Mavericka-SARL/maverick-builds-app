package tenantdb

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

// Provisioned is what a successful Provision returns: the catalog row and
// the default workspace created inside the new database.
type Provisioned struct {
	Tenant      Tenant
	WorkspaceID string
}

// DatabaseName is the PostgreSQL database a tenant gets: "tenant_" plus the
// customer id without dashes (43 characters, under the 63 limit, only
// [a-z0-9_] so it never needs quoting in DSNs).
func DatabaseName(customerID string) string {
	return "tenant_" + strings.ReplaceAll(customerID, "-", "")
}

// Provision creates a tenant: a catalog row, a new database, every migration,
// the core.customer row (same id as the catalog) and a "Default" workspace.
// A failure after the database exists leaves the catalog row in status failed
// with the reason, so an operator can inspect or Deprovision it; it is never
// silently retried.
func (r *Router) Provision(ctx context.Context, name, plan string) (Provisioned, error) {
	if strings.TrimSpace(name) == "" {
		return Provisioned{}, fmt.Errorf("tenant name is required")
	}
	if plan == "" {
		plan = "starter"
	}
	t := Tenant{CustomerID: uuid.NewString(), Name: name, Plan: plan, Status: StatusProvisioning, CreatedAt: time.Now()}
	t.Database = DatabaseName(t.CustomerID)
	if err := r.catalog.Insert(ctx, t); err != nil {
		return Provisioned{}, fmt.Errorf("catalog insert: %w", err)
	}
	fail := func(step string, err error) (Provisioned, error) {
		reason := step + ": " + err.Error()
		_ = r.catalog.SetStatus(ctx, t.CustomerID, StatusFailed, reason)
		return Provisioned{}, fmt.Errorf("provision tenant %s (%s): %w", t.CustomerID, step, err)
	}

	if err := r.adminExec(ctx, `CREATE DATABASE `+pgx.Identifier{t.Database}.Sanitize()); err != nil {
		return fail("create database", err)
	}
	pool, err := r.open(ctx, t.Database)
	if err != nil {
		return fail("connect", err)
	}
	if err := migrate.Run(ctx, pool, r.cfg.Migrations, r.cfg.MigrationsDir); err != nil {
		pool.Close()
		return fail("migrate", err)
	}
	var wsID string
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO core.customer (id, name, plan) VALUES ($1::uuid, $2, $3)`, t.CustomerID, name, plan); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, t.CustomerID).Scan(&wsID)
	})
	if err != nil {
		pool.Close()
		return fail("seed customer", err)
	}
	if err := r.catalog.SetStatus(ctx, t.CustomerID, StatusReady, ""); err != nil {
		pool.Close()
		return fail("catalog ready", err)
	}
	t.Status = StatusReady
	r.mu.Lock()
	r.pools[t.CustomerID] = &entry{pool: pool, lastUsed: time.Now()}
	r.mu.Unlock()
	r.cfg.Log.Info().Str("tenant", t.CustomerID).Str("database", t.Database).Msg("tenant database provisioned")
	return Provisioned{Tenant: t, WorkspaceID: wsID}, nil
}

// Deprovision drops a tenant's database and catalog row. Destructive and
// final: the caller confirms with the operator.
func (r *Router) Deprovision(ctx context.Context, customerID string) error {
	t, err := r.catalog.Get(ctx, customerID)
	if err != nil {
		return err
	}
	r.Forget(customerID)
	// FORCE disconnects any remaining session (PostgreSQL 13+); a pool that
	// another gateway replica still holds would otherwise block the drop.
	if err := r.adminExec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{t.Database}.Sanitize()+` WITH (FORCE)`); err != nil {
		return fmt.Errorf("drop database %s: %w", t.Database, err)
	}
	if err := r.catalog.Delete(ctx, customerID); err != nil {
		return err
	}
	r.cfg.Log.Info().Str("tenant", customerID).Str("database", t.Database).Msg("tenant database dropped")
	return nil
}

// adminExec runs one statement that cannot run inside a transaction or a
// pooled session (CREATE/DROP DATABASE) on a dedicated admin connection.
func (r *Router) adminExec(ctx context.Context, sql string) error {
	var conn *pgx.Conn
	var err error
	if r.cfg.AdminURL != "" {
		conn, err = pgx.Connect(ctx, r.cfg.AdminURL)
	} else {
		conn, err = pgx.ConnectConfig(ctx, r.control.Config().ConnConfig.Copy())
	}
	if err != nil {
		return fmt.Errorf("admin connection: %w", err)
	}
	defer conn.Close(ctx) //nolint:errcheck
	_, err = conn.Exec(ctx, sql)
	return err
}

// PoolForTest is a test seam: it registers an already-open pool under a
// tenant id without provisioning. Not used by production code.
func (r *Router) PoolForTest(customerID string, pool *pgxpool.Pool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pools[customerID] = &entry{pool: pool, lastUsed: time.Now()}
}
