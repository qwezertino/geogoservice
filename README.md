# geogoservice

Stateless Go microservice that generates NDVI PNG tiles on-the-fly from Sentinel-2 satellite imagery.

## Repository layout

```
geogoservice/
├── app/                  # Go application
│   ├── cmd/server/       # Entry point (main.go)
│   ├── internal/
│   │   ├── cache/        # PostGIS + MinIO tile cache
│   │   ├── config/       # Env-based configuration
│   │   ├── geo/          # GDAL reader, CRS transforms
│   │   ├── handler/      # HTTP handlers
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
            ├── PostGIS          (tile cache index)
            ├── MinIO            (PNG cache storage)
            └── STAC providers   (satellite imagery)
                 ├── Planetary Computer  (preferred, SAS token cache)
                 └── AWS Earth Search    (public S3, no auth, fallback)
```

**Request pipeline:**
1. Transform bbox EPSG:3857 → EPSG:4326
2. Check PostGIS cache → if hit, return PNG from MinIO immediately
3. Query STAC API for least-cloudy Sentinel-2 scene (tries preferred provider first, auto-falls back)
4. Read only required pixels from COG via GDAL `/vsicurl/` (HTTP Range requests)
5. Compute NDVI = (NIR − Red) / (NIR + Red)
6. Apply colour map → encode PNG
7. Async upload to MinIO + insert index record in PostGIS
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
docker compose ps
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

Generates an NDVI PNG tile for the given bounding box and date.

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

#### Response

- `200 OK` — PNG image (`image/png`)
- `400 Bad Request` — invalid or missing parameters
- `404 Not Found` — no Sentinel-2 scene found for the given bbox/date (cloud cover < 20%)
- `500 Internal Server Error` — GDAL or processing error

---

## Example requests

### Modern format — Berlin area, 256×256 tile

```bash
curl "http://localhost/api/render?bbox=1486000,6890000,1500000,6900000&date=2024-06-15&w=256&h=256" \
  --output berlin_ndvi.png
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

The easiest way is [bboxfinder.com](http://bboxfinder.com) — draw your area, switch projection to `EPSG:3857`, copy the coordinates.

---

## Colour map

| NDVI value | Colour | Meaning |
|-----------|--------|---------|
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

Set `STAC_PROVIDER` in `.env` to change the preferred provider. Fallback is automatic regardless of which is preferred.

---

## Scaling

Change the number of app replicas at any time without downtime:

```bash
# Scale up to 5 replicas
docker compose up -d --scale gogeoapp=5

# Or use the Makefile shortcut
make scale n=5
```

Nginx automatically discovers new replicas via Docker DNS (re-resolves every 5 s).

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
