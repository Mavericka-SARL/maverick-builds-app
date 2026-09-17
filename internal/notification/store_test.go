package notification_test

import (
	"context"
	"embed"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	notificationv1 "github.com/mavericks-engine/mavericks/gen/go/notification/v1"
	"github.com/mavericks-engine/mavericks/internal/notification"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

func setupDB(t *testing.T) (*notification.Store, func()) {
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

	return notification.NewStore(pool), func() {
		pool.Close()
		_ = pgc.Terminate(ctx)
	}
}

func insertUser(t *testing.T, store *notification.Store) string {
	t.Helper()
	var id string
	err := store.Pool().QueryRow(context.Background(),
		`INSERT INTO identity.user (email) VALUES (gen_random_uuid()::text || '@test.example') RETURNING id::text`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func TestSend_InApp_DeliveredImmediately(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID := insertUser(t, store)

	id, err := store.Send(ctx, userID, "welcome", notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_IN_APP,
		map[string]string{"name": "Alice"}, "", "")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty notification ID")
	}

	n, err := store.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if n.Status != notificationv1.NotificationStatus_NOTIFICATION_STATUS_DELIVERED {
		t.Errorf("status = %v, want DELIVERED", n.Status)
	}
	if n.DeliveredAt == nil {
		t.Error("delivered_at is nil for in_app notification")
	}
	if n.TemplateVars["name"] != "Alice" {
		t.Errorf("template_vars[name] = %q, want Alice", n.TemplateVars["name"])
	}
}

func TestSend_Email_StaysPending(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID := insertUser(t, store)

	id, err := store.Send(ctx, userID, "budget_approved", notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL,
		map[string]string{"amount": "50000"}, "", "")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	n, err := store.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if n.Status != notificationv1.NotificationStatus_NOTIFICATION_STATUS_PENDING {
		t.Errorf("status = %v, want PENDING", n.Status)
	}
	if n.Channel != notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL {
		t.Errorf("channel = %v, want EMAIL", n.Channel)
	}
}

func TestList(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID := insertUser(t, store)

	for i := 0; i < 3; i++ {
		if _, err := store.Send(ctx, userID, "tmpl", notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_IN_APP, nil, "", ""); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	notifs, err := store.List(ctx, userID, false, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(notifs) != 3 {
		t.Errorf("len(notifs) = %d, want 3", len(notifs))
	}
}

func TestList_UnreadOnly(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID := insertUser(t, store)

	// Send 3 in_app (auto-delivered), then mark 1 as read
	ids := make([]string, 3)
	for i := range ids {
		id, err := store.Send(ctx, userID, "tmpl", notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_IN_APP, nil, "", "")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		ids[i] = id
	}

	if _, err := store.MarkRead(ctx, []string{ids[0]}); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}

	notifs, err := store.List(ctx, userID, true, 10)
	if err != nil {
		t.Fatalf("List(unreadOnly): %v", err)
	}
	if len(notifs) != 2 {
		t.Errorf("len(notifs) = %d, want 2 after marking one read", len(notifs))
	}
}

func TestMarkRead(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID := insertUser(t, store)

	id1, _ := store.Send(ctx, userID, "t1", notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_IN_APP, nil, "", "")
	id2, _ := store.Send(ctx, userID, "t2", notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_IN_APP, nil, "", "")

	updated, err := store.MarkRead(ctx, []string{id1, id2})
	if err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if updated != 2 {
		t.Errorf("updated = %d, want 2", updated)
	}

	// Second MarkRead should update 0 rows (already read)
	updated2, err := store.MarkRead(ctx, []string{id1, id2})
	if err != nil {
		t.Fatalf("MarkRead (idempotent): %v", err)
	}
	if updated2 != 0 {
		t.Errorf("second MarkRead updated = %d, want 0", updated2)
	}
}

func TestMarkRead_Empty(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()

	updated, err := store.MarkRead(context.Background(), nil)
	if err != nil {
		t.Fatalf("MarkRead(nil): %v", err)
	}
	if updated != 0 {
		t.Errorf("updated = %d, want 0 for empty slice", updated)
	}
}

func TestGetByID_NotFound(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()

	_, err := store.GetByID(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Error("expected error for unknown notification ID, got nil")
	}
}
