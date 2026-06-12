package handler

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/qwezert/geogoservice/internal/cache"
	"github.com/qwezert/geogoservice/internal/geo"
	"github.com/qwezert/geogoservice/internal/render"
)

// ServeNDVIRaw handles GET /api/ndvi-raw?key=<minio_key>.
//
// Returns the raw float32 NDVI buffer for a tile as little-endian IEEE-754
// bytes, row-major, length = W×H.
//
// Self-healing: if the companion .ndvi.bin was not saved when the tile was
// originally rendered (tiles pre-dating this feature), the handler re-fetches
// the satellite bands from STAC, recomputes NDVI, persists .ndvi.bin to MinIO,
// and returns the data — transparently to the caller.
//
// Cache-Control is set to immutable so browsers never re-request the same tile.
func (rh *RenderHandler) ServeNDVIRaw(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing required query parameter: key", http.StatusBadRequest)
		return
	}
	if strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// ── Fast path: .ndvi.bin already in MinIO ────────────────────────────────
	rawBytes, err := rh.store.GetNDVIRaw(ctx, key)
	if err == nil {
		serveRawBytes(w, rawBytes)
		return
	}
	if !isNotFoundError(err) {
		http.Error(w, "storage error", http.StatusInternalServerError)
		fmt.Printf("[ndvi-raw] get raw key=%q: %v\n", key, err)
		return
	}

	// ── Slow path: re-render from STAC ───────────────────────────────────────
	// Look up the original tile parameters from PostgreSQL.
	rec, err := rh.store.GetTileByKey(ctx, key)
	if err != nil {
		if errors.Is(err, cache.ErrNotFound) {
			http.Error(w, "tile not found", http.StatusNotFound)
			return
		}
		http.Error(w, "database error", http.StatusInternalServerError)
		fmt.Printf("[ndvi-raw] get tile by key=%q: %v\n", key, err)
		return
	}

	// Transform to WGS84 for STAC search.
	bbox4326, err := geo.Transform3857To4326(rec.BBox)
	if err != nil {
		http.Error(w, "bbox transform error", http.StatusInternalServerError)
		return
	}

	// STAC search: find the same scene used when the tile was originally rendered.
	bands, err := rh.stacClient.FindBestScene(ctx, bbox4326, rec.Date,
		rh.defaultSearchWindowDays)
	if err != nil {
		http.Error(w, "STAC search failed: "+err.Error(), http.StatusBadGateway)
		fmt.Printf("[ndvi-raw] STAC search for key=%q: %v\n", key, err)
		return
	}

	// Read Red + NIR bands and compute NDVI (no PNG encoding needed).
	redBuf, err := geo.ReadBandWindow(bands.RedURL, bands.GDALConfigOpts, bbox4326, rec.W, rec.H)
	if err != nil {
		http.Error(w, "band read error", http.StatusBadGateway)
		fmt.Printf("[ndvi-raw] read Red key=%q: %v\n", key, err)
		return
	}
	nirBuf, err := geo.ReadBandWindow(bands.NIRURL, bands.GDALConfigOpts, bbox4326, rec.W, rec.H)
	if err != nil {
		http.Error(w, "band read error", http.StatusBadGateway)
		fmt.Printf("[ndvi-raw] read NIR key=%q: %v\n", key, err)
		return
	}

	ndvi, err := render.ComputeNDVI(redBuf, nirBuf)
	if err != nil {
		http.Error(w, "NDVI compute error", http.StatusInternalServerError)
		return
	}

	// Encode to little-endian float32 bytes.
	rawBytes = make([]byte, len(ndvi)*4)
	for i, v := range ndvi {
		binary.LittleEndian.PutUint32(rawBytes[i*4:], math.Float32bits(v))
	}

	// Persist async so next request is instant.
	rh.store.SaveNDVIRawAsync(key, rawBytes)
	fmt.Printf("[ndvi-raw] backfilled key=%q (%dx%d)\n", key, rec.W, rec.H)

	serveRawBytes(w, rawBytes)
}

func serveRawBytes(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "max-age=86400, immutable")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	_, _ = w.Write(data)
}

// isNotFoundError returns true when the MinIO error indicates the object does
// not exist (NoSuchKey / 404).
func isNotFoundError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "NoSuchKey") ||
		strings.Contains(s, "does not exist") ||
		strings.Contains(s, "not found")
}
