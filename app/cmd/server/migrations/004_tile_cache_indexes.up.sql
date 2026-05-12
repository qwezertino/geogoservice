-- 004_tile_cache_indexes.sql
-- Additional indexes for tile_cache to support efficient range queries by
-- polygon_hash and date, which are used by ListJobTiles (GET /results).

-- Index for ListJobTiles: filters by polygon_hash + date range.
-- Partial index (polygon_hash != '') keeps it small — plain tiles don't need it.
CREATE INDEX IF NOT EXISTS idx_tile_cache_polygon_date
    ON tile_cache (polygon_hash, date_acquired)
    WHERE polygon_hash != '';

-- Full-table index for date-range queries regardless of polygon
-- (used by catalog / future timeline endpoints).
CREATE INDEX IF NOT EXISTS idx_tile_cache_date
    ON tile_cache (date_acquired);
