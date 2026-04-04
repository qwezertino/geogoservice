// Package main is the entry point for the geogoservice microservice.
package main

import (
	"context"
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
	"github.com/qwezert/geogoservice/internal/stac"
)

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Initialise shared dependencies
	store, err := cache.New(ctx, cfg)
	if err != nil {
		return fmt.Errorf("init cache store: %w", err)
	}
	defer store.Close()

	stacClient := stac.NewClient(cfg.STACProvider, nil)

	mux := http.NewServeMux()

	// Core tile endpoint
	renderHandler := handler.New(store, stacClient)
	mux.Handle("/api/render", renderHandler)

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
