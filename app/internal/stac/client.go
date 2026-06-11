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
	"sort"
	"time"

	"github.com/qwezert/geogoservice/internal/geo"
)

// Provider name constants — used in log output.
const (
	ProviderPlanetaryComputer = "planetary-computer"
	ProviderEarthSearch       = "earth-search"
	ProviderCDSE              = "cdse"
)

// Satellite collection IDs used across all providers.
const (
	Sentinel2Collection = "sentinel-2-l2a"
	LandsatCollection   = "landsat-c2-l2"
)

// BandURLs holds the ready-to-use paths for the Sentinel-2 bands needed by
// each spectral index, plus GDAL config options to open them.
//
// All URL fields may be plain HTTPS URLs (public providers) or GDAL
// virtual-filesystem paths like /vsis3/bucket/key (CDSE). Empty string means
// the band was not found in the STAC item — callers should check before use.
//
// GDALConfigOpts carries zero or more "KEY=VALUE" strings applied as
// thread-local GDAL config options before each Open call.
type BandURLs struct {
	// Core bands (always present for L2A)
	RedURL string // B04 (10 m)
	NIRURL string // B08 (10 m)
	// Optional bands — filled when the STAC item exposes them
	BlueURL  string // B02 (10 m) — required for EVI, TCI
	GreenURL string // B03 (10 m) — required for GNDVI, CVI, TCI
	SWIRURL  string // B11 (20 m) — required for soilMoisture
	// SCLURL is the optional URL/path to the Scene Classification Layer (SCL)
	// band. When set, the job runner reads this at low resolution over the AOI
	// to measure actual (local) cloud cover before rendering.
	SCLURL         string
	GDALConfigOpts []string // "KEY=VALUE" pairs forwarded to GDAL, or nil
	ProviderName   string   // name of the provider that returned these URLs
}

// SceneInfo is a single satellite scene returned by FindAllScenes.
type SceneInfo struct {
	Date       string  // YYYY-MM-DD (UTC acquisition date)
	CloudCover float64 // scene-level cloud cover % from STAC metadata
	SceneID    string  // STAC item ID (e.g. "S2B_MSIL2A_20240115T...")
	Bands      *BandURLs
}

// Provider is the interface every STAC data source must implement.
// To add a new provider:
//  1. Create internal/stac/provider_<name>.go
//  2. Implement Name(), FindBestScene(), and FindScenesInRange()
//  3. Instantiate and add it to the ordered slice in NewClient
type Provider interface {
	// Name returns the human-readable identifier used in logs.
	Name() string
	// FindBestScene returns COG URLs for the least-cloudy scene that intersects
	// bbox (EPSG:4326) within ±windowDays of date.
	FindBestScene(ctx context.Context, bbox geo.BBox, date string, windowDays int) (*BandURLs, error)
	// FindScenesInRange returns one SceneInfo per unique acquisition date in
	// [startDate, endDate], picking the least-cloudy scene per date.
	// A single STAC /search request is issued — no per-date round trips.
	FindScenesInRange(ctx context.Context, bbox geo.BBox, startDate, endDate string) ([]SceneInfo, error)
}

