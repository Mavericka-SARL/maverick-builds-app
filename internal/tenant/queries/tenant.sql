-- name: GetCustomer :one
SELECT id, name, plan, created_at, updated_at
FROM core.customer WHERE id = $1;

-- name: ListCustomers :many
SELECT id, name, plan, created_at, updated_at
FROM core.customer ORDER BY name LIMIT $1 OFFSET $2;

-- name: CreateCustomer :one
INSERT INTO core.customer (name, plan)
VALUES ($1, $2) RETURNING *;

-- name: UpdateCustomer :one
UPDATE core.customer SET name = $2, plan = $3
WHERE id = $1 RETURNING *;

-- name: GetWorkspace :one
SELECT id, customer_id, name, settings, created_at, updated_at
FROM core.workspace WHERE id = $1;

-- name: ListWorkspacesByCustomer :many
SELECT id, customer_id, name, settings, created_at, updated_at
FROM core.workspace WHERE customer_id = $1 ORDER BY name;

-- name: CreateWorkspace :one
INSERT INTO core.workspace (customer_id, name, settings)
VALUES ($1, $2, $3) RETURNING *;

-- name: GetApplication :one
SELECT id, workspace_id, name, mode, status, created_at, updated_at
FROM core.application WHERE id = $1;

-- name: ListApplicationsByWorkspace :many
SELECT id, workspace_id, name, mode, status, created_at, updated_at
FROM core.application WHERE workspace_id = $1 ORDER BY name;

-- name: CreateApplication :one
INSERT INTO core.application (workspace_id, name, mode)
VALUES ($1, $2, $3) RETURNING *;

-- name: GetModel :one
SELECT id, application_id, name, storage_type, created_at, updated_at
FROM core.model WHERE id = $1;

-- name: ListModelsByApplication :many
SELECT id, application_id, name, storage_type, created_at, updated_at
FROM core.model WHERE application_id = $1 ORDER BY name;

-- name: CreateModel :one
INSERT INTO core.model (application_id, name, storage_type)
VALUES ($1, $2, $3) RETURNING *;

-- name: GetLatestRevision :one
SELECT id, model_id, version_number, schema_hash, published_at, created_at
FROM core.schema_version
WHERE model_id = $1
ORDER BY version_number DESC LIMIT 1;

-- name: CreateRevision :one
INSERT INTO core.schema_version (model_id, version_number, schema_hash)
VALUES ($1, $2, $3) RETURNING *;

-- name: PublishRevision :one
UPDATE core.schema_version SET published_at = now()
WHERE id = $1 RETURNING *;
