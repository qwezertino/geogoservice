// Package handler implements HTTP handlers for the tile rendering and management endpoints.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/qwezert/geogoservice/internal/cache"
	"github.com/qwezert/geogoservice/internal/config"
	"github.com/qwezert/geogoservice/internal/geo"
	"github.com/qwezert/geogoservice/internal/render"
	"github.com/qwezert/geogoservice/internal/stac"
)

// Both parameter formats are supported:
//
// Modern format (our API):
//
//	bbox=minX,minY,maxX,maxY  date=YYYY-MM-DD  w=256  h=256  index=ndvi
//
// Legacy GeoServer format:
//
//	box[0]=minX  box[1]=minY  box[2]=maxX  box[3]=maxY
//	date=<unix-timestamp>  width=256  height=256  indexName=ndvi

// RenderHandler holds the shared service dependencies.
type RenderHandler struct {
	store      *cache.Store
	stacClient *stac.Client
	sem        chan struct{}       // limits concurrent GDAL renders
	sf         singleflight.Group // deduplicates in-flight renders for the same tile
	svcCtx     context.Context    // service-level context; cancelled on shutdown

	defaultSearchWindowDays int
	maxAOICloud             float64 // AOI cloud fraction hard limit for scene skip (0–100)
	maxRenderAttempts       int     // render retry count per scene/index
}

// HandlerOptions carries configurable defaults for the render handler.
type HandlerOptions struct {
	DefaultSearchWindowDays int
	// MaxAOICloudCover is the AOI-level cloud % above which a scene is skipped (default 80).
	MaxAOICloudCover float64
	// MaxRenderAttempts is the number of render retries before marking a scene as failed (default 3).
	MaxRenderAttempts int
	// RenderWorkers caps concurrent GDAL render operations. 0 = runtime.NumCPU().
	RenderWorkers int
}

// New creates a RenderHandler with the given dependencies.
// ctx should be the application-level context so that background job goroutines
// are cancelled when the server shuts down.
func New(ctx context.Context, store *cache.Store, stacClient *stac.Client, opts HandlerOptions) *RenderHandler {
	searchWindow := opts.DefaultSearchWindowDays
	if searchWindow <= 0 {
		searchWindow = config.DefaultSTACSearchWindowDays
	}
	maxAOICloud := opts.MaxAOICloudCover
	if maxAOICloud <= 0 {
		maxAOICloud = config.DefaultMaxAOICloudCover
	}
	maxRenderAttempts := opts.MaxRenderAttempts
	if maxRenderAttempts <= 0 {
		maxRenderAttempts = config.DefaultMaxRenderAttempts
	}
	workers := opts.RenderWorkers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	return &RenderHandler{
		store:                   store,
		stacClient:              stacClient,
		sem:                     make(chan struct{}, workers),
		svcCtx:                  ctx,
		defaultSearchWindowDays: searchWindow,
		maxAOICloud:             maxAOICloud,
		maxRenderAttempts:       maxRenderAttempts,
	}
}

// ServeHTTP handles GET /api/render — checks cache, then renders on cache miss.
func (rh *RenderHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rh.handleSync(w, r)
}