// Client tries the preferred provider first. If that fails, it races all
// remaining providers in parallel and returns whichever answers first.
//
// This gives the best of both worlds: low latency on the happy path (no
// unnecessary requests), and fast automatic recovery when the preferred
// provider is down or slow.
type Client struct {
	preferred Provider   // tried first; nil when no preference is set
	others    []Provider // tried in order when preferred fails (PC → EarthSearch → CDSE)
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

// FindBestSceneFallback tries each remaining provider sequentially, skipping
// the one that already failed at the band-read stage. Order: PC → EarthSearch → CDSE.
func (c *Client) FindBestSceneFallback(ctx context.Context, bbox geo.BBox, date string, windowDays int, skipProvider string) (*BandURLs, error) {
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
	var lastErr error
	for _, p := range pool {
		b, err := p.FindBestScene(ctx, bbox, date, windowDays)
		if err == nil {
			fmt.Printf("[stac] fallback provider %q succeeded\n", p.Name())
			b.ProviderName = p.Name()
			return b, nil
		}
		fmt.Printf("[stac] fallback provider %q failed: %v\n", p.Name(), err)
		lastErr = err
	}
	return nil, fmt.Errorf("all fallback providers failed; last error: %w", lastErr)
}

// raceProviders starts FindBestScene on all providers in parallel and returns
// the first successful result. Available for future use if low-latency matters
// more than cost — currently unused in favour of sequential fallback.
func raceProviders(ctx context.Context, pool []Provider, bbox geo.BBox, date string, windowDays int) (*BandURLs, error) {
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
			b, err := p.FindBestScene(raceCtx, bbox, date, windowDays)
			ch <- result{bands: b, err: err, provider: p.Name()}
		}()
	}
	var lastErr error
	for range pool {
		r := <-ch
		if r.err == nil {
			fmt.Printf("[stac] race: provider %q won\n", r.provider)
			cancel()
			r.bands.ProviderName = r.provider
			return r.bands, nil
		}
		fmt.Printf("[stac] race: provider %q failed: %v\n", r.provider, r.err)
		lastErr = r.err
	}
	return nil, fmt.Errorf("all providers failed in race; last error: %w", lastErr)
}

// FindBestScene tries providers sequentially: PC → EarthSearch → CDSE.
// Each provider is tried only after the previous one fails, avoiding
// unnecessary parallel requests to slower sources.
func (c *Client) FindBestScene(ctx context.Context, bbox geo.BBox, date string, windowDays int) (*BandURLs, error) {
	var all []Provider
	if c.preferred != nil {
		all = append(all, c.preferred)
	}
	all = append(all, c.others...)

	var lastErr error
	for _, p := range all {
		bands, err := p.FindBestScene(ctx, bbox, date, windowDays)
		if err == nil {
			fmt.Printf("[stac] provider %q succeeded\n", p.Name())
			bands.ProviderName = p.Name()
			return bands, nil
		}
		fmt.Printf("[stac] provider %q failed: %v\n", p.Name(), err)
		lastErr = err
	}
	return nil, fmt.Errorf("all STAC providers failed; last error: %w", lastErr)
}

// providerSearchTimeout is the per-provider deadline for FindAllScenes.
// Each provider gets this budget independently — a slow provider does not
// eat into the budget of the next one.
const providerSearchTimeout = 60 * time.Second

