package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ServeCatalog handles GET /api/catalog?year=2024&bbox=minLng,minLat,maxLng,maxLat
//
// Returns a JSON array of cached NDVI tiles from PostgreSQL that intersect the
// given WGS-84 bbox and were acquired in the given year.
// Always returns [] (not null) when there are no results.
func (rh *RenderHandler) ServeCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()

	// ── year ─────────────────────────────────────────────────────────────────
	yearStr := q.Get("year")
	if yearStr == "" {
		http.Error(w, "missing required parameter: year", http.StatusBadRequest)
		return
	}
	year, err := strconv.Atoi(yearStr)
	if err != nil || year < 2015 || year > 2100 {
		http.Error(w, "year must be an integer between 2015 and 2100", http.StatusBadRequest)
		return
	}

	// ── bbox ─────────────────────────────────────────────────────────────────
	bboxStr := q.Get("bbox")
	if bboxStr == "" {
		http.Error(w, "missing required parameter: bbox", http.StatusBadRequest)
		return
	}
	parts := strings.Split(bboxStr, ",")
	if len(parts) != 4 {
		http.Error(w, "bbox must be 4 comma-separated floats: minLng,minLat,maxLng,maxLat", http.StatusBadRequest)
		return
	}
	var bbox [4]float64
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			http.Error(w, fmt.Sprintf("bbox[%d] is not a valid float: %q", i, p), http.StatusBadRequest)
			return
		}
		bbox[i] = v
	}
	if bbox[0] >= bbox[2] || bbox[1] >= bbox[3] {
		http.Error(w, "bbox must satisfy minLng < maxLng and minLat < maxLat", http.StatusBadRequest)
		return
	}

	// ── query ─────────────────────────────────────────────────────────────────
	tiles, err := rh.store.ListTilesByYear(r.Context(), year, bbox)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		fmt.Printf("[catalog] ListTilesByYear: %v\n", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(tiles); err != nil {
		fmt.Printf("[catalog] encode response: %v\n", err)
	}
}
