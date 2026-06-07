package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/qwezert/geogoservice/internal/cache"
	"github.com/qwezert/geogoservice/internal/geo"
	"github.com/qwezert/geogoservice/internal/render"
)

// jobStatusResponse is the GET /api/jobs/{id} response.
type jobStatusResponse struct {
	ID        string   `json:"id"`
	Status    string   `json:"status"` // pending/running/done/failed
	Done      int      `json:"done"`
	Total     int      `json:"total"`
	Errors    []string `json:"errors,omitempty"`
	CreatedAt string   `json:"created_at"`
}

// createJobRequest is the POST body for POST /api/jobs.
type createJobRequest struct {
	BBox          [4]float64   `json:"bbox"`            // [minX,minY,maxX,maxY] EPSG:3857
	StartDate     string       `json:"start_date"`      // YYYY-MM-DD
	EndDate       string       `json:"end_date"`        // YYYY-MM-DD
	MaxCloudCover float64      `json:"max_cloud_cover"` // 0–100; 0 → server default
	W             int          `json:"w"`
	H             int          `json:"h"`
	Polygon       [][2]float64 `json:"polygon,omitempty"` // WGS-84 [lng, lat] clipping polygon
	Indexes       []string     `json:"indexes,omitempty"` // default: ["ndvi"]
}

// ServeCreateJob handles POST /api/jobs.
//
// Like ServeCreateRangeJob but supports multiple spectral indexes per job.
// The server renders every (scene, index) pair and stores the results with
// per-tile statistics (min/max/mean/histogram) and cloud cover metadata.
func (rh *RenderHandler) ServeCreateJob(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.StartDate == "" || req.EndDate == "" {
		http.Error(w, "start_date and end_date are required", http.StatusBadRequest)
		return
	}
	if req.W <= 0 || req.H <= 0 {
		http.Error(w, "w and h must be positive integers", http.StatusBadRequest)
		return
	}
	if req.BBox[0] >= req.BBox[2] || req.BBox[1] >= req.BBox[3] {
		http.Error(w, "bbox must satisfy minX < maxX and minY < maxY", http.StatusBadRequest)
		return
	}
	if req.MaxCloudCover <= 0 {
		req.MaxCloudCover = rh.defaultMaxCloudCover
	}
	// Validate index names and fall back to ndvi.
	validIndexes := map[string]bool{"ndvi": true, "evi": true, "gndvi": true, "cvi": true, "tci": true, "soilmoisture": true}
	indexes := req.Indexes
	if len(indexes) == 0 {
		indexes = []string{"ndvi"}
	} else {
		for _, ix := range indexes {
			if !validIndexes[ix] {
				http.Error(w, "unsupported index: "+ix, http.StatusBadRequest)
				return
			}
		}
	}

	bbox := geo.BBox{MinX: req.BBox[0], MinY: req.BBox[1], MaxX: req.BBox[2], MaxY: req.BBox[3]}
	polygon := make([]geo.LngLat, 0, len(req.Polygon))
	for _, p := range req.Polygon {
		polygon = append(polygon, geo.LngLat{p[0], p[1]})
	}
	polygonHash := geo.PolygonHash(polygon)

	// Capture the palette at job-creation time so all tiles in the job share
	// one consistent palette even if the token settings change mid-run.
	// Each index gets its own palette; we derive a single paletteHash from the
	// first index since all tiles for a job share one set of settings.
	firstIndex := indexes[0]
	jobPalettes := make(map[string][]render.PaletteStop, len(indexes))
	var paletteHash string
	apiKey := APIKeyFromContext(r.Context())
	for _, ix := range indexes {
		stops, h := paletteForIndex(apiKey, ix)
		jobPalettes[ix] = stops
		if ix == firstIndex {
			paletteHash = h
		}
	}

	jobID := uuid.New().String()
	ctx := r.Context()

	if err := rh.store.CreateJob(ctx, jobID, bbox,
		req.StartDate, req.EndDate, req.MaxCloudCover,
		req.W, req.H, polygonHash, paletteHash, req.Polygon, indexes,
	); err != nil {
		http.Error(w, "failed to create job", http.StatusInternalServerError)
		log.Printf("[job] create job: %v", err)
		return
	}

	go rh.runJob(jobID, bbox, req.StartDate, req.EndDate, req.MaxCloudCover,
		req.W, req.H, polygon, polygonHash, paletteHash, jobPalettes, indexes)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
}

