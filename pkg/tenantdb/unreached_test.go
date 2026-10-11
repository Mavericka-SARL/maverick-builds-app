package tenantdb

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// Only a failure to reach the database counts as unreachable; a statement
// that ran and failed is a failed migration.
func TestUnreachableTellsConnectionFailuresApart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, connErr := pgx.Connect(ctx, "postgres://u:p@127.0.0.1:1/x?connect_timeout=2")
	for name, c := range map[string]struct {
		err  error
		want bool
	}{
		"no server":                       {connErr, true},
		"PgBouncer wait timeout (08P01)":  {fmt.Errorf("ping: %w", &pgconn.PgError{Code: "08P01"}), true},
		"too many connections (53300)":    {&pgconn.PgError{Code: "53300"}, true},
		"timed out":                       {context.DeadlineExceeded, true},
		"a migration's own error (42P01)": {fmt.Errorf("migrate: %w", &pgconn.PgError{Code: "42P01"}), false},
		"anything else":                   {errors.New("checksum mismatch"), false},
	} {
		if got := unreachable(c.err); got != c.want {
			t.Errorf("%s: unreachable = %v, want %v (err %v)", name, got, c.want, c.err)
		}
	}
}

// A tenant held back as unreachable is refused, not disabled, and comes
// back by itself once its migration goes through.
func TestUnreachedTenantIsHeldBackThenMigrated(t *testing.T) {
	ctx := context.Background()
	control := testdb.New(t, migrationfs.FS, ".")
	r := New(control, Config{
		Migrations:     migrationfs.FS,
		MigrationsDir:  ".",
		AdminURL:       testdb.AdminDSN(t),
		IdleTimeout:    -1,
		RetryUnreached: 50 * time.Millisecond,
	})
	t.Cleanup(r.Close)
	p, err := r.Provision(ctx, "A", "")
	if err != nil {
		t.Fatal(err)
	}
	r.holdBack(ctx, p.Tenant)
	if _, err := r.Pool(ctx, p.Tenant.CustomerID); err == nil {
		t.Fatal("a held-back tenant must be refused until it is migrated")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := r.Pool(ctx, p.Tenant.CustomerID); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the held-back tenant was not migrated again")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, _ := r.Catalog().Get(ctx, p.Tenant.CustomerID); got.Status != StatusReady {
		t.Fatalf("status = %s, want ready: unreachable is not failed", got.Status)
	}
}
