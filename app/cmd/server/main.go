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

	// Core tile endpoint — GET returns PNG (cache hit: fast, miss: GDAL render).
	// Concurrent GDAL renders are capped at RenderWorkers (default = NumCPU).
	renderHandler := handler.New(store, stacClient, handler.HandlerOptions{
		DefaultSearchWindowDays: cfg.STACSearchWindowDays,
		DefaultMaxCloudCover:    cfg.STACMaxCloudCover,
		RenderWorkers:           cfg.RenderWorkers,
	})
	mux.Handle("/api/render", renderHandler)

	// Batch tile endpoint — POST returns JSON array of base64-encoded PNGs.
	// All tiles in the batch are rendered in parallel (shared semaphore).
	// Client receives all results at once → displays all tiles simultaneously.
	mux.HandleFunc("/api/render/batch", renderHandler.ServeBatch)

	// Raw NDVI float32 data for pixel-level hover values on the frontend.
	// GET /api/ndvi-raw?key=<minio_key> → little-endian float32 array (W×H)
	mux.HandleFunc("GET /api/ndvi-raw", renderHandler.ServeNDVIRaw)

	// PNG proxy for job-rendered tiles stored in MinIO.
	// GET /api/images?key=<minio_key> → image/png
	mux.HandleFunc("GET /api/images", renderHandler.ServeImage)

	// Catalog endpoint — GET returns JSON list of cached NDVI tiles from PostgreSQL.
	mux.HandleFunc("/api/catalog", renderHandler.ServeCatalog)

	// Delete endpoint — DELETE removes a tile from MinIO, PostgreSQL, and Redis.
	mux.HandleFunc("/api/tiles", renderHandler.ServeDelete)

	// Multi-index job API — POST /api/jobs, GET /api/jobs/{id}, …/results.
	mux.HandleFunc("POST /api/jobs", renderHandler.ServeCreateJob)
	mux.HandleFunc("GET /api/jobs/{id}", renderHandler.ServeJobStatus)
	mux.HandleFunc("GET /api/jobs/{id}/results", renderHandler.ServeJobResults)

	// Scene discovery — returns available Sentinel-2 scenes for a polygon and
	// date range without triggering any rendering.
	// GET /api/scenes?polygon=<GeoJSON>&date_from=…&date_to=…&max_cloud=20
	mux.HandleFunc("GET /api/scenes", renderHandler.ServeScenes)

	// Health check (used by Docker Compose and Nginx)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
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