// handleSync checks cache and, on miss, renders synchronously.
func (rh *RenderHandler) handleSync(w http.ResponseWriter, r *http.Request) {
	params, err := parseParams(r, rh.defaultSearchWindowDays)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	apiKey := APIKeyFromContext(ctx)
	polygonHash := geo.PolygonHash(params.polygon)
	palette, paletteHash, err := paletteForIndex(apiKey, params.index)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	tokenPrefix := tokenPrefixFor(apiKey)

	// ── 1. Cache check ────────────────────────────────────────────────────────
	if !params.noCache {
		hit, found, err := rh.store.Lookup(ctx, params.bbox3857, params.date, params.index, params.w, params.h, polygonHash, paletteHash)
		if err != nil {
			http.Error(w, "cache lookup error", http.StatusInternalServerError)
			fmt.Printf("[handler] cache lookup: %v\n", err)
			return
		}
		if found {
			pngBytes, err := rh.store.GetObject(ctx, hit.MinioKey)
			if err != nil {
				fmt.Printf("[handler] minio get failed, recomputing: %v\n", err)
			} else {
				writePNG(w, pngBytes)
				return
			}
		}
	}

	// ── 2. Singleflight: deduplicate concurrent renders for the same tile ────────
	sfKey := cache.BuildKey(tokenPrefix, params.bbox3857, params.date, params.index, params.w, params.h, polygonHash, paletteHash)
	type result struct{ png []byte }
	v, err, _ := rh.sf.Do(sfKey, func() (any, error) {
		select {
		case rh.sem <- struct{}{}:
			defer func() { <-rh.sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		res, renderErr := render.RenderTile(ctx, render.TileParams{
			BBox:             params.bbox3857,
			Date:             params.date,
			Index:            params.index,
			W:                params.w,
			H:                params.h,
			SearchWindowDays: params.searchWindowDays,
				Polygon:          params.polygon,
			Palette:          palette,
		}, rh.stacClient)
		if renderErr != nil {
			return nil, renderErr
		}

		var statsJSON []byte
		if res.Stats != nil {
			statsJSON, _ = json.Marshal(res.Stats)
		}
		rh.store.SaveAsync(params.bbox3857, params.date, params.index, params.w, params.h, res.PNG, res.RawValues, polygonHash, tokenPrefix, paletteHash, statsJSON, 0)
		return result{png: res.PNG}, nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "request cancelled", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "PNG render failed", http.StatusInternalServerError)
			fmt.Printf("[handler] render: %v\n", err)
		}
		return
	}
	writePNG(w, v.(result).png)
}

func writePNG(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// renderParams holds validated, parsed request parameters.
type renderParams struct {
	bbox3857         geo.BBox
	date             string
	index            string
	w, h             int
	searchWindowDays int
	noCache          bool // skip cache lookup and save (load-testing)
	polygon          []geo.LngLat
}

// parseParams accepts both the modern and legacy GeoServer parameter formats.
// defaultWindow is used when the caller does not supply the window query parameter.
func parseParams(r *http.Request, defaultWindow int) (*renderParams, error) {
	q := r.URL.Query()

	// ── bbox ─────────────────────────────────────────────────────────────────
	// Modern:  bbox=minX,minY,maxX,maxY
	// Legacy:  box[0]=minX  box[1]=minY  box[2]=maxX  box[3]=maxY
	bbox, err := parseBBox(q)
	if err != nil {
		return nil, err
	}

	// ── srs ──────────────────────────────────────────────────────────────────
	srs := q.Get("srs")
	if srs != "" && srs != "EPSG:3857" {
		return nil, fmt.Errorf("unsupported srs %q; only EPSG:3857 is accepted", srs)
	}

	// ── date ─────────────────────────────────────────────────────────────────
	// Modern:  date=YYYY-MM-DD
	// Legacy:  date=<unix timestamp in seconds>
	dateStr, err := parseDate(q.Get("date"))
	if err != nil {
		return nil, err
	}

	// ── width / height ───────────────────────────────────────────────────────
	// Modern:  w=256  h=256
	// Legacy:  width=256  height=256
	width, err := parseDimension(firstNonEmpty(q.Get("w"), q.Get("width")), "width")
	if err != nil {
		return nil, err
	}
	height, err := parseDimension(firstNonEmpty(q.Get("h"), q.Get("height")), "height")
	if err != nil {
		return nil, err
	}

	// ── index ────────────────────────────────────────────────────────────────
	// Modern:  index=ndvi
	// Legacy:  indexName=ndvi
	indexType := strings.ToLower(firstNonEmpty(q.Get("index"), q.Get("indexName")))
	if indexType == "" {
		indexType = "ndvi"
	}
	switch indexType {
	case "ndvi", "evi", "gndvi", "cvi", "tci", "soilmoisture":
		// valid
	default:
		return nil, fmt.Errorf("unsupported index %q; valid values: ndvi, evi, gndvi, cvi, tci, soilmoisture", indexType)
	}

	// ── STAC search overrides ─────────────────────────────────────────────────
	// window=N   — override search window in days
	// cloud=N    — override max cloud cover percent
	// nocache=1  — skip cache read and write (load-testing)
	searchWindow := defaultWindow
	if v := q.Get("window"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			searchWindow = n
		} else {
			return nil, fmt.Errorf("window must be a positive integer, got %q", v)
		}
	}

	noCache := q.Get("nocache") == "1" || q.Get("nocache") == "true"

	var polygon []geo.LngLat
	if polyStr := q.Get("polygon"); polyStr != "" {
		pts, err := parsePolygon(polyStr)
		if err != nil {
			return nil, fmt.Errorf("invalid polygon: %w", err)
		}
		polygon = pts
	}

	return &renderParams{
		bbox3857:         bbox,
		date:             dateStr,
		index:            indexType,
		w:                width,
		h:                height,
		searchWindowDays: searchWindow,
		noCache:          noCache,
		polygon:          polygon,
	}, nil
}

const maxPolygonVertices = 1000

// parsePolygon parses a flat "lng1,lat1,lng2,lat2,..." string into LngLat pairs.
// Requires at least 3 pairs (6 values) and at most maxPolygonVertices pairs.
func parsePolygon(raw string) ([]geo.LngLat, error) {
	parts := strings.Split(raw, ",")
	if len(parts) < 6 || len(parts)%2 != 0 {
		return nil, fmt.Errorf("need at least 3 lng,lat pairs (%d values given)", len(parts))
	}
	if len(parts)/2 > maxPolygonVertices {
		return nil, fmt.Errorf("polygon exceeds maximum of %d vertices (%d given)", maxPolygonVertices, len(parts)/2)
	}
	pts := make([]geo.LngLat, len(parts)/2)
	for i := range pts {
		lng, err := strconv.ParseFloat(strings.TrimSpace(parts[i*2]), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid longitude at pair %d: %q", i, parts[i*2])
		}
		lat, err := strconv.ParseFloat(strings.TrimSpace(parts[i*2+1]), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid latitude at pair %d: %q", i, parts[i*2+1])
		}
		if lng < -180 || lng > 180 {
			return nil, fmt.Errorf("longitude %v at pair %d out of range [-180, 180]", lng, i)
		}
		if lat <= -90 || lat >= 90 {
			return nil, fmt.Errorf("latitude %v at pair %d out of range (-90, 90); poles are not supported", lat, i)
		}
		pts[i] = geo.LngLat{lng, lat}
	}
	return pts, nil
}

// parseBBox parses bbox from either format:
//   - modern:  bbox=minX,minY,maxX,maxY
//   - legacy:  box[0]=minX  box[1]=minY  box[2]=maxX  box[3]=maxY
func parseBBox(q interface{ Get(string) string }) (geo.BBox, error) {
	var coords [4]float64

	if bboxStr := q.Get("bbox"); bboxStr != "" {
		// Modern format
		parts := strings.Split(bboxStr, ",")
		if len(parts) != 4 {
			return geo.BBox{}, errors.New("bbox must have exactly 4 comma-separated values")
		}
		for i, p := range parts {
			v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
			if err != nil {
				return geo.BBox{}, fmt.Errorf("invalid bbox value %q: %w", p, err)
			}
			coords[i] = v
		}
	} else if v := q.Get("box[0]"); v != "" {
		// Legacy GeoServer format
		keys := [4]string{"box[0]", "box[1]", "box[2]", "box[3]"}
		for i, key := range keys {
			val := q.Get(key)
			if val == "" {
				return geo.BBox{}, fmt.Errorf("missing legacy bbox parameter: %s", key)
			}
			f, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return geo.BBox{}, fmt.Errorf("invalid %s value %q: %w", key, val, err)
			}
			coords[i] = f
		}
	} else {
		return geo.BBox{}, errors.New("missing bbox: provide either bbox=X1,Y1,X2,Y2 or box[0..3]=...")
	}

	bbox := geo.BBox{MinX: coords[0], MinY: coords[1], MaxX: coords[2], MaxY: coords[3]}
	if bbox.MinX >= bbox.MaxX || bbox.MinY >= bbox.MaxY {
		return geo.BBox{}, errors.New("bbox is degenerate (minX>=maxX or minY>=maxY)")
	}
	return bbox, nil
}

// parseDate accepts either YYYY-MM-DD or a Unix timestamp (seconds).
func parseDate(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("missing required parameter: date")
	}

	// Try YYYY-MM-DD first
	if _, err := time.Parse("2006-01-02", raw); err == nil {
		return raw, nil
	}

	// Try Unix timestamp (legacy format)
	ts, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return "", fmt.Errorf("date must be YYYY-MM-DD or a Unix timestamp, got %q", raw)
	}
	return time.Unix(ts, 0).UTC().Format("2006-01-02"), nil
}

// parseDimension parses and validates a pixel dimension value.
func parseDimension(raw, name string) (int, error) {
	if raw == "" {
		return 0, fmt.Errorf("missing required parameter: %s (or its alias)", name)
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1 || v > 2048 {
		return 0, fmt.Errorf("%s must be an integer in [1, 2048]", name)
	}
	return v, nil
}

// firstNonEmpty returns the first non-empty string from the arguments.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
