package aiassistant_test

import (
	"context"
	"embed"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	aiassistantv1 "github.com/mavericks-engine/mavericks/gen/go/aiassistant/v1"
	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

func setupDB(t *testing.T) (*aiassistant.Store, func()) {
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

	return aiassistant.NewStore(pool), func() {
		pool.Close()
		_ = pgc.Terminate(ctx)
	}
}

func insertFixtures(t *testing.T, store *aiassistant.Store) (appID, userID string) {
	t.Helper()
	ctx := context.Background()
	pool := store.Pool()

	if err := pool.QueryRow(ctx, `INSERT INTO identity.user (email) VALUES ('dev@test.com') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	var custID string
	if err := pool.QueryRow(ctx, `INSERT INTO core.customer (name) VALUES ('Acme') RETURNING id::text`).Scan(&custID); err != nil {
		t.Fatalf("insert customer: %v", err)
	}

	var wsID string
	if err := pool.QueryRow(ctx, `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'WS') RETURNING id::text`, custID).Scan(&wsID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}

	if err := pool.QueryRow(ctx, `INSERT INTO core.application (workspace_id, name) VALUES ($1::uuid, 'App') RETURNING id::text`, wsID).Scan(&appID); err != nil {
		t.Fatalf("insert application: %v", err)
	}

	return appID, userID
}

func TestCreateAndGetSession(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	appID, userID := insertFixtures(t, store)

	sess, err := store.CreateSession(ctx, appID, userID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.Id == "" {
		t.Fatal("expected non-empty session ID")
	}
	if sess.ApplicationId != appID {
		t.Fatalf("ApplicationId mismatch: %s", sess.ApplicationId)
	}

	got, actions, err := store.GetSession(ctx, sess.Id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Id != sess.Id {
		t.Fatalf("ID mismatch")
	}
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions, got %d", len(actions))
	}
}

func TestCreateAction(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	appID, userID := insertFixtures(t, store)

	sess, err := store.CreateSession(ctx, appID, userID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	diffs := []*aiassistantv1.FileDiff{
		{Path: "metrics/revenue.yaml", Before: "", After: "name: revenue", Diff: "+name: revenue"},
	}
	impact := &aiassistantv1.ImpactSummary{
		AffectedMetrics: []string{"revenue"},
	}

	action, err := store.CreateAction(ctx, sess.Id, aiassistantv1.ActionType_ACTION_TYPE_ADD_METRIC, "add revenue metric", diffs, impact)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if action.Id == "" {
		t.Fatal("expected non-empty action ID")
	}
	if action.ActionType != aiassistantv1.ActionType_ACTION_TYPE_ADD_METRIC {
		t.Fatalf("ActionType mismatch: %v", action.ActionType)
	}

	// GetSession should now return the action.
	_, actions, err := store.GetSession(ctx, sess.Id)
	if err != nil {
		t.Fatalf("GetSession after CreateAction: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Prompt != "add revenue metric" {
		t.Fatalf("Prompt mismatch: %q", actions[0].Prompt)
	}
}

func TestGetAction(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	appID, userID := insertFixtures(t, store)

	sess, _ := store.CreateSession(ctx, appID, userID)
	action, err := store.CreateAction(ctx, sess.Id, aiassistantv1.ActionType_ACTION_TYPE_GENERATE_MIGRATION,
		"add users table", nil, nil)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	got, err := store.GetAction(ctx, action.Id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if got.Id != action.Id {
		t.Fatalf("ID mismatch")
	}
	if got.Applied {
		t.Fatal("expected Applied=false")
	}
}

func TestMarkApplied(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	appID, userID := insertFixtures(t, store)

	sess, _ := store.CreateSession(ctx, appID, userID)
	action, _ := store.CreateAction(ctx, sess.Id, aiassistantv1.ActionType_ACTION_TYPE_ADD_DIMENSION, "add region", nil, nil)

	if err := store.MarkApplied(ctx, action.Id, ""); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}

	got, err := store.GetAction(ctx, action.Id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if !got.Applied {
		t.Fatal("expected Applied=true after MarkApplied")
	}
}

func TestMarkRolledBack(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	appID, userID := insertFixtures(t, store)

	sess, _ := store.CreateSession(ctx, appID, userID)
	action, _ := store.CreateAction(ctx, sess.Id, aiassistantv1.ActionType_ACTION_TYPE_MODIFY_FORMULA, "fix formula", nil, nil)
	_ = store.MarkApplied(ctx, action.Id, "")

	if err := store.MarkRolledBack(ctx, action.Id); err != nil {
		t.Fatalf("MarkRolledBack: %v", err)
	}

	got, err := store.GetAction(ctx, action.Id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if got.Applied {
		t.Fatal("expected Applied=false after MarkRolledBack")
	}
}

func TestDiffRoundtrip(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	appID, userID := insertFixtures(t, store)

	sess, _ := store.CreateSession(ctx, appID, userID)

	diffs := []*aiassistantv1.FileDiff{
		{Path: "a.sql", Before: "old", After: "new", Diff: "@@ -1 +1 @@ -old +new"},
		{Path: "b.yaml", Before: "", After: "key: val", Diff: "+key: val"},
	}
	impact := &aiassistantv1.ImpactSummary{
		AffectedMetrics:    []string{"m1", "m2"},
		AffectedPolicies:   []string{"p1"},
		RequiredMigrations: []string{"add column x"},
		Warnings:           []string{"breaking change"},
	}

	action, err := store.CreateAction(ctx, sess.Id, aiassistantv1.ActionType_ACTION_TYPE_MODIFY_POLICY, "prompt", diffs, impact)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	got, err := store.GetAction(ctx, action.Id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if len(got.Diffs) != 2 {
		t.Fatalf("expected 2 diffs, got %d", len(got.Diffs))
	}
	if got.Diffs[0].Path != "a.sql" {
		t.Fatalf("Diffs[0].Path = %q", got.Diffs[0].Path)
	}
	if got.Impact == nil || len(got.Impact.AffectedMetrics) != 2 {
		t.Fatalf("ImpactSummary not round-tripped correctly")
	}
	if len(got.Impact.Warnings) != 1 || got.Impact.Warnings[0] != "breaking change" {
		t.Fatalf("Warnings not round-tripped: %v", got.Impact.Warnings)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	_, _, err := store.GetSession(ctx, "00000000-0000-0000-0000-000000000001")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}
