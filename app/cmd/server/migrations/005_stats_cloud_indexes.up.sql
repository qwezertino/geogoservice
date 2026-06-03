-- 005_stats_cloud_indexes.up.sql
-- Enriches tile_cache with per-tile statistics and cloud cover so that the
-- GET /api/jobs/{id}/results endpoint can return rich metadata without extra
-- STAC lookups. Adds an indexes column to render_jobs so a single async job
-- can render multiple spectral indexes (ndvi, evi, gndvi, cvi, tci, soilmoisture).

-- Per-tile spectral index statistics (min/max/mean + 20-bucket histogram).
-- NULL for tiles rendered before this migration or for TCI (true-colour).
ALTER TABLE tile_cache
    ADD COLUMN IF NOT EXISTS stats JSONB,
    ADD COLUMN IF NOT EXISTS cloud DOUBLE PRECISION;

-- Ordered list of spectral indexes to render per scene in a job.
-- Defaults to ["ndvi"] so existing rows (pre-migration) behave identically.
ALTER TABLE render_jobs
    ADD COLUMN IF NOT EXISTS indexes TEXT[] NOT NULL DEFAULT '{"ndvi"}';
