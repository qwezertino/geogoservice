// Package cache manages the PostgreSQL tile index, MinIO object storage,
// and the Redis L2 cache that sits in front of MinIO.
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
	"github.com/redis/go-redis/v9"
)

const redisTTL = time.Hour

// Store wraps the PostgreSQL connection pool, MinIO client, and Redis client.
type Store struct {
	db     *pgxpool.Pool
	minio  *minio.Client
	rdb    *redis.Client // L2 cache; nil if Redis is disabled
	bucket string
}

// New connects to PostgreSQL, MinIO, and optionally Redis.
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

	var rdb *redis.Client
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return nil, fmt.Errorf("parse REDIS_URL: %w", err)
		}
		rdb = redis.NewClient(opt)
		if err := rdb.Ping(ctx).Err(); err != nil {
			return nil, fmt.Errorf("redis ping: %w", err)
		}
		fmt.Println("[cache] Redis L2 enabled:", cfg.RedisURL)
	}

	return &Store{db: pool, minio: mc, rdb: rdb, bucket: cfg.MinioBucket}, nil
}

// Close releases all connections.
func (s *Store) Close() {
	s.db.Close()
	if s.rdb != nil {
		s.rdb.Close()
	}
}

// LookupResult is returned by Lookup when a cached tile is found.
type LookupResult struct {
	MinioKey string
}

// Lookup checks whether a tile for the given parameters already exists in the
// cache. polygonHash is "" for plain tiles and an 8-char hex string for
// polygon-masked tiles. Returns (result, true, nil) on a hit, (nil, false, nil)
// on a miss, or (nil, false, err) on a database error.
func (s *Store) Lookup(ctx context.Context, bbox geo.BBox, date string, indexType string, w, h int, polygonHash string) (*LookupResult, bool, error) {
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
		  AND  polygon_hash   = $9
		LIMIT 1`

	var key string
	err := s.db.QueryRow(ctx, q,
		bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY,
		date, indexType, w, h, polygonHash,
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

// GetObject returns the PNG bytes for a MinIO key.
// If Redis is enabled, it checks L2 first and populates it on a miss.
func (s *Store) GetObject(ctx context.Context, key string) ([]byte, error) {
	// L2: Redis
	if s.rdb != nil {
		if data, err := s.rdb.Get(ctx, key).Bytes(); err == nil {
			return data, nil
		}
	}

	// L3: MinIO
	obj, err := s.minio.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("minio get %q: %w", key, err)
	}
	defer obj.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(obj); err != nil {
		return nil, fmt.Errorf("read minio object %q: %w", key, err)
	}
	data := buf.Bytes()

	// Backfill Redis asynchronously so GetObject doesn't block on the SET.
	if s.rdb != nil {
		go func() {
			if err := s.rdb.Set(context.Background(), key, data, redisTTL).Err(); err != nil {
				fmt.Printf("[cache] redis set error key=%s: %v\n", key, err)
			}
		}()
	}

	return data, nil
}

// SaveAsync uploads pngBytes to MinIO and inserts a metadata record in
// PostgreSQL asynchronously. polygonHash is "" for plain tiles and an 8-char
// hex string for polygon-masked tiles. Errors are logged but not returned to
// the caller so that the HTTP response is not blocked.
func (s *Store) SaveAsync(bbox geo.BBox, date, indexType string, w, h int, pngBytes []byte, polygonHash string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		key := BuildKey(bbox, date, indexType, w, h, polygonHash)

		_, err := s.minio.PutObject(ctx, s.bucket, key,
			bytes.NewReader(pngBytes), int64(len(pngBytes)),
			minio.PutObjectOptions{ContentType: "image/png"},
		)
		if err != nil {
			fmt.Printf("[cache] minio upload error key=%s: %v\n", key, err)
			return
		}

		// Populate Redis L2 after a successful MinIO upload.
		if s.rdb != nil {
			if err := s.rdb.Set(ctx, key, pngBytes, redisTTL).Err(); err != nil {
				fmt.Printf("[cache] redis set error key=%s: %v\n", key, err)
			}
		}

		const ins = `
			INSERT INTO tile_cache
				(bbox_geom, bbox_3857_minx, bbox_3857_miny, bbox_3857_maxx, bbox_3857_maxy,
				 date_acquired, index_type, width, height, minio_bucket, minio_key, polygon_hash)
			VALUES
				(ST_Transform(ST_MakeEnvelope($1,$2,$3,$4,3857), 4326), $1,$2,$3,$4, $5,$6,$7,$8, $9,$10, $11)
			ON CONFLICT (bbox_3857_minx, bbox_3857_miny, bbox_3857_maxx, bbox_3857_maxy,
			             date_acquired, index_type, width, height, polygon_hash) DO NOTHING`

		_, err = s.db.Exec(ctx, ins,
			bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY,
			date, indexType, w, h,
			s.bucket, key, polygonHash,
		)
		if err != nil {
			fmt.Printf("[cache] db insert error key=%s: %v\n", key, err)
		}
	}()
}

// TileRecord is a single row returned by ListTilesByYear.
type TileRecord struct {
	BBoxMinX     float64 `json:"bbox_minx"`
	BBoxMinY     float64 `json:"bbox_miny"`
	BBoxMaxX     float64 `json:"bbox_maxx"`
	BBoxMaxY     float64 `json:"bbox_maxy"`
	DateAcquired string  `json:"date_acquired"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	MinioKey     string  `json:"minio_key"`
}

