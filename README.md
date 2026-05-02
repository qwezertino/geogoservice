# geogoservice

Go microservice that renders NDVI PNG tiles on-the-fly from Sentinel-2 imagery and caches them for fast repeated access.

## Repository layout

```
geogoservice/
├── app/                  # Go application
│   ├── cmd/server/
│   │   ├── main.go           # Entry point; runs DB migrations on startup
│   │   └── migrations/       # SQL files embedded into the binary
│   ├── internal/
│   │   ├── cache/        # PostGIS + MinIO + Redis tile cache
│   │   ├── config/       # Env-based configuration
│   │   ├── geo/          # GDAL reader, CRS transforms, polygon masking
│   │   ├── handler/      # HTTP handlers (render, batch, catalog, delete)
│   │   ├── migrate/      # golang-migrate wrapper (iofs + pgx5)
│   │   ├── render/       # NDVI computation + colour map
│   │   └── stac/         # Multi-provider STAC client
│   ├── Dockerfile
│   ├── go.mod
│   └── go.sum
├── nginx/                # Nginx load-balancer config
├── data/                 # Runtime data (gitignored)
│   ├── postgres/         # PostGIS volume
│   └── minio/            # MinIO volume
├── docker-compose.yml
├── Makefile
└── .env.example
```

## Architecture

```
Client
  │
  ▼
Nginx :80  (load balancer, re-resolves Docker DNS every 5 s)
  │
  ├──▶ gogeoapp replica 1 :8080
  ├──▶ gogeoapp replica 2 :8080
  └──▶ gogeoapp replica 3 :8080
            │
            ├── Redis            (L2 in-memory cache, TTL 1 h)
            ├── PostGIS          (tile index, spatial queries)
            ├── MinIO            (PNG object storage)
            └── STAC providers   (satellite imagery — only on cache miss)
                 ├── Planetary Computer  (preferred, SAS token cache)
                 └── AWS Earth Search    (public S3, no auth, fallback)
```

**Render pipeline (cache miss):**
1. Transform bbox EPSG:3857 → EPSG:4326
2. Check Redis → check PostGIS → if hit, return PNG from MinIO immediately
3. Query STAC API for least-cloudy Sentinel-2 scene (tries preferred provider first, auto-falls back)
4. Read only required pixels from COG via GDAL `/vsicurl/` (HTTP Range requests)
5. Compute NDVI = (NIR − Red) / (NIR + Red)
6. Apply colour map → if `polygon` supplied, mask pixels outside the polygon → encode PNG
7. Async upload to MinIO + insert index record in PostGIS + write to Redis
8. Return PNG to client

**DB migrations** run automatically on every container startup via `golang-migrate`. SQL files are embedded in the binary — no external tooling needed.

---

## Quick start

### 1. Prerequisites

- Docker + Docker Compose v2
- A `.env` file (copy from `.env.example`)

```bash
cp .env.example .env
# Edit .env – change passwords and host ports if needed
```

### 2. Start the stack

```bash
make up
```

Wait ~15 seconds for PostGIS and MinIO to become healthy, then verify:

```bash
make ps
```

### 3. Health check

```bash
curl http://localhost/health
# → ok
```

---

## API

### `GET /api/render`

Generates an NDVI PNG tile for the given bounding box and date. Returns the cached tile immediately on subsequent requests.

An optional polygon mask clips the PNG to an arbitrary shape — pixels outside the polygon become transparent. Masked and unmasked versions are cached independently.

#### Parameters

Both the modern and the legacy GeoServer format are accepted simultaneously.

| Parameter | Format | Description |
|-----------|--------|-------------|
| `bbox` | `minX,minY,maxX,maxY` | Bounding box in **EPSG:3857** (metres). Legacy: `box[0..3]=` |
| `date` | `YYYY-MM-DD` or Unix timestamp | Acquisition date |
| `w` / `width` | integer 1–2048 | Output width in pixels |
| `h` / `height` | integer 1–2048 | Output height in pixels |
| `index` / `indexName` | `ndvi` | Spectral index |
| `polygon` | `lng1,lat1,lng2,lat2,...` | Optional WGS-84 clipping polygon (≥ 3 pairs). Pixels outside become transparent |
| `window` | integer | Search window ±N days around `date` (overrides `STAC_SEARCH_WINDOW_DAYS`) |
| `cloud` | float 0–100 | Max cloud cover % (overrides `STAC_MAX_CLOUD_COVER`) |
| `srs` | `EPSG:3857` | CRS (optional, only EPSG:3857 accepted) |

