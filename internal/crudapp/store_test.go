package crudapp_test

import (
	"context"
	"embed"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

func setupDB(t *testing.T) (*crudapp.Store, func()) {
	t.Helper()
	ctx := context.Background()

	pgc, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("mavericks"),
		tcpostgres.WithUsername("mavericks"),
		tcpostgres.WithPassword("mavericks"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}

	dsn, err := pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pgc.Terminate(ctx)
		t.Fatalf("connection string: %v", err)
	}

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		_ = pgc.Terminate(ctx)
		t.Fatalf("connect: %v", err)
	}

	if err := migrate.Run(ctx, pool, testMigrations, "testdata"); err != nil {
		pool.Close()
		_ = pgc.Terminate(ctx)
		t.Fatalf("migrate: %v", err)
	}

	return crudapp.NewStore(pool), func() {
		pool.Close()
		_ = pgc.Terminate(ctx)
	}
}

func insertModel(t *testing.T, store *crudapp.Store) string {
	t.Helper()
	var modelID string
	err := store.Pool().QueryRow(context.Background(), `
		WITH
		  c AS (INSERT INTO core.customer (name) VALUES ('test') RETURNING id),
		  w AS (INSERT INTO core.workspace (customer_id, name) SELECT id, 'ws' FROM c RETURNING id),
		  a AS (INSERT INTO core.application (workspace_id, name) SELECT id, 'app' FROM w RETURNING id)
		INSERT INTO core.model (application_id, name) SELECT id, 'model' FROM a RETURNING id::text
	`).Scan(&modelID)
	if err != nil {
		t.Fatalf("insert model: %v", err)
	}
	return modelID
}

func insertUser(t *testing.T, store *crudapp.Store) string {
	t.Helper()
	var userID string
	err := store.Pool().QueryRow(context.Background(),
		`INSERT INTO identity.user (email) VALUES (gen_random_uuid()::text||'@x.test') RETURNING id::text`,
	).Scan(&userID)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return userID
}

func sampleFields() []crudapp.FormField {
	return []crudapp.FormField{
		{Name: "title", Label: "Title", Type: "text", Required: true},
		{Name: "amount", Label: "Amount", Type: "number", Required: true},
		{Name: "category", Label: "Category", Type: "select", Required: false, Options: []string{"IT", "TRAVEL", "HR"}},
	}
}

func TestCreateAndGetForm(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := insertModel(t, store)

	form, err := store.CreateForm(ctx, modelID, "", "purchase_request", "Purchase Request", sampleFields())
	if err != nil {
		t.Fatalf("CreateForm: %v", err)
	}
	if form.ID == "" {
		t.Error("expected non-empty form ID")
	}
	if len(form.Fields) != 3 {
		t.Errorf("fields = %d, want 3", len(form.Fields))
	}

	got, err := store.GetForm(ctx, form.ID)
	if err != nil {
		t.Fatalf("GetForm: %v", err)
	}
	if got.Name != "purchase_request" {
		t.Errorf("name = %q, want purchase_request", got.Name)
	}
	if got.Fields[2].Options[0] != "IT" {
		t.Errorf("options[0] = %q, want IT", got.Fields[2].Options[0])
	}
}

func TestListForms(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := insertModel(t, store)

	for _, name := range []string{"form_a", "form_b", "form_c"} {
		if _, err := store.CreateForm(ctx, modelID, "", name, name, nil); err != nil {
			t.Fatalf("CreateForm %s: %v", name, err)
		}
	}

	forms, err := store.ListForms(ctx, modelID, "")
	if err != nil {
		t.Fatalf("ListForms: %v", err)
	}
	if len(forms) != 3 {
		t.Errorf("len = %d, want 3", len(forms))
	}
}

func TestCreateAndListRecords(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := insertModel(t, store)
	userID := insertUser(t, store)

	form, err := store.CreateForm(ctx, modelID, "", "pr", "PR", sampleFields())
	if err != nil {
		t.Fatalf("CreateForm: %v", err)
	}

	data := map[string]any{"title": "Laptops Q1", "amount": 15000, "category": "IT"}
	rec, err := store.CreateRecord(ctx, form.ID, userID, data)
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	if rec.Status != "draft" {
		t.Errorf("status = %q, want draft", rec.Status)
	}

	records, err := store.ListRecords(ctx, form.ID, 10)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("len = %d, want 1", len(records))
	}
	if records[0].Data["title"] != "Laptops Q1" {
		t.Errorf("data[title] = %v, want Laptops Q1", records[0].Data["title"])
	}
}

func TestGetRecord(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := insertModel(t, store)
	userID := insertUser(t, store)

	form, _ := store.CreateForm(ctx, modelID, "", "f", "F", nil)
	rec, err := store.CreateRecord(ctx, form.ID, userID, map[string]any{"x": 42})
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}

	got, err := store.GetRecord(ctx, rec.ID)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if got.ID != rec.ID {
		t.Errorf("ID mismatch")
	}
}

func TestUpdateRecord(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := insertModel(t, store)
	userID := insertUser(t, store)

	form, _ := store.CreateForm(ctx, modelID, "", "f", "F", nil)
	rec, _ := store.CreateRecord(ctx, form.ID, userID, map[string]any{"amount": 100})

	if err := store.UpdateRecord(ctx, rec.ID, "submitted", map[string]any{"amount": 200}); err != nil {
		t.Fatalf("UpdateRecord: %v", err)
	}

	got, _ := store.GetRecord(ctx, rec.ID)
	if got.Status != "submitted" {
		t.Errorf("status = %q, want submitted", got.Status)
	}
	if got.Data["amount"].(float64) != 200 {
		t.Errorf("amount = %v, want 200", got.Data["amount"])
	}
}

func TestDeleteRecord(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := insertModel(t, store)
	userID := insertUser(t, store)

	form, _ := store.CreateForm(ctx, modelID, "", "f", "F", nil)
	rec, _ := store.CreateRecord(ctx, form.ID, userID, map[string]any{})

	if err := store.DeleteRecord(ctx, rec.ID); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if err := store.DeleteRecord(ctx, rec.ID); err == nil {
		t.Error("expected error deleting already-deleted record")
	}
}

func TestGetForm_NotFound(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()

	_, err := store.GetForm(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Error("expected error for unknown form ID")
	}
}
