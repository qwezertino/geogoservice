// Package main is the entry point for the geogoservice microservice.
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/airbusgeo/godal"
	"github.com/qwezert/geogoservice/internal/cache"
	"github.com/qwezert/geogoservice/internal/config"
	"github.com/qwezert/geogoservice/internal/handler"
	"github.com/qwezert/geogoservice/internal/migrate"
	"github.com/qwezert/geogoservice/internal/stac"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

//go:embed openapi.yaml
var openapiSpec []byte

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Register all GDAL drivers and VSI handlers (GTiff, /vsicurl/, etc.)
	godal.RegisterAll()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Run database migrations before anything else.
	// Idempotent: already-applied migrations are skipped automatically.
	fmt.Println("[migrate] applying pending migrations...")
	if err := migrate.Run(migrate.MigrationsFS{FS: migrationsFS, Dir: "migrations"}, cfg.MigrateDSN()); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	fmt.Println("[migrate] done")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Initialise shared dependencies
	store, err := cache.New(ctx, cfg)
	if err != nil {
		return fmt.Errorf("init cache store: %w", err)
	}
	defer store.Close()

	stacClient := stac.NewClient(cfg.STACProvider, nil, cfg.CDSES3AccessKey, cfg.CDSES3SecretKey)

	mux := http.NewServeMux()

	renderHandler := handler.New(ctx, store, stacClient, handler.HandlerOptions{
		DefaultSearchWindowDays: cfg.STACSearchWindowDays,
		MaxAOICloudCover:        cfg.MaxAOICloudCover,
		MaxRenderAttempts:       cfg.MaxRenderAttempts,
		RenderWorkers:           cfg.RenderWorkers,
	})

	auth  := handler.RequireAPIKey(store)
	admin := handler.RequireAdmin(cfg.AdminToken)

	// ── Protected API routes (require valid API key) ──────────────────────────

	// Core tile endpoint — GET returns PNG (cache hit: fast, miss: GDAL render).
	// Concurrent GDAL renders are capped at RenderWorkers (default = NumCPU).
	mux.Handle("/api/render", auth(renderHandler))

	// Batch tile endpoint — POST returns JSON array of base64-encoded PNGs.
	// All tiles in the batch are rendered in parallel (shared semaphore).
	// Client receives all results at once → displays all tiles simultaneously.
	mux.Handle("/api/render/batch", auth(http.HandlerFunc(renderHandler.ServeBatch)))

	// Raw NDVI float32 data for pixel-level hover values on the frontend.
	// GET /api/ndvi-raw?key=<s3_key> → little-endian float32 array (W×H)
	mux.Handle("GET /api/ndvi-raw", auth(http.HandlerFunc(renderHandler.ServeNDVIRaw)))

	// PNG proxy for job-rendered tiles stored in S3.
	// GET /api/images?key=<s3_key> → image/png
	mux.Handle("GET /api/images", auth(http.HandlerFunc(renderHandler.ServeImage)))

	// Catalog endpoint — GET returns JSON list of cached NDVI tiles from PostgreSQL.
	mux.Handle("/api/catalog", auth(http.HandlerFunc(renderHandler.ServeCatalog)))

	// Delete endpoint — DELETE removes a tile from S3, PostgreSQL, and Redis.
	mux.Handle("/api/tiles", auth(http.HandlerFunc(renderHandler.ServeDelete)))

	// Multi-index job API — POST /api/jobs, GET /api/jobs/{id}, …/results.
	mux.Handle("POST /api/jobs", auth(http.HandlerFunc(renderHandler.ServeCreateJob)))
	mux.Handle("GET /api/jobs/{id}", auth(http.HandlerFunc(renderHandler.ServeJobStatus)))
	mux.Handle("GET /api/jobs/{id}/results", auth(http.HandlerFunc(renderHandler.ServeJobResults)))

	// Scene discovery — returns available Sentinel-2 scenes for a polygon and
	// date range without triggering any rendering.
	// GET /api/scenes?polygon=<GeoJSON>&date_from=…&date_to=…&max_cloud=20
	mux.Handle("GET /api/scenes", auth(http.HandlerFunc(renderHandler.ServeScenes)))

	// ── Self-service endpoints (require valid API key) ────────────────────────

	mux.Handle("GET /api/me", auth(http.HandlerFunc(renderHandler.ServeGetMe)))
	mux.Handle("PUT /api/me/settings", auth(http.HandlerFunc(renderHandler.ServeUpdateSettings)))

	// ── Admin endpoints (require X-Admin-Token header) ────────────────────────

	mux.Handle("POST /api/admin/keys", admin(http.HandlerFunc(renderHandler.ServeCreateAPIKey)))
	mux.Handle("GET /api/admin/keys", admin(http.HandlerFunc(renderHandler.ServeListAPIKeys)))
	mux.Handle("DELETE /api/admin/keys/{token}", admin(http.HandlerFunc(renderHandler.ServeDeleteAPIKey)))

	// ── Public ────────────────────────────────────────────────────────────────

	// Health check (used by Docker Compose and Nginx)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	// OpenAPI spec — served as YAML for tooling and Swagger UI to consume.
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(openapiSpec)
	})

	// Swagger UI — loads from CDN, points at /openapi.yaml on the same origin.
	mux.HandleFunc("GET /swagger/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, swaggerUI)
	})

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second, // allow time for GDAL+STAC processing
		IdleTimeout:  60 * time.Second,
	}

	// Start server in background
	srvErr := make(chan error, 1)
	go func() {
		fmt.Printf("geogoservice listening on :%s\n", cfg.Port)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()

	// Wait for shutdown signal or server error
	select {
	case err := <-srvErr:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		fmt.Println("shutdown signal received, draining connections…")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	fmt.Println("server stopped cleanly")
	return nil
}
