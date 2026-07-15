CREATE TABLE IF NOT EXISTS account_import_snapshots (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL UNIQUE REFERENCES accounts(id) ON DELETE CASCADE,
    batch_id VARCHAR(64) NOT NULL,
    encrypted_json TEXT NOT NULL,
    imported_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS account_import_snapshots_imported_at_idx
    ON account_import_snapshots(imported_at);
