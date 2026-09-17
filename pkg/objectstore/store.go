package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Record is one storage.object row.
type Record struct {
	ID             string
	Bucket         string
	ObjectKey      string
	ContentType    string
	SizeBytes      int64
	ChecksumSHA256 string
	OwnerType      string
	OwnerID        string
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// MetaStore persists storage.object rows — one per (owner_type, owner_id):
// a second Upsert for the same owner updates the existing row in place
// rather than accumulating history. "Retained artifact" means "the
// current one is always durably available," not an unbounded archive.
type MetaStore struct {
	pool *pgxpool.Pool
}

func NewMetaStore(pool *pgxpool.Pool) *MetaStore { return &MetaStore{pool: pool} }

// Upsert inserts or, for an existing (ownerType, ownerID), updates the row.
func (s *MetaStore) Upsert(ctx context.Context, bucket, objectKey, contentType string, sizeBytes int64, checksum, ownerType, ownerID, createdBy string) (Record, error) {
	var r Record
	err := s.pool.QueryRow(ctx, `
		INSERT INTO storage.object (bucket, object_key, content_type, size_bytes, checksum_sha256, owner_type, owner_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8,'')::uuid)
		ON CONFLICT (owner_type, owner_id) DO UPDATE SET
			bucket          = EXCLUDED.bucket,
			object_key      = EXCLUDED.object_key,
			content_type    = EXCLUDED.content_type,
			size_bytes      = EXCLUDED.size_bytes,
			checksum_sha256 = EXCLUDED.checksum_sha256,
			created_by      = EXCLUDED.created_by,
			updated_at      = now()
		RETURNING id::text, bucket, object_key, content_type, size_bytes, checksum_sha256,
		          owner_type, owner_id, COALESCE(created_by::text,''), created_at, updated_at
	`, bucket, objectKey, contentType, sizeBytes, checksum, ownerType, ownerID, createdBy).Scan(
		&r.ID, &r.Bucket, &r.ObjectKey, &r.ContentType, &r.SizeBytes, &r.ChecksumSHA256,
		&r.OwnerType, &r.OwnerID, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return Record{}, fmt.Errorf("objectstore: upsert metadata: %w", err)
	}
	return r, nil
}

// Get returns the record for (ownerType, ownerID), or ErrNotFound.
func (s *MetaStore) Get(ctx context.Context, ownerType, ownerID string) (Record, error) {
	var r Record
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, bucket, object_key, content_type, size_bytes, checksum_sha256,
		       owner_type, owner_id, COALESCE(created_by::text,''), created_at, updated_at
		FROM storage.object WHERE owner_type=$1 AND owner_id=$2
	`, ownerType, ownerID).Scan(
		&r.ID, &r.Bucket, &r.ObjectKey, &r.ContentType, &r.SizeBytes, &r.ChecksumSHA256,
		&r.OwnerType, &r.OwnerID, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("objectstore: get metadata: %w", err)
	}
	return r, nil
}

// Delete removes the record for (ownerType, ownerID). Not an error if none exists.
func (s *MetaStore) Delete(ctx context.Context, ownerType, ownerID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM storage.object WHERE owner_type=$1 AND owner_id=$2`, ownerType, ownerID); err != nil {
		return fmt.Errorf("objectstore: delete metadata: %w", err)
	}
	return nil
}

// blobPutter/blobDeleter/metaUpserter are the minimal interfaces save
// needs — small enough that the compensating-delete path (see Save's doc
// comment) is unit-testable with a fake metaUpserter, without needing to
// fabricate a real Postgres failure.
type blobPutter interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
}
type blobDeleter interface {
	Delete(ctx context.Context, key string) error
}
type metaUpserter interface {
	Upsert(ctx context.Context, bucket, objectKey, contentType string, sizeBytes int64, checksum, ownerType, ownerID, createdBy string) (Record, error)
}

// Store composes a blob Client and a MetaStore into one Save operation —
// PostgreSQL holds metadata/references, the blob client holds the payload.
type Store struct {
	blob *Client
	meta *MetaStore
}

func NewStore(blob *Client, meta *MetaStore) *Store {
	return &Store{blob: blob, meta: meta}
}

// Save uploads data as the object for (ownerType, ownerID) under key,
// overwriting any previous object/record for that same owner, and records
// its metadata in Postgres.
//
// Not a true transaction — the blob store and Postgres can't share one —
// but deliberately ordered so a partial failure fails toward "nothing
// retained" rather than "a dangling reference that 404s on read": if the
// metadata upsert fails after the blob upload succeeded, Save best-effort
// deletes the just-uploaded blob before returning the error. An orphaned
// blob (if the compensating delete itself also fails) is a harmless space
// leak that self-heals on the next successful Save for the same owner
// (the object key is deterministic per owner, so a later Put overwrites
// it); a metadata row pointing at nothing would be a real correctness bug
// for any caller trusting it.
func (s *Store) Save(ctx context.Context, ownerType, ownerID, key string, data []byte, contentType, createdBy string) (Record, error) {
	return save(ctx, s.blob, s.blob, s.meta, s.blob.Bucket(), ownerType, ownerID, key, data, contentType, createdBy)
}

func save(ctx context.Context, putter blobPutter, deleter blobDeleter, meta metaUpserter,
	bucket, ownerType, ownerID, key string, data []byte, contentType, createdBy string,
) (Record, error) {
	if err := putter.Put(ctx, key, data, contentType); err != nil {
		return Record{}, err
	}
	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])
	rec, err := meta.Upsert(ctx, bucket, key, contentType, int64(len(data)), checksum, ownerType, ownerID, createdBy)
	if err != nil {
		if delErr := deleter.Delete(ctx, key); delErr != nil {
			return Record{}, fmt.Errorf("save metadata failed (%w) and compensating blob delete also failed: %v", err, delErr) //nolint:errorlint
		}
		return Record{}, fmt.Errorf("save metadata failed, blob rolled back: %w", err)
	}
	return rec, nil
}
