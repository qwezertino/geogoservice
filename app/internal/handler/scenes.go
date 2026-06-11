package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/qwezert/geogoservice/internal/geo"
)

// geoJSONPolygon is a minimal GeoJSON Polygon structure used to extract
// coordinates from the `polygon` query parameter.
type geoJSONPolygon struct {
	Type        string         `json:"type"`
	Coordinates [][][2]float64 `json:"coordinates"`
}

// sceneResponse is a single scene returned by GET /api/scenes.
type sceneResponse struct {
	SceneID    string  `json:"scene_id"`
	Date       string  `json:"date"`
	CloudCover float64 `json:"cloud_cover"`
}

// ServeScenes handles GET /api/scenes.
//
// Query parameters:
//
//	polygon     — URL-encoded GeoJSON Polygon (type:"Polygon") in WGS-84.
//	date_from   — YYYY-MM-DD (inclusive). Required.
//	date_to     — YYYY-MM-DD (inclusive). Required.
//	max_cloud   — Maximum cloud cover % (0–100). Default: 20.
//
// Response 200 — JSON array of scene objects ordered by date ascending:
//
//	[{"scene_id":"S2B_MSIL2A_…","date":"2026-03-15","cloud_cover":4.2}, …]
func (rh *RenderHandler) ServeScenes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()

	// ── polygon ──────────────────────────────────────────────────────────────
	polygonParam := q.Get("polygon")
	if polygonParam == "" {
		http.Error(w, "polygon parameter is required", http.StatusBadRequest)
		return
	}
	var poly geoJSONPolygon
	if err := json.Unmarshal([]byte(polygonParam), &poly); err != nil {
		http.Error(w, "polygon must be valid GeoJSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if poly.Type != "Polygon" || len(poly.Coordinates) == 0 || len(poly.Coordinates[0]) < 3 {
		http.Error(w, "polygon must be a GeoJSON Polygon with at least 3 points", http.StatusBadRequest)
		return
	}
	// Compute WGS-84 bounding box from the outer ring.
	bbox4326, err := bboxFromGeoJSONRing(poly.Coordinates[0])
	if err != nil {
		http.Error(w, "invalid polygon coordinates: "+err.Error(), http.StatusBadRequest)
		return
	}

	// ── date range ───────────────────────────────────────────────────────────
	dateFrom := q.Get("date_from")
	dateTo := q.Get("date_to")
	if dateFrom == "" || dateTo == "" {
		http.Error(w, "date_from and date_to are required (YYYY-MM-DD)", http.StatusBadRequest)
		return
	}

	// ── STAC search ───────────────────────────────────────────────────────────
	scenes, err := rh.stacClient.FindAllScenes(r.Context(), bbox4326, dateFrom, dateTo)
	if err != nil {
		http.Error(w, "STAC search failed: "+err.Error(), http.StatusBadGateway)
		fmt.Printf("[scenes] STAC search error: %v\n", err)
		return
	}

	// ── Build response ────────────────────────────────────────────────────────
	result := make([]sceneResponse, 0, len(scenes))
	for _, s := range scenes {
		result = append(result, sceneResponse{
			SceneID:    s.SceneID,
			Date:       s.Date,
			CloudCover: s.CloudCover,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// bboxFromGeoJSONRing computes the WGS-84 bounding box from an outer ring of
// [longitude, latitude] pairs.
func bboxFromGeoJSONRing(ring [][2]float64) (geo.BBox, error) {
	if len(ring) == 0 {
		return geo.BBox{}, fmt.Errorf("empty ring")
	}
	minX, maxX := ring[0][0], ring[0][0]
	minY, maxY := ring[0][1], ring[0][1]
	for _, pt := range ring[1:] {
		if pt[0] < minX {
			minX = pt[0]
		}
		if pt[0] > maxX {
			maxX = pt[0]
		}
		if pt[1] < minY {
			minY = pt[1]
		}
		if pt[1] > maxY {
			maxY = pt[1]
		}
	}
	return geo.BBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}, nil
}
