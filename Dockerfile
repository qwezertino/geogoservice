# ─── Build Stage ──────────────────────────────────────────────────────────────
# golang:alpine already ships with Go – no manual installation needed.
FROM golang:1.23-alpine3.21 AS builder

# build-base  = gcc, make, musl-dev (needed for CGO)
# gdal-dev    = GDAL headers + shared lib (pulls gdal transitively)
# pkgconfig   = lets cgo locate gdal via pkg-config
RUN apk add --no-cache \
        build-base \
        gdal-dev \
        pkgconfig

ENV CGO_ENABLED=1
WORKDIR /src

# Cache module downloads as a separate layer
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build \
        -ldflags="-s -w" \
        -o /app/geogoservice \
        ./cmd/server

# ─── Runtime Stage ────────────────────────────────────────────────────────────
FROM alpine:3.21 AS runtime

# gdal       = shared libraries required at runtime
# ca-certificates = needed for HTTPS calls (STAC API, /vsicurl/)
RUN apk add --no-cache \
        ca-certificates \
        gdal

# Non-root user for defence-in-depth
RUN addgroup -S appgroup && \
    adduser -S -G appgroup -h /app -s /sbin/nologin appuser

WORKDIR /app
COPY --from=builder /app/geogoservice .

USER appuser

EXPOSE 8080
ENTRYPOINT ["/app/geogoservice"]
