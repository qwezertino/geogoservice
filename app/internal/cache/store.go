// Package cache manages the PostgreSQL tile index, MinIO object storage,
// and the Redis L2 cache that sits in front of MinIO.
package cache

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/qwezert/geogoservice/internal/config"
	"github.com/qwezert/geogoservice/internal/geo"
	"github.com/redis/go-redis/v9"
)

const (
	redisTTL    = time.Hour      // live render tiles — evict quickly, saves RAM
	redisJobTTL = 24 * time.Hour // job-rendered tiles — hot data, keep longer
)

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

// Save uploads pngBytes to MinIO, populates Redis L2, and inserts a metadata
// record in PostgreSQL. Unlike SaveAsync it runs synchronously and returns any
// error — use this in background workers where you need to know whether the
// tile was actually persisted before continuing.
//
// ndviRaw is the raw float32 NDVI buffer (length = w*h). When non-nil, it is
// stored as a companion object in MinIO so that GET /api/ndvi-raw can serve
// pixel-level NDVI values without re-rendering.
func (s *Store) Save(ctx context.Context, bbox geo.BBox, date, indexType string, w, h int, pngBytes []byte, ndviRaw []float32, polygonHash string) error {
	key := BuildKey(bbox, date, indexType, w, h, polygonHash)

	_, err := s.minio.PutObject(ctx, s.bucket, key,
		bytes.NewReader(pngBytes), int64(len(pngBytes)),
		minio.PutObjectOptions{ContentType: "image/png"},
	)
	if err != nil {
		return fmt.Errorf("minio put %q: %w", key, err)
	}

	if s.rdb != nil {
		if err := s.rdb.Set(ctx, key, pngBytes, redisJobTTL).Err(); err != nil {
			fmt.Printf("[cache] redis set error key=%s: %v\n", key, err)
		}
	}

	// Store companion raw NDVI binary (little-endian float32 array).
	if len(ndviRaw) > 0 {
		rawKey := NDVIRawKey(key)
		rawBuf := make([]byte, len(ndviRaw)*4)
		for i, v := range ndviRaw {
			binary.LittleEndian.PutUint32(rawBuf[i*4:], math.Float32bits(v))
		}
		_, putErr := s.minio.PutObject(ctx, s.bucket, rawKey,
			bytes.NewReader(rawBuf), int64(len(rawBuf)),
			minio.PutObjectOptions{ContentType: "application/octet-stream"},
		)
		if putErr != nil {
			// Non-fatal — PNG is already saved; raw data is best-effort.
			fmt.Printf("[cache] minio put raw %q: %v\n", rawKey, putErr)
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
		return fmt.Errorf("db insert tile %q: %w", key, err)
	}
	return nil
}

// NDVIRawKey returns the MinIO key for the companion float32 binary of a PNG tile.
func NDVIRawKey(pngKey string) string {
	// Strip .png suffix if present and append .ndvi.bin
	if len(pngKey) > 4 && pngKey[len(pngKey)-4:] == ".png" {
		return pngKey[:len(pngKey)-4] + ".ndvi.bin"
	}
	return pngKey + ".ndvi.bin"
}

// GetNDVIRaw returns the raw float32 NDVI buffer for a tile identified by its
// PNG minio_key. Returns the little-endian float32 bytes directly so the HTTP
// handler can stream them without extra allocation.
func (s *Store) GetNDVIRaw(ctx context.Context, pngKey string) ([]byte, error) {
	rawKey := NDVIRawKey(pngKey)
	obj, err := s.minio.GetObject(ctx, s.bucket, rawKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("minio get raw %q: %w", rawKey, err)
	}
	defer obj.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(obj); err != nil {
		return nil, fmt.Errorf("read raw %q: %w", rawKey, err)
	}
	return buf.Bytes(), nil
}

// TileRenderParams holds the parameters needed to re-render a tile from scratch.
type TileRenderParams struct {
	BBox        geo.BBox
	Date        string
	W, H        int
	PolygonHash string
}

// GetTileByKey looks up tile parameters from the PostgreSQL cache by minio_key.
// Used to re-render raw NDVI data for tiles that predate the .ndvi.bin feature.
func (s *Store) GetTileByKey(ctx context.Context, minioKey string) (*TileRenderParams, error) {
	const q = `
		SELECT bbox_3857_minx, bbox_3857_miny, bbox_3857_maxx, bbox_3857_maxy,
		       date_acquired::text, width, height, polygon_hash
		FROM   tile_cache
		WHERE  minio_key = $1
		LIMIT  1`

	var rec TileRenderParams
	var date string
	err := s.db.QueryRow(ctx, q, minioKey).Scan(
		&rec.BBox.MinX, &rec.BBox.MinY, &rec.BBox.MaxX, &rec.BBox.MaxY,
		&date, &rec.W, &rec.H, &rec.PolygonHash,
	)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get tile by key: %w", err)
	}
	rec.Date = date
	return &rec, nil
}

