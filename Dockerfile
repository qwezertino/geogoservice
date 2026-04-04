# ─── Build Stage ──────────────────────────────────────────────────────────────
FROM ubuntu:22.04 AS builder

ENV DEBIAN_FRONTEND=noninteractive

# Install GDAL dev libs + Go build dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        curl \
        git \
        libgdal-dev \
        gdal-bin \
        pkg-config \
    && rm -rf /var/lib/apt/lists/*

# Install Go 1.22
ENV GO_VERSION=1.22.2
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
    | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:${PATH}"
ENV GOPATH=/go
ENV CGO_ENABLED=1

WORKDIR /src

# Cache Go module downloads separately from source
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a statically-linked (but CGO-enabled) binary
COPY . .
RUN go build \
        -ldflags="-s -w" \
        -o /app/geogoservice \
        ./cmd/server

# ─── Runtime Stage ────────────────────────────────────────────────────────────
FROM ubuntu:22.04 AS runtime

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        libgdal-dev \
        gdal-bin \
    && rm -rf /var/lib/apt/lists/*

# Non-root user for defence-in-depth
RUN groupadd -r appgroup && useradd -r -g appgroup -d /app -s /sbin/nologin appuser

WORKDIR /app
COPY --from=builder /app/geogoservice .

USER appuser

EXPOSE 8080
ENTRYPOINT ["/app/geogoservice"]
