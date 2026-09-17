package tenantdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status of a tenant database in the catalog.
const (
	StatusProvisioning = "provisioning"
	StatusReady        = "ready"
	StatusFailed       = "failed"
	StatusDisabled     = "disabled"
)

// Tenant is one row of platform.tenant_database: a tenant that has (or is
// getting) its own database.
type Tenant struct {
	CustomerID string    `json:"customer_id"`
	Name       string    `json:"name"`
	Plan       string    `json:"plan"`
	Database   string    `json:"database"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ErrNotFound is returned for a tenant or directory entry that does not exist.
var ErrNotFound = errors.New("tenant not found")

// Catalog reads and writes the control-plane tables that describe dedicated
// tenants and where their users and applications live. It only ever talks to
// the control pool.
type Catalog struct {
	control *pgxpool.Pool
}

// NewCatalog wraps the control-plane pool.
func NewCatalog(control *pgxpool.Pool) *Catalog { return &Catalog{control: control} }

const tenantColumns = `customer_id::text, name, plan, database, status, error, created_at`

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	err := row.Scan(&t.CustomerID, &t.Name, &t.Plan, &t.Database, &t.Status, &t.Error, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// Insert records a tenant that is about to be provisioned.
func (c *Catalog) Insert(ctx context.Context, t Tenant) error {
	_, err := c.control.Exec(ctx, `
		INSERT INTO platform.tenant_database (customer_id, name, plan, database, status)
		VALUES ($1::uuid, $2, $3, $4, $5)`, t.CustomerID, t.Name, t.Plan, t.Database, t.Status)
	return err
}

// SetStatus moves a tenant to status, recording why when it failed.
func (c *Catalog) SetStatus(ctx context.Context, customerID, status, reason string) error {
	_, err := c.control.Exec(ctx, `
		UPDATE platform.tenant_database SET status = $2, error = $3, updated_at = now()
		WHERE customer_id = $1::uuid`, customerID, status, reason)
	return err
}

// Rename keeps the catalog's copy of the tenant name and plan in step with
// the core.customer row inside the tenant database.
func (c *Catalog) Rename(ctx context.Context, customerID, name, plan string) error {
	_, err := c.control.Exec(ctx, `
		UPDATE platform.tenant_database SET name = $2, plan = $3, updated_at = now()
		WHERE customer_id = $1::uuid`, customerID, name, plan)
	return err
}

// Get returns one tenant.
func (c *Catalog) Get(ctx context.Context, customerID string) (Tenant, error) {
	return scanTenant(c.control.QueryRow(ctx,
		`SELECT `+tenantColumns+` FROM platform.tenant_database WHERE customer_id = $1::uuid`, customerID))
}

// List returns every tenant in the catalog, oldest first.
func (c *Catalog) List(ctx context.Context) ([]Tenant, error) {
	rows, err := c.control.Query(ctx, `SELECT `+tenantColumns+` FROM platform.tenant_database ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Delete removes the tenant and, by cascade, its directory rows.
func (c *Catalog) Delete(ctx context.Context, customerID string) error {
	_, err := c.control.Exec(ctx, `DELETE FROM platform.tenant_database WHERE customer_id = $1::uuid`, customerID)
	return err
}

// ── directory ────────────────────────────────────────────────────────────────

// AddUser records that the identity with keycloak_sub has a user row in the
// tenant's database. Idempotent.
func (c *Catalog) AddUser(ctx context.Context, sub, customerID, email string) error {
	_, err := c.control.Exec(ctx, `
		INSERT INTO platform.user_directory (keycloak_sub, customer_id, email)
		VALUES ($1, $2::uuid, $3)
		ON CONFLICT (keycloak_sub, customer_id) DO UPDATE SET email = EXCLUDED.email`, sub, customerID, email)
	return err
}

// RemoveUser forgets a membership.
func (c *Catalog) RemoveUser(ctx context.Context, sub, customerID string) error {
	_, err := c.control.Exec(ctx,
		`DELETE FROM platform.user_directory WHERE keycloak_sub = $1 AND customer_id = $2::uuid`, sub, customerID)
	return err
}

// TenantsForUser lists the dedicated tenants an identity belongs to, oldest
// membership first.
func (c *Catalog) TenantsForUser(ctx context.Context, sub string) ([]string, error) {
	rows, err := c.control.Query(ctx,
		`SELECT customer_id::text FROM platform.user_directory WHERE keycloak_sub = $1 ORDER BY created_at`, sub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AddApplication records which tenant database holds an application, so a
// request carrying only X-App-Id can be routed.
func (c *Catalog) AddApplication(ctx context.Context, applicationID, customerID string) error {
	_, err := c.control.Exec(ctx, `
		INSERT INTO platform.application_directory (application_id, customer_id)
		VALUES ($1::uuid, $2::uuid) ON CONFLICT (application_id) DO NOTHING`, applicationID, customerID)
	return err
}

// RemoveApplication forgets an application.
func (c *Catalog) RemoveApplication(ctx context.Context, applicationID string) error {
	_, err := c.control.Exec(ctx, `DELETE FROM platform.application_directory WHERE application_id = $1::uuid`, applicationID)
	return err
}

// TenantForApplication returns the tenant that holds applicationID, or
// ErrNotFound when the application is not in a dedicated database.
func (c *Catalog) TenantForApplication(ctx context.Context, applicationID string) (string, error) {
	var id string
	err := c.control.QueryRow(ctx,
		`SELECT customer_id::text FROM platform.application_directory WHERE application_id = $1::uuid`, applicationID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("application directory: %w", err)
	}
	return id, nil
}
