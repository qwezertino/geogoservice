// Package cache manages the PostgreSQL tile index and MinIO object storage.
package cache

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/qwezert/geogoservice/internal/config"
	"github.com/qwezert/geogoservice/internal/geo"
)

// Store wraps the PostgreSQL connection pool and MinIO client.
type Store struct {
	db     *pgxpool.Pool
	minio  *minio.Client
	bucket string
}

// New connects to PostgreSQL and MinIO using the provided config.
func New(ctx context.Context, cfg *config.Config) (*Store, error) {
	pool, err := pgxpool.New(ctx, cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	mc, err := minio.New(cfg.MinioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.MinioAccessKey, cfg.MinioSecretKey, ""),
		Secure: cfg.MinioUseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("create minio client: %w", err)
	}

	return &Store{db: pool, minio: mc, bucket: cfg.MinioBucket}, nil
}

// Close releases the database connection pool.
func (s *Store) Close() {
	s.db.Close()
}

// LookupResult is returned by Lookup when a cached tile is found.
type LookupResult struct {
	MinioKey string
}

// Lookup checks whether a tile for the given parameters already exists in the
// cache. Returns (result, true, nil) on a hit, (nil, false, nil) on a miss,
// or (nil, false, err) on a database error.
func (s *Store) Lookup(ctx context.Context, bbox geo.BBox, date string, indexType string, w, h int) (*LookupResult, bool, error) {
	const q = `
		SELECT minio_key
		FROM   tile_cache
		WHERE  bbox_3857_minx = $1
		  AND  bbox_3857_miny = $2
		  AND  bbox_3857_maxx = $3
		  AND  bbox_3857_maxy = $4
		  AND  date_acquired  = $5
		  AND  index_type     = $6
		  AND  width          = $7
		  AND  height         = $8
		LIMIT 1`

	var key string
	err := s.db.QueryRow(ctx, q,
		bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY,
		date, indexType, w, h,
	).Scan(&key)
	if err != nil {
		// pgx returns pgx.ErrNoRows which is not a db error
		if err.Error() == "no rows in result set" {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cache lookup: %w", err)
	}
	return &LookupResult{MinioKey: key}, true, nil
}

// GetObject downloads an object from MinIO and returns its bytes.
func (s *Store) GetObject(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.minio.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("minio get %q: %w", key, err)
	}
	defer obj.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(obj); err != nil {
		return nil, fmt.Errorf("read minio object %q: %w", key, err)
	}
	return buf.Bytes(), nil
}

// SaveAsync uploads pngBytes to MinIO and inserts a metadata record in
// PostgreSQL asynchronously. Errors are logged but not returned to the caller
// so that the HTTP response is not blocked.
func (s *Store) SaveAsync(bbox geo.BBox, date, indexType string, w, h int, pngBytes []byte) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		key := buildKey(bbox, date, indexType, w, h)

		_, err := s.minio.PutObject(ctx, s.bucket, key,
			bytes.NewReader(pngBytes), int64(len(pngBytes)),
			minio.PutObjectOptions{ContentType: "image/png"},
		)
		if err != nil {
			// In production replace with structured logger
			fmt.Printf("[cache] minio upload error key=%s: %v\n", key, err)
			return
		}

		const ins = `
			INSERT INTO tile_cache
				(bbox_geom, bbox_3857_minx, bbox_3857_miny, bbox_3857_maxx, bbox_3857_maxy,
				 date_acquired, index_type, width, height, minio_bucket, minio_key)
			VALUES
				(ST_MakeEnvelope($1,$2,$3,$4,4326), $1,$2,$3,$4, $5,$6,$7,$8, $9,$10)
			ON CONFLICT (bbox_3857_minx, bbox_3857_miny, bbox_3857_maxx, bbox_3857_maxy,
			             date_acquired, index_type, width, height) DO NOTHING`

		_, err = s.db.Exec(ctx, ins,
			bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY,
			date, indexType, w, h,
			s.bucket, key,
		)
		if err != nil {
			fmt.Printf("[cache] db insert error key=%s: %v\n", key, err)
		}
	}()
}

// buildKey creates a deterministic MinIO object key from the tile parameters.
func buildKey(bbox geo.BBox, date, indexType string, w, h int) string {
	return fmt.Sprintf("%s/%s/%.6f_%.6f_%.6f_%.6f_%dx%d.png",
		indexType, date,
		bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY,
		w, h,
	)
}
