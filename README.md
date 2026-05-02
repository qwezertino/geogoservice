# geogoservice

Go microservice that renders NDVI PNG tiles on-the-fly from Sentinel-2 imagery and caches them for fast repeated access.

## Repository layout

```
geogoservice/
├── app/                  # Go application
│   ├── cmd/server/       # Entry point (main.go)
│   ├── internal/
│   │   ├── cache/        # PostGIS + MinIO + Redis tile cache
│   │   ├── config/       # Env-based configuration
│   │   ├── geo/          # GDAL reader, CRS transforms
│   │   ├── handler/      # HTTP handlers (render, batch, catalog, delete)
│   │   ├── render/       # NDVI computation + colour map
│   │   └── stac/         # Multi-provider STAC client
│   ├── Dockerfile
│   ├── go.mod
│   └── go.sum
├── migrations/           # SQL schema (auto-applied on first start)
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
            └── STAC providers   (satellite imagery)
                 ├── Planetary Computer  (preferred, SAS token cache)
                 └── AWS Earth Search    (public S3, no auth, fallback)
```

**Render pipeline (cache miss):**
1. Transform bbox EPSG:3857 → EPSG:4326
2. Check Redis → check PostGIS → if hit, return PNG from MinIO immediately
3. Query STAC API for least-cloudy Sentinel-2 scene (tries preferred provider first, auto-falls back)
4. Read only required pixels from COG via GDAL `/vsicurl/` (HTTP Range requests)
5. Compute NDVI = (NIR − Red) / (NIR + Red)
6. Apply colour map → encode PNG
7. Async upload to MinIO + insert index record in PostGIS + write to Redis
8. Return PNG to client

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
make up          # runs make setup first, then docker compose up -d --scale gogeoapp=3
```

Wait ~15 seconds for PostGIS and MinIO to become healthy, then verify:

```bash
make ps
```

All services should show `healthy` or `running`.

### 3. Health check

```bash
curl http://localhost/health
# → ok
```

---

## API

### `GET /api/render`

Generates an NDVI PNG tile for the given bounding box and date. Returns the cached tile immediately on subsequent requests.

#### Parameters

Both the modern and the legacy GeoServer format are accepted simultaneously.

| Parameter | Modern | Legacy (GeoServer) | Description |
|-----------|--------|--------------------|-------------|
| bbox | `bbox=minX,minY,maxX,maxY` | `box[0]=minX&box[1]=minY&box[2]=maxX&box[3]=maxY` | Bounding box in **EPSG:3857** (metres) |
| date | `date=YYYY-MM-DD` | `date=<unix timestamp>` | Acquisition date |
| width | `w=256` | `width=256` | Output image width in pixels (1–2048) |
| height | `h=256` | `height=256` | Output image height in pixels (1–2048) |
| index | `index=ndvi` | `indexName=ndvi` | Spectral index (only `ndvi` supported) |
| srs | `srs=EPSG:3857` | — | CRS (optional, only EPSG:3857 accepted) |
| window | `window=15` | — | Search window ±N days around `date` (overrides `STAC_SEARCH_WINDOW_DAYS`) |
| cloud | `cloud=20` | — | Max cloud cover % 0–100 (overrides `STAC_MAX_CLOUD_COVER`) |

#### Response

- `200 OK` — PNG image (`image/png`)
- `400 Bad Request` — invalid or missing parameters
- `404 Not Found` — no Sentinel-2 scene found for the given bbox/date
- `500 Internal Server Error` — GDAL or processing error

---

### `POST /api/render/batch`

Renders multiple tiles in parallel and returns all results at once. Intended for NDVI viewers that need to load 10–20 tiles simultaneously — all tiles appear at the same time instead of streaming in one by one.

**Concurrent GDAL renders are capped at `RENDER_WORKERS` (default: `NumCPU`).**
**Maximum batch size: 100 tiles.**

#### Request body

JSON array of tile descriptors:

```json
[
  {"bbox": [minX, minY, maxX, maxY], "date": "2024-06-15", "index": "ndvi", "w": 512, "h": 512},
  {"bbox": [minX, minY, maxX, maxY], "date": "2024-07-01", "index": "ndvi", "w": 512, "h": 512,
   "search_window_days": 3, "max_cloud_cover": 5}
]
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `bbox` | `[4]float64` | yes | `[minX, minY, maxX, maxY]` in EPSG:3857 |
| `date` | `string` | yes | `YYYY-MM-DD` |
| `index` | `string` | yes | Spectral index — `"ndvi"` |
| `w` | `int` | yes | Width in pixels (1–2048) |
| `h` | `int` | yes | Height in pixels (1–2048) |
| `search_window_days` | `int` | no | Override `STAC_SEARCH_WINDOW_DAYS` for this tile |
| `max_cloud_cover` | `float64` | no | Override `STAC_MAX_CLOUD_COVER` for this tile |

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