#### Response

- `200 OK` — PNG image (`image/png`)
- `400 Bad Request` — invalid or missing parameters
- `404 Not Found` — no Sentinel-2 scene found for the given bbox/date
- `500 Internal Server Error` — GDAL or processing error

#### Example — with polygon mask

```bash
curl "http://localhost/api/render?bbox=3430000,5872000,3432000,5874000&date=2026-04-01&w=512&h=512\
&polygon=30.83,46.21,30.84,46.21,30.84,46.22,30.83,46.22" \
  --output masked_ndvi.png
```

---

### `POST /api/render/batch`

Renders multiple tiles in parallel and returns all results at once. Intended for NDVI viewers that need to load 10–20 tiles simultaneously — all tiles appear at the same time.

**When `minio_key` is provided the tile is fetched directly from cache (MinIO/Redis) — no STAC call is made.** Use this for the catalog-viewer flow.

**Concurrent GDAL renders are capped at `RENDER_WORKERS` (default: `NumCPU`).**
**Maximum batch size: 100 tiles.**

#### Request body

JSON array of tile descriptors. Two usage modes:

**Mode 1 — fetch from cache by key** (catalog flow, no STAC):

```json
[
  {"minio_key": "ndvi/2026-04-01/3430440_..._512x398.png"},
  {"minio_key": "ndvi/2026-04-02/3430440_..._512x398.png"}
]
```

**Mode 2 — render on demand** (arbitrary bbox/date):

```json
[
  {"bbox": [minX, minY, maxX, maxY], "date": "2024-06-15", "index": "ndvi", "w": 512, "h": 512},
  {"bbox": [minX, minY, maxX, maxY], "date": "2024-07-01", "index": "ndvi", "w": 512, "h": 512,
   "polygon": [[30.83, 46.21], [30.84, 46.21], [30.84, 46.22]],
   "search_window_days": 3, "max_cloud_cover": 5}
]
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `minio_key` | `string` | — | Object key from `GET /api/catalog`. If present, all other fields are ignored |
| `bbox` | `[4]float64` | yes* | `[minX, minY, maxX, maxY]` in EPSG:3857. *Required when `minio_key` is absent |
| `date` | `string` | yes* | `YYYY-MM-DD` |
| `index` | `string` | yes* | `"ndvi"` |
| `w` | `int` | yes* | Width in pixels (1–2048) |
| `h` | `int` | yes* | Height in pixels (1–2048) |
| `polygon` | `[[lng,lat],...]` | no | WGS-84 clipping polygon (≥ 3 pairs) |
| `search_window_days` | `int` | no | Override `STAC_SEARCH_WINDOW_DAYS` |
| `max_cloud_cover` | `float64` | no | Override `STAC_MAX_CLOUD_COVER` |

#### Response body

JSON array in the **same order** as the request:

```json
[
  {"index": 0, "data": "<base64 PNG>", "cached": true},
  {"index": 1, "error": "no scene found"}
]
```

- `200 OK` — always returned; per-tile errors are embedded in the array
- `400 Bad Request` — malformed JSON or batch exceeds 100 items

---

### `GET /api/catalog`

Returns cached NDVI tiles from PostgreSQL that intersect a given WGS-84 bounding box and were acquired in a given year.

#### Parameters

| Parameter | Description |
|-----------|-------------|
| `year` | Integer, 2015–2100 |
| `bbox` | `minLng,minLat,maxLng,maxLat` in **EPSG:4326** (WGS-84 degrees) |

#### Response body

JSON array (never `null` — empty array when no results):

```json
[
  {
    "minio_key":     "ndvi/2026-04-01/3430440_5873412_..._512x398.png",
    "date_acquired": "2026-04-01",
    "index_type":    "ndvi",
    "width":         512,
    "height":        398,
    "bbox_minx":     3430440.234,
    "bbox_miny":     5873412.602,
    "bbox_maxx":     3430578.179,
    "bbox_maxy":     5873519.801
  }
]
```

Pass the `minio_key` values straight into `POST /api/render/batch` to load tiles without any STAC calls.

- `200 OK` — JSON array
- `400 Bad Request` — invalid year or bbox
- `500 Internal Server Error` — database error

#### Example

```bash
curl "http://localhost/api/catalog?year=2026&bbox=30.8,46.2,31.2,46.6"
```

---

### `DELETE /api/tiles`

Removes a single tile from MinIO, PostgreSQL, and Redis.

#### Parameters

| Parameter | Description |
|-----------|-------------|
| `key` | `minio_key` value from `GET /api/catalog` |

#### Response

- `200 OK` — `{"deleted": "<key>"}`
- `400 Bad Request` — missing or invalid key (path traversal rejected)
- `500 Internal Server Error` — storage error

#### Example

```bash
curl -X DELETE "http://localhost/api/tiles?key=ndvi/2026-04-01/3430440_..._512x398.png"
```

---

## Typical catalog-viewer flow

```
1. GET /api/catalog?year=2026&bbox=<viewport WGS-84>
      → returns list of tiles with minio_key

