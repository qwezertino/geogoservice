-- 002_add_polygon_hash.sql
-- Adds polygon_hash column to tile_cache so that polygon-masked tiles can be
-- stored and looked up separately from plain (full-bbox) tiles.
--
-- Safe to run on an existing database: both the ALTER and the index creation
-- use IF NOT EXISTS / IF EXISTS guards.

ALTER TABLE tile_cache
    ADD COLUMN IF NOT EXISTS polygon_hash VARCHAR(16) NOT NULL DEFAULT '';

-- Replace the old unique index (which did not include polygon_hash) with one
-- that does. The old index is dropped first to avoid a naming conflict.
DROP INDEX IF EXISTS idx_tile_cache_lookup;

CREATE UNIQUE INDEX IF NOT EXISTS idx_tile_cache_lookup
    ON tile_cache (
        bbox_3857_minx,
        bbox_3857_miny,
        bbox_3857_maxx,
        bbox_3857_maxy,
        date_acquired,
        index_type,
        width,
        height,
        polygon_hash
    );
