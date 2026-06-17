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
│   │   ├── cache/        # PostGIS + S3 + Redis tile cache
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
│   (S3 storage lives in the separate seaweedfs-infra project)
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
            ├── S3 storage         (PNG object storage, e.g. SeaweedFS)
            └── STAC providers   (satellite imagery — only on cache miss)
                 ├── Planetary Computer  (preferred, SAS token cache)
                 └── AWS Earth Search    (public S3, no auth, fallback)
```

**Render pipeline (cache miss):**
1. Transform bbox EPSG:3857 → EPSG:4326
2. Check Redis → check PostGIS → if hit, return PNG from S3 immediately
3. Query STAC API for least-cloudy Sentinel-2 scene (tries preferred provider first, auto-falls back)
4. Read only required pixels from COG via GDAL `/vsicurl/` (HTTP Range requests)
5. Compute NDVI = (NIR − Red) / (NIR + Red)
6. Apply colour map → if `polygon` supplied, mask pixels outside the polygon → encode PNG
7. Async upload to S3 + insert index record in PostGIS + write to Redis
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

Wait ~15 seconds for PostGIS and S3 storage to become healthy, then verify:

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

Полная интерактивная документация — **Swagger UI: `http://localhost/swagger/`**

Спецификация в формате OpenAPI 3.0: `http://localhost/openapi.yaml`

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
| `ADMIN_TOKEN` | — | Secret for `X-Admin-Token` header (admin endpoints disabled if empty) |
| `DB_HOST` | — | PostGIS hostname |
| `DB_PORT` | — | PostGIS port |
| `DB_USER` | — | PostGIS user |
| `DB_PASSWORD` | — | PostGIS password |
| `DB_NAME` | — | PostGIS database name |
| `S3_ENDPOINT` | — | S3 gateway host:port (no `http://` prefix) |
| `S3_ACCESS_KEY` | — | S3 access key |
| `S3_SECRET_KEY` | — | S3 secret key |
| `S3_BUCKET` | — | S3 bucket name |
| `S3_USE_SSL` | `false` | Use TLS for S3 connections |
| `REDIS_URL` | — | Redis connection URL (`redis://host:port/db`). Redis is disabled if empty |
| `STAC_PROVIDER` | `planetary-computer` | Preferred STAC provider |
| `STAC_SEARCH_WINDOW_DAYS` | `15` | ±N day search radius around the requested date |
| `MAX_AOI_CLOUD_COVER` | `80` | AOI cloud % above which a scene is rendered without temporal fill |
| `RENDER_WORKERS` | `NumCPU` | Max parallel GDAL renders |
| `MAX_RENDER_ATTEMPTS` | `3` | Render retries per scene/index before marking as failed |
| `HOST_PORT_HTTP` | `80` | Host port for Nginx |
| `HOST_PORT_DB` | `5432` | Host port for PostgreSQL |
| `CDSE_S3_ACCESS_KEY` | — | Copernicus Data Space S3 credentials (long-lived, from eodata-s3keysmanager) |
| `CDSE_S3_SECRET_KEY` | — | Copernicus Data Space S3 secret |
| `PPROF_ENABLED` | `false` | Expose pprof profiler at `:6060` |

---

## Performance benchmarks

Tests run on a local developer machine with `STAC_PROVIDER=local` (synthetic
COGs pre-loaded into S3, no external STAC calls).  All scenarios completed
with **0 % errors**.

### Results summary

| Scenario | Replicas | VUs | req/s | p50 | p95 | p99 |
|----------|----------|-----|-------|-----|-----|-----|
| Warm cache — Redis L2 (single tile) | 3 | 1000 | **1 693** | 45 ms | 186 ms | 775 ms |
| Warm cache — S3 only (single tile) | 3 | 1000 | **930** | 92 ms | 647 ms | 2.1 s |
| Warm cache — Redis + S3 (60 unique tiles) | 10 | 1000 | **1 855** | 39 ms | 160 ms | 480 ms |
| Cold render — no singleflight | 3 | 100 | **164** | 137 ms | 685 ms | — |
| Cold render — with singleflight | 10 | 100 | **455** | 52 ms | 157 ms | — |

### Warm cache — 10 replicas, 60 unique tiles (Redis + S3)

```
http_req_duration..: avg=61ms  p(50)=39.61ms  p(95)=160.71ms  p(99)=480.66ms  max=3.78s
http_req_failed....: 0.00%   0 out of 475 846
http_reqs..........: 475 846  1855 req/s
data_received......: 49 GB    191 MB/s
png_size_bytes.....: avg=99 300  min=98 774  max=99 993
```

60 unique pre-warmed tiles, random selection per VU. Redis serves hot tiles
from RAM; cold tiles fall through to S3. At 191 MB/s the bottleneck is
S3 I/O, not CPU.

### Warm cache — 3 replicas, single hot tile (Redis L2)

```
http_req_duration..: avg=75ms   p(50)=45ms   p(95)=186ms   p(99)=775ms
http_req_failed....: 0.00%
http_reqs..........: 422 205   1693 req/s
data_received......: 66 GB     263 MB/s
```

Single tile, 100 % Redis hits. PostGIS only validates key freshness. At
263 MB/s the bottleneck is the loopback network, not the Go service.

### Cold render — 10 replicas + singleflight (GDAL → NDVI → PNG)

```
http_req_duration..: avg=~100ms  p(50)=52ms  p(95)=157ms
http_req_failed....: 0.00%
http_reqs..........: ~27 000    455 req/s
```

With singleflight, identical concurrent requests collapse into one GDAL
pipeline per replica. At 10 replicas that is up to 10 parallel pipelines.
Compared to the 3-replica baseline (164 req/s, p95=648ms) this is **+171 %
throughput** and **−76 % p95 latency**.

### Cold render — 3 replicas, no singleflight (baseline)

```
http_req_duration..: avg=225ms  p(50)=137ms  p(95)=685ms  max=11s
http_req_failed....: 0.00%
http_reqs..........: 49 679    164 req/s
data_received......: 3.6 GB    12 MB/s
```

CPU is the limiting resource: GDAL HTTP range requests + NDVI computation
+ PNG encoding compete for cores. Max latency spike (11 s) occurs at peak
100-VU load when all GDAL semaphore slots are taken.

### Profiling (pprof)

Enable pprof with `PPROF_ENABLED=true` in `.env`, then while k6 is running:

```bash
go tool pprof -http=:8888 'http://localhost:6060/debug/pprof/profile?seconds=30'
```

CPU flame graphs show the cold-render bottleneck is split between GDAL HTTP
range requests to S3 and PNG encoding; NDVI math is negligible.

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

## S3 admin console

The object store itself (SeaweedFS) lives in the separate `seaweedfs-infra` project — see its `docker-compose.yml` for the admin UI port and credentials.

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
docker compose down -v && rm -rf ./data/postgres