// FindAllScenes issues a single STAC search across the full date range and
// returns one SceneInfo per unique acquisition date (least-cloudy scene wins
// when multiple scenes exist for the same date). The preferred provider is
// tried first; on failure the others are tried in order.
// Each provider gets its own providerSearchTimeout so a hanging provider
// does not block the fallback chain.
func (c *Client) FindAllScenes(ctx context.Context, bbox geo.BBox, startDate, endDate string) ([]SceneInfo, error) {
	providers := make([]Provider, 0)
	if c.preferred != nil {
		providers = append(providers, c.preferred)
	}
	providers = append(providers, c.others...)

	for _, p := range providers {
		pCtx, pCancel := context.WithTimeout(ctx, providerSearchTimeout)
		scenes, err := p.FindScenesInRange(pCtx, bbox, startDate, endDate)
		pCancel()
		if err != nil {
			fmt.Printf("[stac] FindAllScenes: provider %q failed: %v\n", p.Name(), err)
			continue
		}
		for i := range scenes {
			scenes[i].Bands.ProviderName = p.Name()
		}
		fmt.Printf("[stac] FindAllScenes: provider %q returned %d scenes\n", p.Name(), len(scenes))
		return scenes, nil
	}
	return nil, fmt.Errorf("all providers failed for FindAllScenes %s/%s", startDate, endDate)
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
	ID         string                `json:"id"`
	Collection string                `json:"collection"`
	Assets     map[string]*stacAsset `json:"assets"`
	Properties struct {
		Datetime   string  `json:"datetime"`
		CloudCover float64 `json:"eo:cloud_cover"`
	} `json:"properties"`
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

// buildDatetimeRange returns the STAC datetime range string for [startDate, endDate].
func buildDatetimeRange(startDate, endDate string) (string, error) {
	if _, err := time.Parse(time.DateOnly, startDate); err != nil {
		return "", fmt.Errorf("invalid start_date %q: %w", startDate, err)
	}
	if _, err := time.Parse(time.DateOnly, endDate); err != nil {
		return "", fmt.Errorf("invalid end_date %q: %w", endDate, err)
	}
	return fmt.Sprintf("%sT00:00:00Z/%sT23:59:59Z", startDate, endDate), nil
}

// findScenesInRangeHelper executes a single STAC /search for the full date range,
// groups results by acquisition date, picks the least-cloudy scene per date,
// and applies extractFn to produce BandURLs. Result is sorted by date ascending.
// extractFn receives a context so token-based providers (PC) can refresh lazily.
func findScenesInRangeHelper(
	ctx context.Context,
	hc *http.Client,
	baseURL string,
	bbox geo.BBox,
	startDate, endDate string,
	collections []string,
	extractFn func(context.Context, stacRawFeature) (*BandURLs, error),
) ([]SceneInfo, error) {
	datetime, err := buildDatetimeRange(startDate, endDate)
	if err != nil {
		return nil, err
	}

	// One request for all scenes in the range. We do not filter by cloud cover
	// here because the STAC metadata reflects the full S2 tile (~110×110 km),
	// not the user's (typically small) AOI. Per-AOI cloud checking is done later
	// using the SCL band in the job runner. Sorting by cloud cover ascending still
	// ensures the least-cloudy scene wins when multiple scenes share a date.
	features, err := doSearch(ctx, hc, baseURL, stacSearchRequest{
		Collections: collections,
		Datetime:    datetime,
		BBox:        [4]float64{bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY},
		SortBy:      []stacSortBy{{Field: "properties.eo:cloud_cover", Direction: "asc"}},
		Limit:       200,
	})
	if err != nil {
		return nil, err
	}

	// Group by date. Sentinel-2 beats Landsat for the same date (better resolution);
	// within the same constellation, prefer lower cloud cover.
	type bestFeature struct {
		f    stacRawFeature
		cc   float64
		isS2 bool
	}
	byDate := make(map[string]bestFeature, len(features))
	for _, f := range features {
		dt := f.Properties.Datetime
		if len(dt) < 10 {
			continue
		}
		date := dt[:10]
		cc := f.Properties.CloudCover
		isS2 := f.Collection == Sentinel2Collection
		existing, ok := byDate[date]
		if !ok {
			byDate[date] = bestFeature{f: f, cc: cc, isS2: isS2}
			continue
		}
		// S2 always wins over Landsat; within same constellation pick lowest cloud.
		if (!existing.isS2 && isS2) || (existing.isS2 == isS2 && cc < existing.cc) {
			byDate[date] = bestFeature{f: f, cc: cc, isS2: isS2}
		}
	}

	// Sort dates for deterministic output.
	dates := make([]string, 0, len(byDate))
	for d := range byDate {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	scenes := make([]SceneInfo, 0, len(dates))
	for _, d := range dates {
		b := byDate[d]
		bands, err := extractFn(ctx, b.f)
		if err != nil {
			fmt.Printf("[stac] findScenesInRange: skip %s — extract failed: %v\n", d, err)
			continue
		}
		scenes = append(scenes, SceneInfo{Date: d, CloudCover: b.cc, SceneID: b.f.ID, Bands: bands})
	}

	if len(scenes) == 0 {
		return nil, fmt.Errorf("no usable scenes found in %s/%s", startDate, endDate)
	}
	return scenes, nil
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
