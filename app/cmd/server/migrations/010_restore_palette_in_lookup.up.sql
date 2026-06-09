-- Palette is baked into the stored PNG: different palette = different cached tile.
-- Restore palette_hash as part of the unique index so jobs re-render when the
-- user's palette changes.

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
        polygon_hash,
        palette_hash
    );
