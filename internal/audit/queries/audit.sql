-- name: InsertAuditEvent :one
INSERT INTO audit.audit_event
    (category, event_type, actor_user_id, actor_role, workspace_id,
     resource_type, resource_id, metadata, before_state, after_state)
VALUES
    ($1::audit.event_category, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id;

-- name: QueryAuditEvents :many
SELECT id, category::text, event_type, actor_user_id, actor_role, workspace_id,
       resource_type, resource_id, metadata, occurred_at
FROM audit.audit_event
WHERE ($1::uuid IS NULL OR workspace_id = $1)
  AND ($2::text IS NULL OR category::text = $2)
  AND ($3::text IS NULL OR resource_type = $3)
  AND ($4::text IS NULL OR resource_id = $4)
  AND ($5::timestamptz IS NULL OR occurred_at >= $5)
  AND ($6::timestamptz IS NULL OR occurred_at <= $6)
ORDER BY occurred_at DESC
LIMIT $7 OFFSET $8;

-- name: PartitionExists :one
SELECT EXISTS(
    SELECT 1 FROM audit.partition_registry WHERE partition_name = $1
);

-- name: RegisterPartition :exec
INSERT INTO audit.partition_registry (partition_name, range_start, range_end)
VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;
