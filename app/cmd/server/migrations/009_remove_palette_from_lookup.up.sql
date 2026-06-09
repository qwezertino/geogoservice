-- Palette is now applied at serve time from raw float32 data, not baked into
-- the cached PNG. Remove palette_hash from the tile_cache unique index so each
-- geographic tile is stored exactly once regardless of which palette was active
-- when it was first rendered.

-- Keep one row per tile geometry (lowest id = earliest render wins).
DELETE FROM tile_cache
WHERE id NOT IN (
    SELECT MIN(id)
    FROM tile_cache
    GROUP BY bbox_3857_minx, bbox_3857_miny, bbox_3857_maxx, bbox_3857_maxy,
             date_acquired, index_type, width, height, polygon_hash
);

-- Replace the palette-aware index with a geometry-only one.
DROP INDEX IF EXISTS idx_tile_cache_lookup;

CREATE UNIQUE INDEX idx_tile_cache_lookup
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
