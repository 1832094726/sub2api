CREATE TABLE IF NOT EXISTS image_primary_tasks (
    id BIGSERIAL PRIMARY KEY,
    public_id VARCHAR(64) NOT NULL UNIQUE,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    usage_log_id BIGINT NULL REFERENCES usage_logs(id) ON DELETE SET NULL,
    protocol VARCHAR(32) NOT NULL,
    model VARCHAR(128) NOT NULL,
    request_hash VARCHAR(64) NOT NULL,
    upstream_task_id VARCHAR(128),
    status VARCHAR(16) NOT NULL DEFAULT 'queued',
    fallback_reason VARCHAR(128),
    result_locator TEXT,
    image_count INTEGER NOT NULL DEFAULT 0,
    image_size VARCHAR(32),
    primary_duration_ms BIGINT NOT NULL DEFAULT 0,
    fallback_duration_ms BIGINT NOT NULL DEFAULT 0,
    settlement_state VARCHAR(16) NOT NULL DEFAULT 'pending',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT image_primary_tasks_status_check CHECK (status IN ('queued', 'running', 'success', 'error')),
    CONSTRAINT image_primary_tasks_settlement_check CHECK (settlement_state IN ('pending', 'claimed', 'settled'))
);

CREATE INDEX IF NOT EXISTS image_primary_tasks_owner_idx
    ON image_primary_tasks(api_key_id, public_id);
CREATE INDEX IF NOT EXISTS image_primary_tasks_status_updated_idx
    ON image_primary_tasks(status, updated_at);
CREATE INDEX IF NOT EXISTS image_primary_tasks_expires_idx
    ON image_primary_tasks(expires_at);

ALTER TABLE usage_logs ALTER COLUMN account_id DROP NOT NULL;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_channel VARCHAR(32);
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS primary_task_id VARCHAR(64);
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS primary_duration_ms INTEGER;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS fallback_reason VARCHAR(64);
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS fallback_duration_ms INTEGER;

CREATE INDEX IF NOT EXISTS usage_logs_primary_task_id_idx
    ON usage_logs(primary_task_id)
    WHERE primary_task_id IS NOT NULL;
