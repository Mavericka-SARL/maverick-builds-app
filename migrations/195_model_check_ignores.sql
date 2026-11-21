-- The model check (2026-11-21, internal/modelcheck): a finding a developer
-- has looked at and decided to keep is ignored with a reason, per revision.
-- The key names the finding by what it is about — rule, metric, dimension,
-- dashboard names — never by ids, so a revision copy and a model export
-- carry the ignores over unchanged.
CREATE TABLE IF NOT EXISTS model.check_ignore (
    revision_id UUID        NOT NULL REFERENCES model.revision(id) ON DELETE CASCADE,
    key         TEXT        NOT NULL,
    rule        TEXT        NOT NULL,
    reason      TEXT        NOT NULL CHECK (btrim(reason) <> ''),
    ignored_by  UUID        REFERENCES identity.user(id) ON DELETE SET NULL,
    ignored_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (revision_id, key)
);
