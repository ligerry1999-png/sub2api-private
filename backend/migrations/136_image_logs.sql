-- Store successful image generation records for admin review.
CREATE TABLE IF NOT EXISTS image_logs (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    account_id BIGINT REFERENCES accounts(id) ON DELETE SET NULL,
    group_id BIGINT REFERENCES groups(id) ON DELETE SET NULL,
    request_id VARCHAR(128) NOT NULL DEFAULT '',
    source VARCHAR(32) NOT NULL DEFAULT 'sub2api',
    endpoint VARCHAR(64) NOT NULL DEFAULT '',
    model VARCHAR(100) NOT NULL DEFAULT '',
    prompt TEXT NOT NULL DEFAULT '',
    status VARCHAR(32) NOT NULL DEFAULT 'success',
    error_message TEXT,
    image_count INTEGER NOT NULL DEFAULT 0,
    image_size VARCHAR(32),
    duration_ms INTEGER,
    images JSONB NOT NULL DEFAULT '[]'::jsonb,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_image_logs_created_at ON image_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_image_logs_user_created_at ON image_logs(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_image_logs_api_key_created_at ON image_logs(api_key_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_image_logs_account_created_at ON image_logs(account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_image_logs_request_id ON image_logs(request_id);
CREATE INDEX IF NOT EXISTS idx_image_logs_source ON image_logs(source);
