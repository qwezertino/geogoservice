// Package handler implements the HTTP request handler for the /api/render endpoint.
package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/qwezert/geogoservice/internal/cache"
	"github.com/qwezert/geogoservice/internal/geo"
	"github.com/qwezert/geogoservice/internal/render"
	"github.com/qwezert/geogoservice/internal/stac"
)

// RenderHandler holds the shared service dependencies.
type RenderHandler struct {
	store      *cache.Store
	stacClient *stac.Client
}

// New creates a RenderHandler with the given dependencies.
func New(store *cache.Store, stacClient *stac.Client) *RenderHandler {
	return &RenderHandler{store: store, stacClient: stacClient}
}

// ServeHTTP handles GET /api/render requests.
//
// Required query parameters:
//
//	bbox  – comma-separated minX,minY,maxX,maxY in EPSG:3857
//	srs   – must be "EPSG:3857"
//	date  – YYYY-MM-DD
//	w     – output width  in pixels (1–2048)
//	h     – output height in pixels (1–2048)
//	index – spectral index type ("ndvi")
func (rh *RenderHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	params, err := parseParams(r)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// ── 1. Cache check ────────────────────────────────────────────────────────
	hit, found, err := rh.store.Lookup(ctx, params.bbox3857, params.date, params.index, params.w, params.h)
	if err != nil {
		http.Error(w, "cache lookup error", http.StatusInternalServerError)
		fmt.Printf("[handler] cache lookup: %v\n", err)
		return
	}
	if found {
		pngBytes, err := rh.store.GetObject(ctx, hit.MinioKey)
		if err != nil {
			// Cache inconsistency – fall through and recompute
			fmt.Printf("[handler] minio get failed, recomputing: %v\n", err)
		} else {
			writePNG(w, pngBytes)
			return
		}
	}

	// ── 2. Transform bbox 3857 → 4326 for STAC query ─────────────────────────
	bbox4326, err := geo.Transform3857To4326(params.bbox3857)
	if err != nil {
		http.Error(w, "coordinate transform error", http.StatusInternalServerError)
		fmt.Printf("[handler] transform: %v\n", err)
		return
	}

	// ── 3. STAC query ────────────────────────────────────────────────────────
	bands, err := rh.stacClient.FindBestScene(ctx, bbox4326, params.date)
	if err != nil {
		http.Error(w, "no satellite imagery found: "+err.Error(), http.StatusNotFound)
		fmt.Printf("[handler] STAC query: %v\n", err)
		return
	}

	// ── 4. COG reads via GDAL /vsicurl/ (HTTP range requests) ────────────────
	redBuf, err := geo.ReadBandWindow(bands.RedURL, bbox4326, params.w, params.h)
	if err != nil {
		http.Error(w, "failed to read Red band", http.StatusInternalServerError)
		fmt.Printf("[handler] read Red: %v\n", err)
		return
	}

	nirBuf, err := geo.ReadBandWindow(bands.NIRURL, bbox4326, params.w, params.h)
	if err != nil {
		http.Error(w, "failed to read NIR band", http.StatusInternalServerError)
		fmt.Printf("[handler] read NIR: %v\n", err)
		return
	}

	// ── 5. NDVI computation ───────────────────────────────────────────────────
	ndvi, err := render.ComputeNDVI(redBuf, nirBuf)
	if err != nil {
		http.Error(w, "NDVI computation failed", http.StatusInternalServerError)
		fmt.Printf("[handler] ndvi: %v\n", err)
		return
	}

	// ── 6. Colour render → PNG ────────────────────────────────────────────────
	pngBytes, err := render.RenderPNG(ndvi, params.w, params.h)
	if err != nil {
		http.Error(w, "PNG render failed", http.StatusInternalServerError)
		fmt.Printf("[handler] render png: %v\n", err)
		return
	}

	// ── 7. Async cache save ───────────────────────────────────────────────────
	rh.store.SaveAsync(params.bbox3857, params.date, params.index, params.w, params.h, pngBytes)

	// ── 8. Return PNG ─────────────────────────────────────────────────────────
	writePNG(w, pngBytes)
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
	bbox3857 geo.BBox
	date     string
	index    string
	w, h     int
}

func parseParams(r *http.Request) (*renderParams, error) {
	q := r.URL.Query()

	// bbox
	bboxStr := q.Get("bbox")
	if bboxStr == "" {
		return nil, errors.New("missing required parameter: bbox")
	}
	parts := strings.Split(bboxStr, ",")
	if len(parts) != 4 {
		return nil, errors.New("bbox must have exactly 4 comma-separated values")
	}
	coords := make([]float64, 4)
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid bbox value %q: %w", p, err)
		}
		coords[i] = v
	}
	bbox := geo.BBox{MinX: coords[0], MinY: coords[1], MaxX: coords[2], MaxY: coords[3]}
	if bbox.MinX >= bbox.MaxX || bbox.MinY >= bbox.MaxY {
		return nil, errors.New("bbox is degenerate (minX>=maxX or minY>=maxY)")
	}

	// srs
	srs := q.Get("srs")
	if srs != "" && srs != "EPSG:3857" {
		return nil, fmt.Errorf("unsupported srs %q; only EPSG:3857 is accepted", srs)
	}

	// date
	dateStr := q.Get("date")
	if dateStr == "" {
		return nil, errors.New("missing required parameter: date")
	}
	if _, err := time.Parse("2006-01-02", dateStr); err != nil {
		return nil, fmt.Errorf("date must be YYYY-MM-DD: %w", err)
	}

	// w / h
	wStr, hStr := q.Get("w"), q.Get("h")
	if wStr == "" || hStr == "" {
		return nil, errors.New("missing required parameters: w and h")
	}
	width, err := strconv.Atoi(wStr)
	if err != nil || width < 1 || width > 2048 {
		return nil, errors.New("w must be an integer in [1, 2048]")
	}
	height, err := strconv.Atoi(hStr)
	if err != nil || height < 1 || height > 2048 {
		return nil, errors.New("h must be an integer in [1, 2048]")
	}

	// index
	indexType := q.Get("index")
	if indexType == "" {
		indexType = "ndvi"
	}
	if indexType != "ndvi" {
		return nil, fmt.Errorf("unsupported index %q; only 'ndvi' is supported", indexType)
	}

	return &renderParams{
		bbox3857: bbox,
		date:     dateStr,
		index:    indexType,
		w:        width,
		h:        height,
	}, nil
}