2. POST /api/render/batch
      body: [{"minio_key": "..."}, {"minio_key": "..."}, ...]
      → returns base64 PNGs for all tiles at once, fetched from cache only
        (no STAC calls, no rendering)

3. Display all tiles simultaneously
```

---

## Example render requests

### Modern format — Berlin area, 256×256 tile

```bash
curl "http://localhost/api/render?bbox=1486000,6890000,1500000,6900000&date=2024-06-15&w=256&h=256" \
  --output berlin_ndvi.png
```

### With polygon mask

```bash
curl "http://localhost/api/render?bbox=3430000,5872000,3432000,5874000&date=2026-04-01&w=512&h=512\
&polygon=30.83,46.21,30.84,46.21,30.84,46.22,30.83,46.22" \
  --output masked_ndvi.png
```

### Narrow search — only ±3 days, max 5% cloud cover

```bash
curl "http://localhost/api/render?bbox=1486000,6890000,1500000,6900000&date=2024-06-15&w=256&h=256&window=3&cloud=5" \
  --output berlin_ndvi_clear.png
```

### Legacy GeoServer format (drop-in replacement)

```bash
curl "http://localhost/api/render?box[0]=1486000&box[1]=6890000&box[2]=1500000&box[3]=6900000&date=1718409600&width=256&height=256&indexName=ndvi" \
  --output berlin_ndvi.png
```

> `date=1718409600` is Unix timestamp for `2024-06-15`.

### How to find a bbox in EPSG:3857

[bboxfinder.com](http://bboxfinder.com) — draw your area, switch projection to `EPSG:3857`, copy the coordinates.

---

## Colour map

| NDVI value | Colour | Meaning |
|------------|--------|---------|
| −1.0 … 0.05 | Transparent | Water, clouds, bare rock |
| 0.05 … 0.2  | Red → Yellow | Sparse / stressed vegetation |
| 0.2 … 1.0   | Light green → Dark green | Healthy vegetation |

---

## STAC providers

The service tries providers in order and automatically falls back if one fails. STAC is only contacted on a cache miss.

| Provider | `STAC_PROVIDER` value | Data source | Auth |
|----------|-----------------------|-------------|------|
| Microsoft Planetary Computer | `planetary-computer` (default) | Azure Blob COGs | SAS token (auto-refreshed every ~55 min) |
| AWS Earth Search | `earth-search` | Public S3 (`sentinel-cogs`) | None |

Set `STAC_PROVIDER` in `.env` to change the preferred provider. Fallback is automatic.

---

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP listen port inside the container |
| `DB_HOST` | — | PostGIS hostname |
| `DB_PORT` | — | PostGIS port |
| `DB_USER` | — | PostGIS user |
| `DB_PASSWORD` | — | PostGIS password |
| `DB_NAME` | — | PostGIS database name |
| `MINIO_ENDPOINT` | — | MinIO host:port |
| `MINIO_ACCESS_KEY` | — | MinIO access key |
| `MINIO_SECRET_KEY` | — | MinIO secret key |
| `MINIO_BUCKET` | — | MinIO bucket name |
| `MINIO_USE_SSL` | `false` | Use TLS for MinIO |
| `REDIS_URL` | — | Redis connection URL (`redis://host:port/db`). Redis is disabled if empty |
| `STAC_PROVIDER` | `planetary-computer` | Preferred STAC provider |
| `STAC_SEARCH_WINDOW_DAYS` | `15` | ±N day search radius around the requested date |
| `STAC_MAX_CLOUD_COVER` | `20` | Max cloud cover % for scene selection |
| `RENDER_WORKERS` | `NumCPU` | Max parallel GDAL renders |
| `HOST_PORT_HTTP` | `80` | Host port for Nginx |
| `HOST_PORT_DB` | `5432` | Host port for PostgreSQL |
| `HOST_PORT_MINIO` | `9000` | Host port for MinIO S3 API |
| `HOST_PORT_MINIO_CONSOLE` | `9001` | Host port for MinIO Web UI |

