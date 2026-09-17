package objectstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/objectstore"
)

func setupMetaStoreDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool := testdb.New(t, migrationfs.FS, ".")
	return pool
}

func TestMetaStore_UpsertInsertsThenUpdatesInPlace(t *testing.T) {
	pool := setupMetaStoreDB(t)
	meta := objectstore.NewMetaStore(pool)
	ctx := context.Background()

	first, err := meta.Upsert(ctx, "mavericks", "packages/model-1/rev-1.tar.gz", "application/gzip", 100, "sum1", "deployment_package", "rev-1", "")
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if first.ObjectKey != "packages/model-1/rev-1.tar.gz" {
		t.Errorf("first upsert object_key = %q", first.ObjectKey)
	}

	second, err := meta.Upsert(ctx, "mavericks", "packages/model-1/rev-1-v2.tar.gz", "application/gzip", 200, "sum2", "deployment_package", "rev-1", "")
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("second upsert created a NEW row (id %s != %s) — want update in place", second.ID, first.ID)
	}
	if second.ObjectKey != "packages/model-1/rev-1-v2.tar.gz" || second.SizeBytes != 200 || second.ChecksumSHA256 != "sum2" {
		t.Errorf("second upsert did not overwrite: %+v", second)
	}

	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM storage.object WHERE owner_type='deployment_package' AND owner_id='rev-1'`).Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("row count = %d, want 1 (upsert must not accumulate history)", rowCount)
	}
}

func TestMetaStore_GetReturnsErrNotFoundForMissingOwner(t *testing.T) {
	pool := setupMetaStoreDB(t)
	meta := objectstore.NewMetaStore(pool)

	_, err := meta.Get(context.Background(), "deployment_package", "does-not-exist")
	if !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("err = %v, want objectstore.ErrNotFound", err)
	}
}

func TestMetaStore_Delete(t *testing.T) {
	pool := setupMetaStoreDB(t)
	meta := objectstore.NewMetaStore(pool)
	ctx := context.Background()

	if _, err := meta.Upsert(ctx, "mavericks", "k", "text/plain", 1, "sum", "deployment_package", "rev-x", ""); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := meta.Delete(ctx, "deployment_package", "rev-x"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := meta.Get(ctx, "deployment_package", "rev-x"); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("get after delete: err = %v, want objectstore.ErrNotFound", err)
	}
}

func TestMetaStore_UpsertWithRealCreatedBy(t *testing.T) {
	pool := setupMetaStoreDB(t)
	ctx := context.Background()
	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email) VALUES ('objstore-test', 'objstore@test.dev') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	meta := objectstore.NewMetaStore(pool)
	rec, err := meta.Upsert(ctx, "mavericks", "k2", "text/plain", 1, "sum", "deployment_package", "rev-y", userID)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if rec.CreatedBy != userID {
		t.Errorf("created_by = %q, want %q", rec.CreatedBy, userID)
	}
}
