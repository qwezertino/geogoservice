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

// createRangeJobRequest is the POST body for POST /api/jobs/render-range.
type createRangeJobRequest struct {
	BBox          [4]float64   `json:"bbox"`            // [minX,minY,maxX,maxY] EPSG:3857
	StartDate     string       `json:"start_date"`      // YYYY-MM-DD
	EndDate       string       `json:"end_date"`        // YYYY-MM-DD
	MaxCloudCover float64      `json:"max_cloud_cover"` // 0–100; 0 → server default
	W             int          `json:"w"`
	H             int          `json:"h"`
	Polygon       [][2]float64 `json:"polygon,omitempty"` // WGS-84 [lng, lat] clipping polygon
}

// jobStatusResponse is the GET /api/jobs/render-range/{id} response.
type jobStatusResponse struct {
	ID        string   `json:"id"`
	Status    string   `json:"status"` // pending/running/done/failed
	Done      int      `json:"done"`
	Total     int      `json:"total"`
	Errors    []string `json:"errors,omitempty"`
	CreatedAt string   `json:"created_at"`
}

// ServeCreateRangeJob handles POST /api/jobs/render-range.
//
// It validates the request, creates a job record in PostgreSQL, launches a
// background worker goroutine, and immediately returns the job ID so the
// client can poll /api/jobs/render-range/{id} for progress.
//
// Request body (JSON):
//
//	{
//	  "bbox": [minX,minY,maxX,maxY],
//	  "start_date": "2026-03-01",
//	  "end_date":   "2026-05-31",
//	  "max_cloud_cover": 20,
//	  "w": 512, "h": 512,
//	  "polygon": [[lng,lat], ...]   // optional
//	}
//
// Response 202:
//
//	{"job_id": "<uuid>"}
func (rh *RenderHandler) ServeCreateRangeJob(w http.ResponseWriter, r *http.Request) {
	var req createRangeJobRequest
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

	bbox := geo.BBox{MinX: req.BBox[0], MinY: req.BBox[1], MaxX: req.BBox[2], MaxY: req.BBox[3]}

	// Build polygon and its hash for tile deduplication.
	polygon := make([]geo.LngLat, 0, len(req.Polygon))
	for _, p := range req.Polygon {
		polygon = append(polygon, geo.LngLat{p[0], p[1]})
	}
	polygonHash := geo.PolygonHash(polygon)

	jobID := uuid.New().String()
	ctx := r.Context()

	if err := rh.store.CreateJob(ctx, jobID, bbox,
		req.StartDate, req.EndDate, req.MaxCloudCover,
		req.W, req.H, polygonHash, req.Polygon,
	); err != nil {
		http.Error(w, "failed to create job", http.StatusInternalServerError)
		log.Printf("[job] create job: %v", err)
		return
	}

	// Launch background worker — not bound to the request context so the
	// render continues even after the HTTP response is sent.
	go rh.runRangeJob(jobID, bbox, req.StartDate, req.EndDate, req.MaxCloudCover,
		req.W, req.H, polygon, polygonHash)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
}

