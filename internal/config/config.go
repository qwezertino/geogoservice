// Package config loads service configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
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
		stacProvider = "planetary-computer"
	}

	return &Config{
		Port:           port,
		DBHost:         os.Getenv("DB_HOST"),
		DBPort:         os.Getenv("DB_PORT"),
		DBUser:         os.Getenv("DB_USER"),
		DBPassword:     os.Getenv("DB_PASSWORD"),
		DBName:         os.Getenv("DB_NAME"),
		MinioEndpoint:  os.Getenv("MINIO_ENDPOINT"),
		MinioAccessKey: os.Getenv("MINIO_ACCESS_KEY"),
		MinioSecretKey: os.Getenv("MINIO_SECRET_KEY"),
		MinioBucket:    os.Getenv("MINIO_BUCKET"),
		MinioUseSSL:    useSSL,
		STACProvider:   stacProvider,
	}, nil
}

// DSN returns a PostgreSQL connection string.
func (c *Config) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName,
	)
}
