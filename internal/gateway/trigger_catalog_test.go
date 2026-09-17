package gateway

import (
	"context"
	"embed"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

//go:embed testdata/*.sql
var catalogTestMigrations embed.FS

func TestBuildCatalogSystemEventsAlwaysPresent(t *testing.T) {
	ctx := context.Background()

	pool := testdb.New(t, catalogTestMigrations, "testdata")

	// Insert app with NO model → system events only
	var custID, wsID, appID string
	_ = pool.QueryRow(ctx, `INSERT INTO core.customer (name) VALUES ('X') RETURNING id::text`).Scan(&custID)
	_ = pool.QueryRow(ctx, `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'w') RETURNING id::text`, custID).Scan(&wsID)
	_ = pool.QueryRow(ctx, `INSERT INTO core.application (workspace_id, name) VALUES ($1::uuid, 'A') RETURNING id::text`, wsID).Scan(&appID)

	catalog, err := buildWorkflowTriggerEventCatalog(ctx, pool, appID, "")
	if err != nil {
		t.Fatalf("buildWorkflowTriggerEventCatalog: %v", err)
	}

	keys := make(map[string]bool)
	for _, item := range catalog {
		keys[item.Key] = true
	}

	for _, want := range []string{"manual", "api.workflow.start"} {
		if !keys[want] {
			t.Errorf("expected system event %q to be present", want)
		}
	}
	// No model → no form/integration events
	for _, unwanted := range []string{"form.submit", "integration.import.completed"} {
		if keys[unwanted] {
			t.Errorf("did not expect %q without a model", unwanted)
		}
	}
}

func TestBuildCatalogFormAndIntegrationEvents(t *testing.T) {
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
	defer func() { _ = pgc.Terminate(ctx) }()

	dsn, _ := pgc.ConnectionString(ctx, "sslmode=disable")
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if err := migrate.Run(ctx, pool, catalogTestMigrations, "testdata"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	mustScan := func(err error, label string) {
		t.Helper()
		if err != nil {
			t.Fatalf("insert %s: %v", label, err)
		}
	}

	var custID, wsID, appID, modelID string
	mustScan(pool.QueryRow(ctx, `INSERT INTO core.customer (name) VALUES ('ACME') RETURNING id::text`).Scan(&custID), "customer")
	mustScan(pool.QueryRow(ctx, `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID).Scan(&wsID), "workspace")
	mustScan(pool.QueryRow(ctx, `INSERT INTO core.application (workspace_id, name) VALUES ($1::uuid, 'App') RETURNING id::text`, wsID).Scan(&appID), "application")
	mustScan(pool.QueryRow(ctx, `INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, appID).Scan(&modelID), "model")

	// Form with typed fields
	if _, err := pool.Exec(ctx, `
		INSERT INTO model.form_def (model_id, name, label, fields) VALUES ($1::uuid, 'expense_claim', 'Expense Claim',
		'[{"name":"amount","type":"number","required":true},{"name":"note","type":"text","required":false}]')
	`, modelID); err != nil {
		t.Fatalf("insert form_def: %v", err)
	}

	// Integration
	if _, err := pool.Exec(ctx, `
		INSERT INTO model.integration_def (model_id, name, type) VALUES ($1::uuid, 'Actuals CSV', 'csv_import')
	`, modelID); err != nil {
		t.Fatalf("insert integration_def: %v", err)
	}

	catalog, err := buildWorkflowTriggerEventCatalog(ctx, pool, appID, "")
	if err != nil {
		t.Fatalf("buildWorkflowTriggerEventCatalog: %v", err)
	}

	byKey := make(map[string]TriggerEventCatalogItem, len(catalog))
	for _, item := range catalog {
		byKey[item.Key] = item
	}

	// Generic form.submit present
	if _, ok := byKey["form.submit"]; !ok {
		t.Error("expected generic form.submit event")
	}

	// Specific form event
	formEv, ok := byKey["expense_claim.submitted"]
	if !ok {
		t.Fatal("expected expense_claim.submitted event")
	}
	if formEv.Category != "form" {
		t.Errorf("category = %q, want form", formEv.Category)
	}
	if formEv.SourceName != "Expense Claim" {
		t.Errorf("source_name = %q, want Expense Claim", formEv.SourceName)
	}
	// payload: record_id, submitted_by_user_id, amount, note
	if len(formEv.PayloadSchema) != 4 {
		t.Errorf("payload fields = %d, want 4", len(formEv.PayloadSchema))
	}
	payloadByKey := make(map[string]TriggerPayloadField)
	for _, f := range formEv.PayloadSchema {
		payloadByKey[f.Key] = f
	}
	if payloadByKey["amount"].Type != "number" {
		t.Errorf("amount type = %q, want number", payloadByKey["amount"].Type)
	}
	if !payloadByKey["amount"].Required {
		t.Error("amount should be required")
	}
	if payloadByKey["note"].Required {
		t.Error("note should not be required")
	}

	// Integration events
	completedEv, ok := byKey["actuals_csv.import.completed"]
	if !ok {
		t.Fatal("expected actuals_csv.import.completed event")
	}
	if completedEv.SourceName != "Actuals CSV" {
		t.Errorf("source_name = %q, want Actuals CSV", completedEv.SourceName)
	}
	if completedEv.Category != "integration" {
		t.Errorf("category = %q, want integration", completedEv.Category)
	}

	if _, ok := byKey["actuals_csv.import.failed"]; !ok {
		t.Error("expected actuals_csv.import.failed event")
	}

	// Planning events were static placeholders nothing ever dispatched; a
	// rule bound to one silently never fired. They stay out of the catalog
	// until something emits them.
	for _, pk := range []string{"budget.submitted", "forecast.submitted", "grid.review_requested"} {
		if _, ok := byKey[pk]; ok {
			t.Errorf("planning placeholder %q is still advertised although nothing dispatches it", pk)
		}
	}
}

func TestBuildCatalogEventKeySlug(t *testing.T) {
	cases := []struct{ input, want string }{
		{"Purchase Request", "purchase_request"},
		{"actuals-csv", "actuals_csv"},
		{"My Integration", "my_integration"},
	}
	for _, c := range cases {
		got := toEventKey(c.input)
		if got != c.want {
			t.Errorf("toEventKey(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestFormFieldTypeToPayloadType(t *testing.T) {
	cases := []struct{ in, out string }{
		{"number", "number"},
		{"boolean", "boolean"},
		{"date", "date"},
		{"dimension", "dimension_member"},
		{"metric", "metric"},
		{"text", "text"},
		{"select", "text"},
		{"unknown", "text"},
	}
	for _, c := range cases {
		got := formFieldTypeToPayloadType(c.in)
		if got != c.out {
			t.Errorf("formFieldTypeToPayloadType(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}
