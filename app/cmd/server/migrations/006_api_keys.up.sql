CREATE TABLE IF NOT EXISTS api_keys (
    id           BIGSERIAL    PRIMARY KEY,
    token        TEXT         NOT NULL UNIQUE,
    label        TEXT         NOT NULL DEFAULT '',
    settings     JSONB        NOT NULL DEFAULT '{}',
    is_active    BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_api_keys_token
    ON api_keys (token)
    WHERE is_active = TRUE;