// SaveNDVIRawAsync stores a raw float32 NDVI buffer as a companion .ndvi.bin
// object in MinIO. Runs in a background goroutine — errors are logged only.
func (s *Store) SaveNDVIRawAsync(pngKey string, rawBytes []byte) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		rawKey := NDVIRawKey(pngKey)
		_, err := s.minio.PutObject(ctx, s.bucket, rawKey,
			bytes.NewReader(rawBytes), int64(len(rawBytes)),
			minio.PutObjectOptions{ContentType: "application/octet-stream"},
		)
		if err != nil {
			fmt.Printf("[cache] SaveNDVIRawAsync %q: %v\n", rawKey, err)
		}
	}()
}

// ── Render jobs ───────────────────────────────────────────────────────────────

// ErrNotFound is returned by GetJob when no job with the given ID exists.
var ErrNotFound = errors.New("not found")

// Job is an asynchronous range-render job.
type Job struct {
	ID        string
	Status    string // pending / running / done / failed
	Done      int
	Total     int
	Errors    []string
	CreatedAt time.Time
}

// CreateJob inserts a new pending job and returns its UUID.
func (s *Store) CreateJob(
	ctx context.Context,
	id string,
	bbox geo.BBox,
	startDate, endDate string,
	maxCloud float64,
	w, h int,
	polygonHash string,
	polygon [][2]float64,
) error {
	polyJSON, err := json.Marshal(polygon)
	if err != nil {
		return fmt.Errorf("marshal polygon: %w", err)
	}
	const q = `
		INSERT INTO render_jobs
			(id, bbox_minx, bbox_miny, bbox_maxx, bbox_maxy,
			 start_date, end_date, max_cloud, w, h, polygon_hash, polygon)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
	_, err = s.db.Exec(ctx, q,
		id,
		bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY,
		startDate, endDate, maxCloud, w, h,
		polygonHash, string(polyJSON),
	)
	return err
}

// SetJobRunning transitions a job to "running" and records how many scenes were found.
func (s *Store) SetJobRunning(ctx context.Context, id string, total int) error {
	_, err := s.db.Exec(ctx,
		`UPDATE render_jobs SET status='running', total=$2 WHERE id=$1`, id, total)
	return err
}

// IncrJobDone atomically increments the done counter. Safe to call from
// multiple goroutines concurrently — the UPDATE is atomic in PostgreSQL.
func (s *Store) IncrJobDone(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE render_jobs SET done=done+1 WHERE id=$1`, id)
	return err
}

// AppendJobError appends an error message to the job's errors array.
func (s *Store) AppendJobError(ctx context.Context, id, msg string) error {
	errJSON, _ := json.Marshal(msg)
	_, err := s.db.Exec(ctx,
		`UPDATE render_jobs SET errors=errors || $2::jsonb WHERE id=$1`, id, string(errJSON))
	return err
}