Returns the list of cached NDVI tiles from PostgreSQL that intersect a given bounding box and were acquired in a given year. Useful for building a tile picker UI.

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
    "bbox":          [minLng, minLat, maxLng, maxLat]
  }
]
```

The `minio_key` field is used as the `key` parameter for `DELETE /api/tiles`.

- `200 OK` — JSON array
- `400 Bad Request` — invalid year or bbox
- `500 Internal Server Error` — database error

#### Example

```bash
curl "http://localhost/api/catalog?year=2026&bbox=30.8,46.2,31.2,46.6"
```

---

### `DELETE /api/tiles`

Removes a single tile from all three stores: MinIO, PostgreSQL, and Redis.

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
curl -X DELETE "http://localhost/api/tiles?key=ndvi/2026-04-01/3430440_5873412_..._512x398.png"
# → {"deleted":"ndvi/2026-04-01/3430440_5873412_..._512x398.png"}
```

---

## Example render requests

### Modern format — Berlin area, 256×256 tile

```bash
curl "http://localhost/api/render?bbox=1486000,6890000,1500000,6900000&date=2024-06-15&w=256&h=256" \
  --output berlin_ndvi.png
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

### Paris area, 512×512 tile

```bash
curl "http://localhost/api/render?bbox=261000,6241000,272000,6251000&date=2024-07-01&w=512&h=512" \
  --output paris_ndvi.png
```

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

The service tries providers in order and automatically falls back if one fails.

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

---

## Scaling

Change the number of app replicas at any time without downtime:

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

## Configuration (`.env`)

| Variable | Default | Description |
|----------|---------|-------------|
| `DB_USER` | `geouser` | PostgreSQL username |
| `DB_PASSWORD` | — | PostgreSQL password |
| `DB_NAME` | `geodb` | PostgreSQL database name |
| `MINIO_ACCESS_KEY` | `minioadmin` | MinIO access key |
| `MINIO_SECRET_KEY` | — | MinIO secret key |
| `MINIO_BUCKET` | `ndvi-tiles` | Bucket for cached PNG tiles |
| `STAC_PROVIDER` | `planetary-computer` | Preferred STAC provider (`planetary-computer` or `earth-search`) |
| `STAC_SEARCH_WINDOW_DAYS` | `15` | Default ±N day search window (overridable per-request via `window=`) |
| `STAC_MAX_CLOUD_COVER` | `20` | Default max cloud cover % (overridable per-request via `cloud=`) |
| `HOST_PORT_HTTP` | `80` | Host port for Nginx (public entry point) |
| `HOST_PORT_DB` | `5432` | Host port for PostgreSQL |
| `HOST_PORT_MINIO` | `9000` | Host port for MinIO S3 API |
| `HOST_PORT_MINIO_CONSOLE` | `9001` | Host port for MinIO Web UI |

> Internal ports (container-to-container) are hardcoded in `docker-compose.yml` and should not be changed.

---

## MinIO console

Browse cached tiles at **http://localhost:9001** (or your `HOST_PORT_MINIO_CONSOLE`).

Login with `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY` from your `.env`.

---

## Makefile targets

```bash
make up           # setup + docker compose up -d --scale gogeoapp=3
make down         # docker compose down
make build        # rebuild Docker images
make scale n=5    # change replica count without restart
make logs         # tail logs from all services
make test         # run Go unit tests inside container
make tidy         # go mod tidy inside container
make lint         # golangci-lint (requires local install)
make setup        # create data/ dirs with correct permissions (runs automatically with make up)
```

---

## Useful commands

```bash
# View logs from all services
docker compose logs -f

# View logs from app replicas only
docker compose logs -f gogeoapp

# Stop everything (data is preserved in ./data/)
docker compose down

# Remove everything including data
docker compose down -v
rm -rf ./data/postgres ./data/minio
```
