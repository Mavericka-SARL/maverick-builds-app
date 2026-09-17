-- Schema: storage
-- Metadata/reference table backing pkg/objectstore — the S3-compatible blob
-- abstraction (MinIO in dev/k8s, any S3-compatible endpoint in production).
-- PostgreSQL holds only metadata; the payload bytes live in the object
-- store. owner_type/owner_id is a polymorphic reference (TEXT, no FK) —
-- mirrors audit.audit_event's resource_type/resource_id and
-- notification.notification's resource_type/resource_id, both intentionally
-- FK-less since the owner's table varies by owner_type (see those
-- migrations' own comments for the same reasoning).
--
-- One row per (owner_type, owner_id): a second Save() for the same owner
-- updates the existing row's object_key/checksum in place rather than
-- accumulating history — "retained artifact" means "the current one is
-- always durably available," not an unbounded, unretentioned archive.

CREATE SCHEMA IF NOT EXISTS storage;

CREATE TABLE storage.object (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    bucket          TEXT NOT NULL,
    object_key      TEXT NOT NULL,
    content_type    TEXT NOT NULL DEFAULT '',
    size_bytes      BIGINT NOT NULL,
    checksum_sha256 TEXT NOT NULL,
    owner_type      TEXT NOT NULL,
    owner_id        TEXT NOT NULL,
    created_by      UUID REFERENCES identity.user(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (bucket, object_key),
    UNIQUE (owner_type, owner_id)
);

CREATE INDEX ON storage.object (owner_type, owner_id);
