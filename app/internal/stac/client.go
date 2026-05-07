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

// Provider name constants — used in log output.
const (
	ProviderPlanetaryComputer = "planetary-computer"
	ProviderEarthSearch       = "earth-search"
	ProviderCDSE              = "cdse"
)

// Sentinel2Collection is the Sentinel-2 Level-2A collection ID used by all providers.
const Sentinel2Collection = "sentinel-2-l2a"

// BandURLs holds the ready-to-use paths for the Red (B04) and NIR (B08) bands
// of a Sentinel-2 scene, plus optional GDAL config options required to open them.
//
// RedURL and NIRURL may be plain HTTPS URLs (for public providers) or GDAL
// virtual-filesystem paths such as /vsis3/bucket/key (for CDSE).
//
// GDALConfigOpts carries zero or more "KEY=VALUE" strings that are applied as
// thread-local GDAL config options before each Open call. Public providers
// leave this nil; CDSE sets the S3 endpoint and credentials here.
type BandURLs struct {
	RedURL         string
	NIRURL         string
	GDALConfigOpts []string // "KEY=VALUE" pairs forwarded to GDAL, or nil
	ProviderName   string   // name of the provider that returned these URLs
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

// Client tries the preferred provider first. If that fails, it races all
// remaining providers in parallel and returns whichever answers first.
//
// This gives the best of both worlds: low latency on the happy path (no
// unnecessary requests), and fast automatic recovery when the preferred
// provider is down or slow.
type Client struct {
	preferred Provider   // tried first; nil when no preference is set
	others    []Provider // raced in parallel when preferred fails
}

// NewClient builds a Client with an optional preferred provider.
//
// preferredName controls which provider is tried first on every request.
// Accepted values: "planetary-computer" (default), "earth-search", "cdse".
// If the preferred provider returns an error, the remaining providers are
// raced in parallel and the fastest successful result is returned.
//
// cdseS3AccessKey and cdseS3SecretKey are long-lived S3 credentials for the
// CDSE object-storage endpoint (eodata.dataspace.copernicus.eu). Generate them
// once at https://eodata-s3keysmanager.dataspace.copernicus.eu/. Pass empty
// strings to disable the CDSE provider.
func NewClient(preferredName string, httpClient *http.Client, cdseS3AccessKey, cdseS3SecretKey string) *Client {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{MaxIdleConnsPerHost: 10},
		}
	}

	all := []Provider{
		newPlanetaryComputerProvider(httpClient),
		newEarthSearchProvider(httpClient),
	}
	if cdseS3AccessKey != "" && cdseS3SecretKey != "" {
		all = append(all, newCDSEProvider(httpClient, cdseS3AccessKey, cdseS3SecretKey))
	}

	var preferred Provider
	var others []Provider
	for _, p := range all {
		if p.Name() == preferredName {
			preferred = p
		} else {
			others = append(others, p)
		}
	}
	if preferred == nil {
		// Requested provider not available (e.g. cdse without credentials).
		fmt.Printf("[stac] preferred provider %q not available; will race all\n", preferredName)
		return &Client{others: all}
	}
	return &Client{preferred: preferred, others: others}
}

// FindBestSceneFallback races all non-preferred providers. Called when the
// preferred provider returned URLs that subsequently failed at the read stage
// (e.g. transient S3 error). The previously-used provider name is skipped so
// we don't retry the same broken source.
func (c *Client) FindBestSceneFallback(ctx context.Context, bbox geo.BBox, date string, windowDays int, maxCloud float64, skipProvider string) (*BandURLs, error) {
	var pool []Provider
	if c.preferred != nil && c.preferred.Name() != skipProvider {
		pool = append(pool, c.preferred)
	}
	for _, p := range c.others {
		if p.Name() != skipProvider {
			pool = append(pool, p)
		}
	}
	if len(pool) == 0 {
		return nil, fmt.Errorf("no fallback providers available (skipped %q)", skipProvider)
	}
	if len(pool) == 1 {
		b, err := pool[0].FindBestScene(ctx, bbox, date, windowDays, maxCloud)
		if err == nil {
			fmt.Printf("[stac] fallback provider %q succeeded\n", pool[0].Name())
			b.ProviderName = pool[0].Name()
		}
		return b, err
	}

	type result struct {
		bands    *BandURLs
		err      error
		provider string
	}
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan result, len(pool))
	for _, p := range pool {
		p := p
		go func() {
			b, err := p.FindBestScene(raceCtx, bbox, date, windowDays, maxCloud)
			ch <- result{bands: b, err: err, provider: p.Name()}
		}()
	}
	var lastErr error
	for range pool {
		r := <-ch
		if r.err == nil {
			fmt.Printf("[stac] fallback provider %q succeeded\n", r.provider)
			cancel()
			r.bands.ProviderName = r.provider
			return r.bands, nil
		}
		lastErr = r.err
	}
	return nil, fmt.Errorf("all fallback providers failed; last error: %w", lastErr)
}

// FindBestScene tries the preferred provider first. On failure it races all
// remaining providers simultaneously and returns the fastest successful result.
func (c *Client) FindBestScene(ctx context.Context, bbox geo.BBox, date string, windowDays int, maxCloud float64) (*BandURLs, error) {
	// Fast path: preferred provider.
	if c.preferred != nil {
		bands, err := c.preferred.FindBestScene(ctx, bbox, date, windowDays, maxCloud)
		if err == nil {
			fmt.Printf("[stac] provider %q succeeded\n", c.preferred.Name())
			bands.ProviderName = c.preferred.Name()
			return bands, nil
		}
		fmt.Printf("[stac] preferred provider %q failed: %v — racing fallbacks\n", c.preferred.Name(), err)
	}

	// Fallback: race remaining providers.
	pool := c.others
	if len(pool) == 0 {
		return nil, fmt.Errorf("all STAC providers failed")
	}
	if len(pool) == 1 {
		bands, err := pool[0].FindBestScene(ctx, bbox, date, windowDays, maxCloud)
		if err == nil {
			bands.ProviderName = pool[0].Name()
		}
		return bands, err
	}

	type result struct {
		bands    *BandURLs
		err      error
		provider string
	}

	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch := make(chan result, len(pool))
	for _, p := range pool {
		p := p
		go func() {
			b, err := p.FindBestScene(raceCtx, bbox, date, windowDays, maxCloud)
			ch <- result{bands: b, err: err, provider: p.Name()}
		}()
	}

	var lastErr error
	for range pool {
		r := <-ch
		if r.err == nil {
			fmt.Printf("[stac] fallback provider %q won the race\n", r.provider)
			cancel()
			r.bands.ProviderName = r.provider
			return r.bands, nil
		}
		fmt.Printf("[stac] provider %q failed: %v\n", r.provider, r.err)
		lastErr = r.err
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