// ServeRangeJobStatus handles GET /api/jobs/render-range/{id}.
//
// Returns the current status, progress counters, and any per-scene error
// messages accumulated so far.
func (rh *RenderHandler) ServeRangeJobStatus(w http.ResponseWriter, r *http.Request) {
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

// ServeRangeJobResults handles GET /api/jobs/render-range/{id}/results.
//
// Returns the list of tiles produced by the job — one entry per successfully
// rendered acquisition date. Each entry carries the minio_key so the client
// can call POST /api/render/batch with {"minio_key": "..."} to fetch the PNG
// without triggering a new STAC search or render.
//
// Response (JSON array, ordered by date ascending):
//
//	[{"date":"2026-03-15","minio_key":"ndvi/2026-03-15/...png"}, ...]
//
// Can be called while the job is still running to show partial results.
func (rh *RenderHandler) ServeRangeJobResults(w http.ResponseWriter, r *http.Request) {
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

	tiles, err := rh.store.ListJobTiles(ctx, params)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		log.Printf("[job] list job tiles %s: %v", id, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tiles)
}

// runRangeJob is the background worker for a range render job.
//
// Algorithm:
//  1. Find all Sentinel-2 scenes in the date range — one STAC request total.
//  2. Skip scenes already present in the tile cache (idempotent re-runs).
//  3. Render remaining scenes in parallel (up to rh.sem concurrency budget,
//     shared with live render requests so GDAL is never overloaded).
//  4. Save each PNG to MinIO + PostgreSQL + Redis synchronously before
//     incrementing done so the progress counter reflects durable writes.
func (rh *RenderHandler) runRangeJob(
	jobID string,
	bbox geo.BBox,
	startDate, endDate string,
	maxCloud float64,
	w, h int,
	polygon []geo.LngLat,
	polygonHash string,
) {
	ctx := context.Background()

	// ── 1. Find all scenes in range ──────────────────────────────────────────
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

	if err := rh.store.SetJobRunning(ctx, jobID, len(scenes)); err != nil {
		log.Printf("[job] %s: SetJobRunning: %v", jobID, err)
	}
	log.Printf("[job] %s: found %d scenes for %s/%s", jobID, len(scenes), startDate, endDate)

	// ── 2. Render scenes in parallel ─────────────────────────────────────────
	var wg sync.WaitGroup
	for _, scene := range scenes {
		scene := scene // capture

		// Skip if already cached — supports idempotent re-runs.
		if _, found, _ := rh.store.Lookup(ctx, bbox, scene.Date, "ndvi", w, h, polygonHash); found {
			log.Printf("[job] %s: skip %s (cached)", jobID, scene.Date)
			if err := rh.store.IncrJobDone(ctx, jobID); err != nil {
				log.Printf("[job] %s: IncrJobDone: %v", jobID, err)
			}
			continue
		}

		// Acquire shared render semaphore so we don't overload GDAL.
		rh.sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-rh.sem; wg.Done() }()

			// ── AOI cloud check via SCL band ─────────────────────────────────
			// The global eo:cloud_cover metadata describes the whole ~12 000 km²
			// Sentinel-2 tile, not the specific field. Read the SCL band at low
			// resolution over the AOI to get a local cloud fraction.
			skip := false
			if scene.Bands.SCLURL != "" {
				fraction, sclErr := geo.AOICloudFraction(scene.Bands.SCLURL, scene.Bands.GDALConfigOpts, bbox4326)
				if sclErr != nil {
					log.Printf("[job] %s: %s: SCL read failed (%v), rendering anyway", jobID, scene.Date, sclErr)
				} else if fraction*100 > maxCloud {
					msg := fmt.Sprintf("skip %s: AOI cloud %.0f%% exceeds threshold %.0f%%", scene.Date, fraction*100, maxCloud)
					log.Printf("[job] %s: %s", jobID, msg)
					_ = rh.store.AppendJobError(ctx, jobID, msg)
					skip = true
				}
			}

			if !skip {
				params := render.TileParams{
					BBox:    bbox,
					Date:    scene.Date,
					Index:   "ndvi",
					W:       w,
					H:       h,
					Polygon: polygon,
				}

				pngBytes, renderErr := render.RenderFromBands(ctx, scene.Bands, params)
				if renderErr != nil {
					msg := fmt.Sprintf("%s: %v", scene.Date, renderErr)
					log.Printf("[job] %s: render failed: %s", jobID, msg)
					_ = rh.store.AppendJobError(ctx, jobID, msg)
				} else {
					if saveErr := rh.store.Save(ctx, bbox, scene.Date, "ndvi", w, h, pngBytes, polygonHash); saveErr != nil {
						msg := fmt.Sprintf("%s: save: %v", scene.Date, saveErr)
						log.Printf("[job] %s: %s", jobID, msg)
						_ = rh.store.AppendJobError(ctx, jobID, msg)
					}
				}
			}

			if err := rh.store.IncrJobDone(ctx, jobID); err != nil {
				log.Printf("[job] %s: IncrJobDone: %v", jobID, err)
			}
		}()
	}

	wg.Wait()

	if err := rh.store.SetJobDone(ctx, jobID); err != nil {
		log.Printf("[job] %s: SetJobDone: %v", jobID, err)
	}
	log.Printf("[job] %s: done", jobID)
}
