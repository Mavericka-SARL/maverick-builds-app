package tenant_test

import (
	"context"
	"embed"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/tenant"
	"github.com/mavericks-engine/mavericks/internal/testdb"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

func setupDB(t *testing.T) *tenant.Store {
	t.Helper()

	pool := testdb.New(t, testMigrations, "testdata")

	return tenant.NewStore(pool)
}

func TestCreateAndGetCustomer(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()

	c, err := store.CreateCustomer(ctx, "Acme Corp", "enterprise")
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	if c.Name != "Acme Corp" {
		t.Errorf("name = %q, want %q", c.Name, "Acme Corp")
	}

	got, err := store.GetCustomer(ctx, c.Id)
	if err != nil {
		t.Fatalf("get customer: %v", err)
	}
	if got.Id != c.Id {
		t.Errorf("id mismatch: got %q want %q", got.Id, c.Id)
	}
}

func TestCreateWorkspaceAndApplication(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()

	c, _ := store.CreateCustomer(ctx, "TestCo", "starter")
	w, err := store.CreateWorkspace(ctx, c.Id, "main")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	app, err := store.CreateApplication(ctx, w.Id, "OPEX Planning", "planning")
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	if app.Name != "OPEX Planning" {
		t.Errorf("name = %q, want %q", app.Name, "OPEX Planning")
	}

	apps, err := store.ListApplications(ctx, w.Id, 10, 0)
	if err != nil {
		t.Fatalf("list applications: %v", err)
	}
	if len(apps) != 1 {
		t.Errorf("len(apps) = %d, want 1", len(apps))
	}
}