// ListTilesByYear returns cached NDVI tiles for the given year that intersect
// the provided WGS-84 bbox [minLng, minLat, maxLng, maxLat].
// At most 200 rows are returned, ordered by date_acquired ascending.
func (s *Store) ListTilesByYear(ctx context.Context, year int, bbox [4]float64) ([]TileRecord, error) {
	const q = `
		SELECT bbox_3857_minx, bbox_3857_miny, bbox_3857_maxx, bbox_3857_maxy,
		       date_acquired::text, width, height, minio_key
		FROM   tile_cache
		WHERE  EXTRACT(YEAR FROM date_acquired) = $1
		  AND  index_type = 'ndvi'
		  AND  ST_Intersects(bbox_geom, ST_MakeEnvelope($2, $3, $4, $5, 4326))
		ORDER  BY date_acquired
		LIMIT  200`

	rows, err := s.db.Query(ctx, q, year, bbox[0], bbox[1], bbox[2], bbox[3])
	if err != nil {
		return nil, fmt.Errorf("list tiles by year: %w", err)
	}
	defer rows.Close()

	results := make([]TileRecord, 0)
	for rows.Next() {
		var r TileRecord
		if err := rows.Scan(
			&r.BBoxMinX, &r.BBoxMinY, &r.BBoxMaxX, &r.BBoxMaxY,
			&r.DateAcquired, &r.Width, &r.Height, &r.MinioKey,
		); err != nil {
			return nil, fmt.Errorf("scan tile record: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tiles by year rows: %w", err)
	}
	return results, nil
}

// DeleteTile removes a tile from MinIO, PostgreSQL, and Redis (if enabled).
// It is safe to call even if the tile does not exist in one of the stores.
func (s *Store) DeleteTile(ctx context.Context, minioKey string) error {
	// 1. MinIO
	if err := s.minio.RemoveObject(ctx, s.bucket, minioKey, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("minio remove %q: %w", minioKey, err)
	}

	// 2. PostgreSQL
	const del = `DELETE FROM tile_cache WHERE minio_key = $1`
	if _, err := s.db.Exec(ctx, del, minioKey); err != nil {
		return fmt.Errorf("db delete %q: %w", minioKey, err)
	}

	// 3. Redis
	if s.rdb != nil {
		if err := s.rdb.Del(ctx, minioKey).Err(); err != nil {
			// Non-fatal — tile is already gone from the primary stores.
			fmt.Printf("[cache] redis del %q: %v\n", minioKey, err)
		}
	}

	return nil
}

// BuildKey creates a deterministic MinIO object key from the tile parameters.
// If polygonHash is non-empty it is appended to the filename, distinguishing
// polygon-masked tiles from the plain tile with the same bbox/date/dimensions.
// Exported so workers can construct the result URL before SaveAsync completes.
func BuildKey(bbox geo.BBox, date, indexType string, w, h int, polygonHash string) string {
	if polygonHash == "" {
		return fmt.Sprintf("%s/%s/%.6f_%.6f_%.6f_%.6f_%dx%d.png",
			indexType, date,
			bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY,
			w, h,
		)
	}
	return fmt.Sprintf("%s/%s/%.6f_%.6f_%.6f_%.6f_%dx%d_%s.png",
		indexType, date,
		bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY,
		w, h, polygonHash,
	)
}
