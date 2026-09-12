CREATE TABLE IF NOT EXISTS downstream_cyber_risk (
    user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    strikes INTEGER NOT NULL CHECK (strikes BETWEEN 1 AND 2),
    blocked_until TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS downstream_cyber_events (
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, request_id)
);
