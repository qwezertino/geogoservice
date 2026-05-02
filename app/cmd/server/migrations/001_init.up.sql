-- Enable PostGIS extension
CREATE EXTENSION IF NOT EXISTS postgis;

-- Tile cache index table
CREATE TABLE IF NOT EXISTS tile_cache (
    id            BIGSERIAL PRIMARY KEY,
    bbox_geom     GEOMETRY(Polygon, 4326) NOT NULL,
    -- original EPSG:3857 bbox stored for fast exact-match lookup
    bbox_3857_minx DOUBLE PRECISION NOT NULL,
    bbox_3857_miny DOUBLE PRECISION NOT NULL,
    bbox_3857_maxx DOUBLE PRECISION NOT NULL,
    bbox_3857_maxy DOUBLE PRECISION NOT NULL,
    date_acquired DATE           NOT NULL,
    index_type    VARCHAR(32)    NOT NULL DEFAULT 'ndvi',
    width         INT            NOT NULL,
    height        INT            NOT NULL,
    minio_bucket  VARCHAR(255)   NOT NULL,
    minio_key     VARCHAR(512)   NOT NULL,
    created_at    TIMESTAMPTZ    NOT NULL DEFAULT NOW()
);

-- Spatial index on the WGS84 geometry for area queries
CREATE INDEX IF NOT EXISTS idx_tile_cache_geom
    ON tile_cache USING GIST (bbox_geom);

-- Composite index for exact-match cache lookups (the hot path)
CREATE UNIQUE INDEX IF NOT EXISTS idx_tile_cache_lookup
    ON tile_cache (
        bbox_3857_minx,
        bbox_3857_miny,
        bbox_3857_maxx,
        bbox_3857_maxy,
        date_acquired,
        index_type,
        width,
        height
    );
