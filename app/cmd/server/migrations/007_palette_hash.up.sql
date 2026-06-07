-- Add palette_hash to tile_cache so tiles rendered with different palettes
-- get separate cache entries and don't collide in the unique index.
ALTER TABLE tile_cache ADD COLUMN IF NOT EXISTS palette_hash TEXT NOT NULL DEFAULT '';

-- Replace the old 9-column unique index with one that includes palette_hash.
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
        polygon_hash,
        palette_hash
    );

-- Store the palette hash in render_jobs so results can be correctly queried
-- from tile_cache (which now includes palette_hash in its unique key).
ALTER TABLE render_jobs ADD COLUMN IF NOT EXISTS palette_hash TEXT NOT NULL DEFAULT '';