// SetJobDone marks the job as successfully completed.
func (s *Store) SetJobDone(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE render_jobs SET status='done' WHERE id=$1`, id)
	return err
}

// SetJobFailed marks the job as failed and records the reason.
func (s *Store) SetJobFailed(ctx context.Context, id, reason string) error {
	errJSON, _ := json.Marshal(reason)
	_, err := s.db.Exec(ctx,
		`UPDATE render_jobs SET status='failed', errors=errors || $2::jsonb WHERE id=$1`,
		id, string(errJSON))
	return err
}

// GetJob fetches a job by ID. Returns ErrNotFound if no such job exists.
func (s *Store) GetJob(ctx context.Context, id string) (*Job, error) {
	const q = `
		SELECT id, status, done, total, errors, created_at
		FROM render_jobs WHERE id=$1`

	var job Job
	var errorsJSON []byte
	err := s.db.QueryRow(ctx, q, id).Scan(
		&job.ID, &job.Status, &job.Done, &job.Total, &errorsJSON, &job.CreatedAt,
	)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get job %s: %w", id, err)
	}
	if len(errorsJSON) > 2 { // "[]" is 2 bytes
		_ = json.Unmarshal(errorsJSON, &job.Errors)
	}
	return &job, nil
}

// JobParams holds the parameters needed to query results for a job.
type JobParams struct {
	BBox        geo.BBox
	StartDate   string
	EndDate     string
	W           int
	H           int
	PolygonHash string
}

// GetJobParams returns the stored parameters for a job (bbox, dates, dimensions).
// Used by the results endpoint to query tile_cache without the caller needing
// to know the job schema.
func (s *Store) GetJobParams(ctx context.Context, id string) (*JobParams, error) {
	const q = `
		SELECT bbox_minx, bbox_miny, bbox_maxx, bbox_maxy,
		       start_date::text, end_date::text, w, h, polygon_hash
		FROM render_jobs WHERE id=$1`

	var p JobParams
	err := s.db.QueryRow(ctx, q, id).Scan(
		&p.BBox.MinX, &p.BBox.MinY, &p.BBox.MaxX, &p.BBox.MaxY,
		&p.StartDate, &p.EndDate, &p.W, &p.H, &p.PolygonHash,
	)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get job params %s: %w", id, err)
	}
	return &p, nil
}

// JobTile is a single rendered scene belonging to a range job.
type JobTile struct {
	Date     string `json:"date"`
	MinioKey string `json:"minio_key"`
}

// ListJobTiles returns all tile_cache rows that match the job's exact bbox,
// dimensions, polygon_hash, and fall within [startDate, endDate].
// These are exactly the tiles written by runRangeJob.
func (s *Store) ListJobTiles(ctx context.Context, p *JobParams) ([]JobTile, error) {
	const q = `
		SELECT date_acquired::text, minio_key
		FROM   tile_cache
		WHERE  bbox_3857_minx = $1 AND bbox_3857_miny = $2
		  AND  bbox_3857_maxx = $3 AND bbox_3857_maxy = $4
		  AND  width = $5 AND height = $6
		  AND  polygon_hash = $7
		  AND  index_type = 'ndvi'
		  AND  date_acquired BETWEEN $8::date AND $9::date
		ORDER  BY date_acquired`

	rows, err := s.db.Query(ctx, q,
		p.BBox.MinX, p.BBox.MinY, p.BBox.MaxX, p.BBox.MaxY,
		p.W, p.H, p.PolygonHash,
		p.StartDate, p.EndDate,
	)
	if err != nil {
		return nil, fmt.Errorf("list job tiles: %w", err)
	}
	defer rows.Close()

	tiles := make([]JobTile, 0)
	for rows.Next() {
		var t JobTile
		if err := rows.Scan(&t.Date, &t.MinioKey); err != nil {
			return nil, fmt.Errorf("scan job tile: %w", err)
		}
		tiles = append(tiles, t)
	}
	return tiles, rows.Err()
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