---

## Performance benchmarks

Load tests run on a local machine with 3 app replicas (`make loadtest-up`),
`STAC_PROVIDER=local` (synthetic COGs pre-loaded into MinIO, no external STAC
calls), and the k6 scripts in `loadtest/k6/`.

### Results summary

| Scenario | VUs | req/s | p50 | p95 | p99 | Notes |
|----------|-----|-------|-----|-----|-----|-------|
| **Warm cache — Redis L2** | 1000 | **1 693** | 45 ms | 186 ms | 775 ms | PostGIS lookup → Redis RAM → HTTP |
| **Warm cache — MinIO only** | 1000 | **930** | 92 ms | 647 ms | 2.1 s | PostGIS lookup → MinIO GET → HTTP |
| **Cold render** | 100 | **164** | 137 ms | 685 ms | — | GDAL range-req → NDVI → PNG encode |

All scenarios completed with **0 % errors**.

### Cache hit path (Redis L2 enabled)

```
checks_succeeded...: 100.00% 1 266 615 / 1 266 615
http_req_duration..: avg=75ms   p(50)=45ms   p(90)=135ms  p(95)=186ms  p(99)=775ms
http_req_failed....: 0.00%
http_reqs..........: 422 205   1693 req/s
data_received......: 66 GB     263 MB/s
```

Each PNG tile is ~149 KB. Redis absorbs the read load; PostGIS only does a
key lookup to validate freshness.

### Cache hit path (MinIO only, Redis disabled)

```
checks_succeeded...: 100.00% 692 382 / 692 382
http_req_duration..: avg=189ms  p(50)=92ms   p(90)=359ms  p(95)=647ms  p(99)=2.1s
http_req_failed....: 0.00%
http_reqs..........: 230 795   931 req/s
data_received......: 36 GB     145 MB/s
```

Bottleneck is MinIO I/O: 930 req/s × 149 KB ≈ **138 MB/s** saturates the
local MinIO instance.

### Cold render path (GDAL → NDVI → PNG)

```
checks_succeeded...: 100.00% 149 037 / 149 037
http_req_duration..: avg=225ms  p(50)=137ms  p(90)=501ms  p(95)=685ms  max=11s
http_req_failed....: 0.00%
http_reqs..........: 49 679    164 req/s
data_received......: 3.6 GB    12 MB/s
```

At 100 concurrent VUs the 3 replicas together sustain 164 renders/s
(~55 renders/s per replica). The CPU semaphore (capped at `GOMAXPROCS`) keeps
each replica healthy; the long max latency (11 s) appears at the 100-VU peak
when all GDAL slots are busy.

### Profiling (pprof)

Enable pprof with `PPROF_ENABLED=true` in `.env`, then while k6 is running:

```bash
go tool pprof -http=:8888 'http://localhost:6060/debug/pprof/profile?seconds=30'
```

CPU flame graphs show that the cold-render bottleneck is split between GDAL
HTTP range requests to MinIO and PNG encoding; NDVI math is negligible.

---

## Scaling

```bash
make scale n=5
```

Nginx automatically discovers new replicas via Docker DNS (re-resolves every 5 s).

---

## Makefile targets

| Target | Description |
|--------|-------------|
| `make up` | Build images and start 3 replicas |
| `make down` | Stop all containers (volumes preserved) |
| `make scale n=N` | Change replica count on-the-fly |
| `make ps` | Show container status |
| `make logs` | Tail logs from all services |
| `make build` | Build Docker images only |
| `make test` | Run Go unit tests inside a container |
| `make tidy` | Run `go mod tidy` inside a container |

---

## MinIO console

Browse cached tiles at **http://localhost:9001** (or your `HOST_PORT_MINIO_CONSOLE`).

Login with `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY` from your `.env`.

---

## Useful commands

```bash
# Tail logs from all services
docker compose logs -f

# Tail logs from app replicas only
docker compose logs -f gogeoapp

# Stop everything (data is preserved in ./data/)
docker compose down

# Remove everything including data
docker compose down -v && rm -rf ./data/postgres ./data/minio
