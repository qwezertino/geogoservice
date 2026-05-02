// Package config loads service configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/qwezert/geogoservice/internal/stac"
)

// Config holds all runtime configuration for the service.
type Config struct {
	Port string

	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string

	MinioEndpoint  string
	MinioAccessKey string
	MinioSecretKey string
	MinioBucket    string
	MinioUseSSL    bool

	// STACProvider controls which satellite data provider is used first.
	// Accepted values: "planetary-computer" (default), "earth-search".
	// If the preferred provider fails, the service automatically falls back
	// to the next registered provider.
	STACProvider string

	// STACSearchWindowDays is the ±radius (in days) around the requested date
	// when querying for Sentinel-2 scenes. Default: 15.
	STACSearchWindowDays int

	// STACMaxCloudCover is the maximum acceptable cloud cover percentage (0–100).
	// Default: 20.
	STACMaxCloudCover float64

	// RenderWorkers is the number of goroutines in the async render worker pool.
	// Defaults to runtime.NumCPU() when 0.
	RenderWorkers int

	// RedisURL is the connection URL for the Redis L2 tile cache.
	// Format: redis://[user:password@]host:port/db
	// If empty, Redis is disabled and tiles are always fetched from MinIO.
	RedisURL string
}

// Load reads configuration from environment variables and returns a Config.
// It returns an error if any required variable is missing.
func Load() (*Config, error) {
	required := []string{
		"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME",
		"MINIO_ENDPOINT", "MINIO_ACCESS_KEY", "MINIO_SECRET_KEY", "MINIO_BUCKET",
	}
	for _, k := range required {
		if os.Getenv(k) == "" {
			return nil, fmt.Errorf("required environment variable %s is not set", k)
		}
	}

	useSSL, _ := strconv.ParseBool(os.Getenv("MINIO_USE_SSL"))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	stacProvider := os.Getenv("STAC_PROVIDER")
	if stacProvider == "" {
		stacProvider = stac.ProviderPlanetaryComputer
	}

	searchWindow := 15
	if v := os.Getenv("STAC_SEARCH_WINDOW_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			searchWindow = n
		}
	}

	maxCloud := 20.0
	if v := os.Getenv("STAC_MAX_CLOUD_COVER"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 100 {
			maxCloud = f
		}
	}

	return &Config{
		Port:                 port,
		DBHost:               os.Getenv("DB_HOST"),
		DBPort:               os.Getenv("DB_PORT"),
		DBUser:               os.Getenv("DB_USER"),
		DBPassword:           os.Getenv("DB_PASSWORD"),
		DBName:               os.Getenv("DB_NAME"),
		MinioEndpoint:        os.Getenv("MINIO_ENDPOINT"),
		MinioAccessKey:       os.Getenv("MINIO_ACCESS_KEY"),
		MinioSecretKey:       os.Getenv("MINIO_SECRET_KEY"),
		MinioBucket:          os.Getenv("MINIO_BUCKET"),
		MinioUseSSL:          useSSL,
		STACProvider:         stacProvider,
		STACSearchWindowDays: searchWindow,
		STACMaxCloudCover:    maxCloud,
		RedisURL:             os.Getenv("REDIS_URL"),
		RenderWorkers:        func() int { n, _ := strconv.Atoi(os.Getenv("RENDER_WORKERS")); return n }(),
	}, nil
}

// DSN returns a PostgreSQL connection string.
func (c *Config) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName,
	)
}
