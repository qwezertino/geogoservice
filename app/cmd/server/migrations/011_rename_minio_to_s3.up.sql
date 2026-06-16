-- Rename storage-vendor-specific column names to vendor-agnostic ones.
-- The backing object store is no longer MinIO (now SeaweedFS, S3-compatible).
ALTER TABLE tile_cache RENAME COLUMN minio_bucket TO s3_bucket;
ALTER TABLE tile_cache RENAME COLUMN minio_key TO s3_key;
