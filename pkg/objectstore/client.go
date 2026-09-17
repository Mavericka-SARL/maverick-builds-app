// Package objectstore is the one S3-compatible blob abstraction this
// codebase uses: a thin Client wrapping minio-go/v7 (MinIO in dev/k8s,
// any S3-compatible endpoint in production — nothing here is MinIO-
// specific beyond the wire protocol) plus a MetaStore recording what's
// stored in PostgreSQL, composed by Store.Save. PostgreSQL holds only
// metadata/references; payload bytes live in the blob store.
package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ErrNotFound is returned by Client.Get (and MetaStore.Get) when the
// requested object/record doesn't exist — distinguishable from a generic
// connectivity/permission error, the same way writeguard.HiddenAccess
// distinguishes pgx.ErrNoRows from a real DB error.
var ErrNotFound = errors.New("objectstore: not found")

// Client is a thin wrapper over minio-go/v7, scoped to one bucket.
type Client struct {
	mc     *minio.Client
	bucket string
}

// NewClient constructs a Client against an S3-compatible endpoint. endpoint
// is host:port with no scheme (e.g. "minio:9000" or "s3.amazonaws.com").
func NewClient(endpoint, accessKey, secretKey, bucket string, useSSL bool) (*Client, error) {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: new client: %w", err)
	}
	return &Client{mc: mc, bucket: bucket}, nil
}

// Bucket returns the bucket this client is scoped to.
func (c *Client) Bucket() string { return c.bucket }

// EnsureBucket creates the configured bucket if it doesn't already exist.
// Idempotent — safe to call on every boot.
func (c *Client) EnsureBucket(ctx context.Context) error {
	exists, err := c.mc.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("objectstore: bucket exists check: %w", err)
	}
	if exists {
		return nil
	}
	if err := c.mc.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{}); err != nil {
		return fmt.Errorf("objectstore: make bucket: %w", err)
	}
	return nil
}

// Put uploads data under key, overwriting any existing object at that key.
func (c *Client) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := c.mc.PutObject(ctx, c.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("objectstore: put %s: %w", key, err)
	}
	return nil
}

// Get downloads the object at key. Returns ErrNotFound if it doesn't exist.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := c.mc.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("objectstore: get %s: %w", key, err)
	}
	defer obj.Close() //nolint:errcheck
	// GetObject is lazy — errors (including "doesn't exist") only surface
	// on Stat()/Read(), never on the call above.
	if _, err := obj.Stat(); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("objectstore: stat %s: %w", key, err)
	}
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("objectstore: read %s: %w", key, err)
	}
	return data, nil
}

// Delete removes the object at key. Not an error if it doesn't exist
// (matches S3/MinIO's own delete semantics).
func (c *Client) Delete(ctx context.Context, key string) error {
	if err := c.mc.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("objectstore: delete %s: %w", key, err)
	}
	return nil
}
