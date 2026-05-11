-- 003_render_jobs.sql
-- Stores asynchronous range-render jobs created via POST /api/jobs/render-range.
-- Each job renders all available Sentinel-2 scenes for a bbox+date range,
-- saving one NDVI PNG per acquisition date to MinIO/tile_cache.

CREATE TABLE IF NOT EXISTS render_jobs (
    id           TEXT            PRIMARY KEY,                   -- UUID as text
    status       TEXT            NOT NULL DEFAULT 'pending',    -- pending/running/done/failed
    bbox_minx    DOUBLE PRECISION NOT NULL,
    bbox_miny    DOUBLE PRECISION NOT NULL,
    bbox_maxx    DOUBLE PRECISION NOT NULL,
    bbox_maxy    DOUBLE PRECISION NOT NULL,
    start_date   DATE            NOT NULL,
    end_date     DATE            NOT NULL,
    max_cloud    DOUBLE PRECISION NOT NULL DEFAULT 20,
    w            INT             NOT NULL,
    h            INT             NOT NULL,
    polygon      JSONB           NOT NULL DEFAULT '[]',          -- [][2]float64 WGS-84 points
    polygon_hash TEXT            NOT NULL DEFAULT '',
    total        INT             NOT NULL DEFAULT 0,             -- scenes found
    done         INT             NOT NULL DEFAULT 0,             -- scenes rendered (success + error)
    errors       JSONB           NOT NULL DEFAULT '[]',          -- []string error messages
    created_at   TIMESTAMPTZ     NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_render_jobs_created_at ON render_jobs (created_at DESC);
