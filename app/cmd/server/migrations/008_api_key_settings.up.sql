-- Move api key settings into a dedicated table for clean separation of concerns.
-- api_key_id is the PK (enforces 1:1) and also serves as the FK index.
CREATE TABLE IF NOT EXISTS api_key_settings (
    api_key_id  BIGINT  PRIMARY KEY REFERENCES api_keys(id) ON DELETE CASCADE,
    settings    JSONB   NOT NULL DEFAULT '{}'
);

-- Migrate existing settings data.
INSERT INTO api_key_settings (api_key_id, settings)
SELECT id, settings
FROM api_keys
ON CONFLICT (api_key_id) DO NOTHING;

ALTER TABLE api_keys DROP COLUMN IF EXISTS settings;
