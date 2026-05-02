package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/qwezert/geogoservice/internal/geo"
	"github.com/qwezert/geogoservice/internal/render"
)

const maxBatchSize = 100

// BatchRequest describes a single tile within a batch request.
type BatchRequest struct {
	// MinioKey, when provided, bypasses all rendering and cache lookups: the PNG
	// is fetched directly from MinIO/Redis. Use this when you already have a key
	// from GET /api/catalog — no STAC call will be made.
	MinioKey string `json:"minio_key,omitempty"`

	BBox  [4]float64 `json:"bbox"`  // [minX, minY, maxX, maxY] EPSG:3857
	Date  string     `json:"date"`  // YYYY-MM-DD
	Index string     `json:"index"` // e.g. "ndvi"
	W     int        `json:"w"`
	H     int        `json:"h"`

	// Optional — server defaults are used when zero.
	SearchWindowDays int     `json:"search_window_days,omitempty"`
	MaxCloudCover    float64 `json:"max_cloud_cover,omitempty"`
	// Polygon is an optional WGS-84 clipping polygon as [longitude, latitude] pairs.
	// Pixels outside the polygon become transparent in the rendered PNG.
	Polygon [][2]float64 `json:"polygon,omitempty"`
}

// BatchResult is the outcome for a single tile.
// On success Data contains the base64-encoded PNG.
// On failure Error contains the reason; Data is empty.
type BatchResult struct {
	Index  int    `json:"index"`
	Data   string `json:"data,omitempty"` // base64-encoded PNG
	Cached bool   `json:"cached"`
	Error  string `json:"error,omitempty"`
}

// ServeBatch handles POST /api/render/batch.
//
// It accepts a JSON array of BatchRequest, renders all tiles in parallel
// (subject to the shared semaphore), and returns a JSON array of BatchResult
// only when every tile has been processed. The client can therefore set all
// <img src> attributes at once, making all tiles appear simultaneously.
//
// Request body (JSON array, max 100 items):
//
//	[{"bbox":[minX,minY,maxX,maxY],"date":"2024-06-15","index":"ndvi","w":512,"h":512}, ...]
//
// Response (JSON array, same order as input):
//
//	[{"index":0,"data":"<base64 PNG>","cached":true}, {"index":1,"error":"..."}, ...]
func (rh *RenderHandler) ServeBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var requests []BatchRequest
	if err := json.NewDecoder(r.Body).Decode(&requests); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(requests) == 0 {
		http.Error(w, "empty batch", http.StatusBadRequest)
		return
	}
	if len(requests) > maxBatchSize {
		http.Error(w, fmt.Sprintf("batch exceeds limit of %d tiles", maxBatchSize), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	results := make([]BatchResult, len(requests))

	var wg sync.WaitGroup
	for i, req := range requests {
		wg.Add(1)
		go func(idx int, req BatchRequest) {
			defer wg.Done()
			results[idx] = rh.processBatchItem(ctx, idx, req)
		}(i, req)
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(results); err != nil {
		fmt.Printf("[batch] encode response: %v\n", err)
	}
}

// processBatchItem renders (or retrieves from cache) a single tile.
// It participates in the shared semaphore so batch renders don't starve
// single-tile requests and vice-versa.
func (rh *RenderHandler) processBatchItem(ctx context.Context, idx int, req BatchRequest) BatchResult {
	// ── 0. Direct MinIO fetch (catalog flow) ─────────────────────────────────
	// When minio_key is provided we skip rendering entirely: the tile is already
	// cached and no STAC call will be made.
	if req.MinioKey != "" {
		pngBytes, err := rh.store.GetObject(ctx, req.MinioKey)
		if err != nil {
			return BatchResult{Index: idx, Error: "fetch failed: " + err.Error()}
		}
		return BatchResult{
			Index:  idx,
			Data:   base64.StdEncoding.EncodeToString(pngBytes),
			Cached: true,
		}
	}

	// Validate required fields.
	if req.Date == "" {
		return BatchResult{Index: idx, Error: "missing required field: date"}
	}
	if _, err := parseDate(req.Date); err != nil {
		return BatchResult{Index: idx, Error: "invalid date: " + err.Error()}
	}
	if req.Index == "" {
		return BatchResult{Index: idx, Error: "missing required field: index"}
	}
	if req.W <= 0 || req.H <= 0 {
		return BatchResult{Index: idx, Error: "w and h must be positive"}
	}

	bbox := geo.BBox{
		MinX: req.BBox[0],
		MinY: req.BBox[1],
		MaxX: req.BBox[2],
		MaxY: req.BBox[3],
	}

	searchWindow := req.SearchWindowDays
	if searchWindow <= 0 {
		searchWindow = rh.defaultSearchWindowDays
	}
	maxCloud := req.MaxCloudCover
	if maxCloud <= 0 {
		maxCloud = rh.defaultMaxCloudCover
	}

	polygon := make([]geo.LngLat, len(req.Polygon))
	for i, pt := range req.Polygon {
		polygon[i] = geo.LngLat{pt[0], pt[1]}
	}
	polygonHash := geo.PolygonHash(polygon)

	// ── 1. Cache check ────────────────────────────────────────────────────────
	hit, found, err := rh.store.Lookup(ctx, bbox, req.Date, req.Index, req.W, req.H, polygonHash)
	if err == nil && found {
		pngBytes, err := rh.store.GetObject(ctx, hit.MinioKey)
		if err == nil {
			return BatchResult{
				Index:  idx,
				Data:   base64.StdEncoding.EncodeToString(pngBytes),
				Cached: true,
			}
		}
		// Cache inconsistency — fall through to render.
		fmt.Printf("[batch] minio get failed for idx %d, re-rendering: %v\n", idx, err)
	}

	// ── 2. Acquire render slot ────────────────────────────────────────────────
	select {
	case rh.sem <- struct{}{}:
		defer func() { <-rh.sem }()
	case <-ctx.Done():
		return BatchResult{Index: idx, Error: "request cancelled"}
	}

	// ── 3. Render ─────────────────────────────────────────────────────────────
	pngBytes, err := render.RenderTile(ctx, render.TileParams{
		BBox:             bbox,
		Date:             req.Date,
		Index:            req.Index,
		W:                req.W,
		H:                req.H,
		SearchWindowDays: searchWindow,
		MaxCloudCover:    maxCloud,
		Polygon:          polygon,
	}, rh.stacClient)
	if err != nil {
		return BatchResult{Index: idx, Error: err.Error()}
	}

	rh.store.SaveAsync(bbox, req.Date, req.Index, req.W, req.H, pngBytes, polygonHash)

	return BatchResult{
		Index: idx,
		Data:  base64.StdEncoding.EncodeToString(pngBytes),
	}
}
