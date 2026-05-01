// Package stac provides a multi-provider STAC client with automatic fallback.
//
// Architecture: add a new satellite data source by implementing the Provider
// interface and registering it in NewClient. No other files need to change.
package stac

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/qwezert/geogoservice/internal/geo"
)

// Provider name constants — used in the STAC_PROVIDER env variable.
const (
	ProviderPlanetaryComputer = "planetary-computer"
	ProviderEarthSearch       = "earth-search"
)

const (
	// Sentinel2Collection is the Sentinel-2 Level-2A collection ID used by all providers.
	Sentinel2Collection = "sentinel-2-l2a"

	defaultSearchWindowDays = 15
	defaultMaxCloudCover    = 20.0
)

// BandURLs holds the ready-to-use (signed if necessary) HTTPS/S3 URLs for the
// Red (B04) and NIR (B08) bands of a Sentinel-2 scene.
type BandURLs struct {
	RedURL string
	NIRURL string
}

// Provider is the interface every STAC data source must implement.
// To add a new provider:
//  1. Create internal/stac/provider_<name>.go
//  2. Implement Name() and FindBestScene()
//  3. Instantiate and add it to the ordered slice in NewClient
type Provider interface {
	// Name returns the human-readable identifier used in logs.
	Name() string
	// FindBestScene returns COG URLs for the least-cloudy Sentinel-2 scene
	// that intersects bbox (EPSG:4326) within ±windowDays of date,
	// filtered to scenes with cloud cover below maxCloud.
	FindBestScene(ctx context.Context, bbox geo.BBox, date string, windowDays int, maxCloud float64) (*BandURLs, error)
}

// Client holds an ordered list of providers and retries each one in turn,
// returning the first successful result (automatic fallback).
type Client struct {
	providers        []Provider
	searchWindowDays int
	maxCloudCover    float64
}

// ClientOptions holds optional tuning parameters for the STAC client.
type ClientOptions struct {
	// SearchWindowDays is the ±radius (in days) around the requested date.
	// Sentinel-2 revisit period is ~5 days; 15 days guarantees ≥3 overpasses.
	// Defaults to 15 when zero.
	SearchWindowDays int
	// MaxCloudCover is the maximum acceptable cloud cover percentage (0–100).
	// Defaults to 20 when zero.
	MaxCloudCover float64
}

// NewClient builds a Client with providers ordered so that preferredName is
// tried first. All known providers are registered so fallback is always
// available even if the preferred one is unavailable.
func NewClient(preferredName string, httpClient *http.Client, opts ClientOptions) *Client {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{MaxIdleConnsPerHost: 10},
		}
	}

	searchWindow := opts.SearchWindowDays
	if searchWindow <= 0 {
		searchWindow = defaultSearchWindowDays
	}
	maxCloud := opts.MaxCloudCover
	if maxCloud <= 0 {
		maxCloud = defaultMaxCloudCover
	}

	pc := newPlanetaryComputerProvider(httpClient)
	es := newEarthSearchProvider(httpClient)

	// Default order: PC first, Earth Search as fallback.
	// Flip when the operator explicitly prefers Earth Search.
	ordered := []Provider{pc, es}
	if preferredName == ProviderEarthSearch {
		ordered = []Provider{es, pc}
	}

	return &Client{
		providers:        ordered,
		searchWindowDays: searchWindow,
		maxCloudCover:    maxCloud,
	}
}

// FindBestScene tries each registered provider in order, returning the first
// successful result. Failures are logged so operators can see which provider
// was used and why a fallback occurred.
func (c *Client) FindBestScene(ctx context.Context, bbox geo.BBox, date string) (*BandURLs, error) {
	var lastErr error
	for _, p := range c.providers {
		result, err := p.FindBestScene(ctx, bbox, date, c.searchWindowDays, c.maxCloudCover)
		if err == nil {
			return result, nil
		}
		fmt.Printf("[stac] provider %q failed: %v — trying next\n", p.Name(), err)
		lastErr = err
	}
	return nil, fmt.Errorf("all STAC providers failed; last error: %w", lastErr)
}

// ── Shared STAC search helpers ────────────────────────────────────────────────

// stacSearchRequest is the JSON body sent to any OGC/STAC /search endpoint.
type stacSearchRequest struct {
	Collections []string               `json:"collections"`
	Datetime    string                 `json:"datetime"`
	BBox        [4]float64             `json:"bbox"`
	Query       map[string]interface{} `json:"query"`
	SortBy      []stacSortBy           `json:"sortby"`
	Limit       int                    `json:"limit"`
}

type stacSortBy struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
}

// stacSearchResponse is a partial GeoJSON FeatureCollection from /search.
type stacSearchResponse struct {
	Features []stacRawFeature `json:"features"`
}

// stacRawFeature uses a generic asset map so each provider can look up its own
// asset keys (e.g. "B04"/"B08" for PC vs "red"/"nir" for Earth Search).
type stacRawFeature struct {
	Assets map[string]*stacAsset `json:"assets"`
}

type stacAsset struct {
	Href string `json:"href"`
}

// buildDatetimeInterval returns the STAC datetime range string for a
// ±windowDays window around the given YYYY-MM-DD date.
func buildDatetimeInterval(date string, windowDays int) (string, error) {
	d, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return "", fmt.Errorf("invalid date %q: %w", date, err)
	}
	from := d.AddDate(0, 0, -windowDays)
	to := d.AddDate(0, 0, windowDays)
	return fmt.Sprintf("%sT00:00:00Z/%sT23:59:59Z",
		from.Format(time.DateOnly), to.Format(time.DateOnly)), nil
}

// doSearch executes a STAC POST /search request against baseURL and returns
// the raw feature list. Providers call this and then extract their own asset keys.
func doSearch(ctx context.Context, hc *http.Client, baseURL string, body stacSearchRequest) ([]stacRawFeature, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal STAC request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/search", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("build STAC request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(b))

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("STAC HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("STAC API returned HTTP %d", resp.StatusCode)
	}

	var result stacSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode STAC response: %w", err)
	}

	if len(result.Features) == 0 {
		return nil, fmt.Errorf("no scenes found")
	}

	return result.Features, nil
}