// ServeJobStatus handles GET /api/jobs/{id}.
func (rh *RenderHandler) ServeJobStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing job id", http.StatusBadRequest)
		return
	}
	job, err := rh.store.GetJob(r.Context(), id)
	if err != nil {
		if errors.Is(err, cache.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		http.Error(w, "database error", http.StatusInternalServerError)
		log.Printf("[job] get job %s: %v", id, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jobStatusResponse{
		ID:        job.ID,
		Status:    job.Status,
		Done:      job.Done,
		Total:     job.Total,
		Errors:    job.Errors,
		CreatedAt: job.CreatedAt.Format(time.RFC3339),
	})
}

// ServeJobResults handles GET /api/jobs/{id}/results.
//
// Returns all tiles produced by the job, including per-tile stats, cloud cover,
// and the bbox of each tile. Tiles from all requested indexes are included,
// sorted by date then index name.
func (rh *RenderHandler) ServeJobResults(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing job id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	params, err := rh.store.GetJobParams(ctx, id)
	if err != nil {
		if errors.Is(err, cache.ErrNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		http.Error(w, "database error", http.StatusInternalServerError)
		log.Printf("[job] get job params %s: %v", id, err)
		return
	}
	tiles, err := rh.store.ListJobTilesWithStats(ctx, params)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		log.Printf("[job] list job tiles %s: %v", id, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tiles)
}

// runJob is the background worker for a multi-index range job.
//
// For each (scene, index) pair:
//  1. Skip if a matching tile already exists in the cache.
//  2. Perform AOI cloud check via SCL band (skips overclouded scenes).
//  3. Render with render.RenderFromBands.
//  4. Save PNG + raw values + stats + cloud cover to MinIO + PostgreSQL.
func (rh *RenderHandler) runJob(
	jobID string,
	bbox geo.BBox,
	startDate, endDate string,
	maxCloud float64,
	w, h int,
	polygon []geo.LngLat,
	polygonHash string,
	paletteHash string,
	palettes map[string][]render.PaletteStop,
	indexes []string,
) {
	ctx := context.Background()

	bbox4326, err := geo.Transform3857To4326(bbox)
	if err != nil {
		_ = rh.store.SetJobFailed(ctx, jobID, fmt.Sprintf("transform bbox: %v", err))
		return
	}

	scenes, err := rh.stacClient.FindAllScenes(ctx, bbox4326, startDate, endDate, maxCloud)
	if err != nil {
		_ = rh.store.SetJobFailed(ctx, jobID, fmt.Sprintf("STAC search: %v", err))
		return
	}

	total := len(scenes) * len(indexes)
	if err := rh.store.SetJobRunning(ctx, jobID, total); err != nil {
		log.Printf("[job] %s: SetJobRunning: %v", jobID, err)
	}
	log.Printf("[job] %s: found %d scenes × %d indexes = %d tiles", jobID, len(scenes), len(indexes), total)

	var wg sync.WaitGroup
	for _, scene := range scenes {
		scene := scene

		// AOI cloud check once per scene (not per-index).
		if scene.Bands.SCLURL != "" {
			fraction, sclErr := geo.AOICloudFraction(scene.Bands.SCLURL, scene.Bands.GDALConfigOpts, bbox4326)
			if sclErr != nil {
				log.Printf("[job] %s: %s: SCL read failed (%v), rendering anyway", jobID, scene.Date, sclErr)
			} else if fraction*100 > maxCloud {
				msg := fmt.Sprintf("skip %s: AOI cloud %.0f%% exceeds threshold %.0f%%", scene.Date, fraction*100, maxCloud)
				log.Printf("[job] %s: %s", jobID, msg)
				_ = rh.store.AppendJobError(ctx, jobID, msg)
				// Count all indexes for this scene as "done" (skipped).
				for range indexes {
					_ = rh.store.IncrJobDone(ctx, jobID)
				}
				continue
			}
		}

		for _, index := range indexes {
			index := index

			// Skip if already cached.
			if _, found, _ := rh.store.Lookup(ctx, bbox, scene.Date, index, w, h, polygonHash, paletteHash); found {
				log.Printf("[job] %s: skip %s/%s (cached)", jobID, scene.Date, index)
				_ = rh.store.IncrJobDone(ctx, jobID)
				continue
			}

			rh.sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-rh.sem; wg.Done() }()

				params := render.TileParams{
					BBox:    bbox,
					Date:    scene.Date,
					Index:   index,
					W:       w,
					H:       h,
					Polygon: polygon,
					Palette: palettes[index],
				}
				result, renderErr := render.RenderFromBands(ctx, scene.Bands, params)
				if renderErr != nil {
					msg := fmt.Sprintf("%s/%s: %v", scene.Date, index, renderErr)
					log.Printf("[job] %s: render failed: %s", jobID, msg)
					_ = rh.store.AppendJobError(ctx, jobID, msg)
				} else {
					var statsJSON []byte
					if result.Stats != nil {
						statsJSON, _ = json.Marshal(result.Stats)
					}
					if saveErr := rh.store.Save(ctx, bbox, scene.Date, index, w, h, result.PNG, result.RawValues, polygonHash, paletteHash, statsJSON, scene.CloudCover); saveErr != nil {
						msg := fmt.Sprintf("%s/%s: save: %v", scene.Date, index, saveErr)
						log.Printf("[job] %s: %s", jobID, msg)
						_ = rh.store.AppendJobError(ctx, jobID, msg)
					}
				}
				_ = rh.store.IncrJobDone(ctx, jobID)
			}()
		}
	}

	wg.Wait()
	if err := rh.store.SetJobDone(ctx, jobID); err != nil {
		log.Printf("[job] %s: SetJobDone: %v", jobID, err)
	}
	log.Printf("[job] %s: done", jobID)
}
