package objectstore_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/mavericks-engine/mavericks/pkg/objectstore"
)

func startMinioClient(t *testing.T, bucket string) *objectstore.Client {
	t.Helper()
	ctx := context.Background()

	ctr, err := // quay.io, not Docker Hub: the minio/minio repository there was
		// withdrawn on 2026-09-11 and every other MinIO reference in this
		// repository already moved. A machine with the old image cached
		// keeps passing, which is why this survived until a clean runner
		// tried to pull it.
		tcminio.Run(ctx, "quay.io/minio/minio:RELEASE.2024-01-16T16-07-38Z")
	if err != nil {
		t.Fatalf("start minio: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	endpoint, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	client, err := objectstore.NewClient(endpoint, ctr.Username, ctr.Password, bucket, false)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	return client
}

func TestClient_EnsureBucketIsIdempotent(t *testing.T) {
	// startMinioClient already calls EnsureBucket once during setup; a
	// second call here must not error.
	client := startMinioClient(t, "mavericks-test")
	if err := client.EnsureBucket(context.Background()); err != nil {
		t.Fatalf("ensure bucket (2nd call, must be idempotent): %v", err)
	}
}

func TestClient_PutGetRoundTrip(t *testing.T) {
	client := startMinioClient(t, "mavericks-test")
	ctx := context.Background()

	data := []byte("hello object storage")
	if err := client.Put(ctx, "some/key.txt", data, "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := client.Get(ctx, "some/key.txt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("get returned %q, want %q", got, data)
	}
}

func TestClient_GetMissingKeyReturnsErrNotFound(t *testing.T) {
	client := startMinioClient(t, "mavericks-test")
	ctx := context.Background()

	_, err := client.Get(ctx, "does/not/exist.txt")
	if !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("get missing key: err = %v, want objectstore.ErrNotFound", err)
	}
}

func TestClient_Delete(t *testing.T) {
	client := startMinioClient(t, "mavericks-test")
	ctx := context.Background()

	if err := client.Put(ctx, "to-delete.txt", []byte("x"), "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := client.Delete(ctx, "to-delete.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := client.Get(ctx, "to-delete.txt"); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("get after delete: err = %v, want objectstore.ErrNotFound", err)
	}
}

func TestClient_PutOverwritesExistingKey(t *testing.T) {
	client := startMinioClient(t, "mavericks-test")
	ctx := context.Background()

	if err := client.Put(ctx, "k.txt", []byte("v1"), "text/plain"); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	if err := client.Put(ctx, "k.txt", []byte("v2"), "text/plain"); err != nil {
		t.Fatalf("put v2: %v", err)
	}
	got, err := client.Get(ctx, "k.txt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != "v2" {
		t.Errorf("got %q, want v2 (overwrite, not accumulation)", got)
	}
}
