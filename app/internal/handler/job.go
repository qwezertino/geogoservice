package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/qwezert/geogoservice/internal/cache"
	"github.com/qwezert/geogoservice/internal/geo"
	"github.com/qwezert/geogoservice/internal/render"
	"github.com/qwezert/geogoservice/internal/stac"
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

// buildFillCandidates returns BandURLs from all scenes except scenes[targetIdx],
// sorted by ascending day-distance from the target scene's date.
func buildFillCandidates(scenes []stac.SceneInfo, targetIdx int) []*stac.BandURLs {
	if len(scenes) <= 1 {
		return nil
	}
	target, err := time.Parse(time.DateOnly, scenes[targetIdx].Date)
	if err != nil {
		return nil
	}
	type entry struct {
		dist int
		b    *stac.BandURLs
	}
	entries := make([]entry, 0, len(scenes)-1)
	for i, s := range scenes {
		if i == targetIdx {
			continue
		}
		d, err := time.Parse(time.DateOnly, s.Date)
		if err != nil {
			continue
		}
		diff := int(target.Sub(d).Hours() / 24)
		if diff < 0 {
			diff = -diff
		}
		entries = append(entries, entry{diff, s.Bands})
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].dist < entries[b].dist })
	out := make([]*stac.BandURLs, len(entries))
	for i, e := range entries {
		out[i] = e.b
	}
	return out
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
	tokenPrefix := tokenPrefixFor(apiKey)
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

	log.Printf("[job] %s: created bbox=[%.0f,%.0f,%.0f,%.0f] dates=%s/%s cloud=%.0f%% size=%dx%d indexes=%v",
		jobID, bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY,
		req.StartDate, req.EndDate, req.MaxCloudCover, req.W, req.H, indexes)

	go rh.runJob(jobID, bbox, req.StartDate, req.EndDate, req.MaxCloudCover,
		req.W, req.H, polygon, polygonHash, tokenPrefix, paletteHash, jobPalettes, indexes)

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
	tokenPrefix string,
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

	// Pre-check cache before hitting external STAC providers.
	// existingTiles: tiles already stored under the current paletteHash.
	// alternateTiles: tiles stored under a different paletteHash → need recolouring.
	jobParams := &cache.JobParams{
		BBox: bbox, StartDate: startDate, EndDate: endDate,
		W: w, H: h, PolygonHash: polygonHash, PaletteHash: paletteHash,
	}
	existingTiles, _ := rh.store.ListJobTilesWithStats(ctx, jobParams)
	alternateTiles, altErr := rh.store.FindAlternatePaletteTiles(ctx,
		bbox, w, h, polygonHash, startDate, endDate, indexes, paletteHash)
	if altErr != nil {
		log.Printf("[job] %s: FindAlternatePaletteTiles: %v", jobID, altErr)
	}
	hasCache := len(existingTiles) > 0 || len(alternateTiles) > 0

	// STAC search: each provider gets providerSearchTimeout independently
	// (defined in the stac package). Failure is fatal only when there is no
	// cached data to fall back on.
	var scenes []stac.SceneInfo
	scenes, err = rh.stacClient.FindAllScenes(ctx, bbox4326, startDate, endDate)
	if err != nil {
		if !hasCache {
			_ = rh.store.SetJobFailed(ctx, jobID, fmt.Sprintf("STAC search: %v", err))
			return
		}
		log.Printf("[job] %s: STAC failed (using cache): %v", jobID, err)
		scenes = nil
	}

	total := len(scenes)*len(indexes) + len(alternateTiles)
	if err := rh.store.SetJobRunning(ctx, jobID, total); err != nil {
		log.Printf("[job] %s: SetJobRunning: %v", jobID, err)
	}
	log.Printf("[job] %s: cache=%d existing+%d repalette, STAC=%d scenes × %d indexes",
		jobID, len(existingTiles), len(alternateTiles), len(scenes), len(indexes))

	maxAOICloud := rh.maxAOICloud

	var wg sync.WaitGroup
	for i, scene := range scenes {
		scene := scene

		// Fast path: determine which indexes still need rendering before doing
		// any expensive GDAL/network work (SCL read, fill candidates).
		var needsRender []string
		for _, index := range indexes {
			if _, found, _ := rh.store.Lookup(ctx, bbox, scene.Date, index, w, h, polygonHash, paletteHash); found {
				log.Printf("[job] %s: skip %s/%s (cached)", jobID, scene.Date, index)
				_ = rh.store.IncrJobDone(ctx, jobID)
			} else {
				needsRender = append(needsRender, index)
			}
		}
		if len(needsRender) == 0 {
			continue
		}

		// AOI cloud check: scenes below maxAOICloud get pixel-level fill from
		// neighbour scenes; scenes above threshold are rendered as-is (no fill).
		// No scenes are skipped — all dates are returned regardless of cloud cover.
		// aoiFraction is used as the post-render cloud estimate for no-fill scenes.
		var aoiFraction float64
		fillCandidates := buildFillCandidates(scenes, i)
		if scene.Bands.SCLURL != "" {
			f, sclErr := geo.AOICloudFraction(scene.Bands.SCLURL, scene.Bands.GDALConfigOpts, bbox4326)
			if sclErr != nil {
				log.Printf("[job] %s: %s: SCL read failed (%v), rendering without fill", jobID, scene.Date, sclErr)
				fillCandidates = nil
			} else {
				aoiFraction = f
				if f*100 >= maxAOICloud {
					log.Printf("[job] %s: %s: AOI cloud %.0f%% >= %.0f%%, rendering without fill", jobID, scene.Date, f*100, maxAOICloud)
					fillCandidates = nil
				}
			}
		}

		// Pre-render short-circuit: fill won't run AND we know the post-render
		// cloud check will reject this scene anyway — skip the render entirely.
		if maxCloud > 0 && fillCandidates == nil && aoiFraction > 0 && aoiFraction*100 > maxCloud {
			for _, index := range needsRender {
				msg := fmt.Sprintf("skip %s/%s: post-fill cloud %.0f%% > max_cloud_cover %.0f%%", scene.Date, index, aoiFraction*100, maxCloud)
				log.Printf("[job] %s: %s", jobID, msg)
				_ = rh.store.AppendJobError(ctx, jobID, msg)
				_ = rh.store.IncrJobDone(ctx, jobID)
			}
			continue
		}

		for _, index := range needsRender {
			index := index

			rh.sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-rh.sem; wg.Done() }()

				params := render.TileParams{
					BBox:       bbox,
					Date:       scene.Date,
					Index:      index,
					W:          w,
					H:          h,
					Polygon:    polygon,
					Palette:    palettes[index],
					FillScenes: fillCandidates,
				}
				var result *render.RenderResult
				var renderErr error
				for attempt := range rh.maxRenderAttempts {
					result, renderErr = render.RenderFromBands(ctx, scene.Bands, params)
					if renderErr == nil {
						break
					}
					if attempt < rh.maxRenderAttempts-1 {
						log.Printf("[job] %s: %s/%s: attempt %d failed, retrying: %v", jobID, scene.Date, index, attempt+1, renderErr)
					}
				}
				if renderErr != nil {
					msg := fmt.Sprintf("%s/%s: %v", scene.Date, index, renderErr)
					log.Printf("[job] %s: render failed: %s", jobID, msg)
					_ = rh.store.AppendJobError(ctx, jobID, msg)
				} else {
					// Post-render cloud check against user-supplied max_cloud_cover.
					// For filled scenes use the post-fill remaining fraction;
					// for unfilled scenes use the AOI fraction measured before rendering.
					var remainingCloud float64
					if fillCandidates != nil {
						remainingCloud = result.RemainingCloud
					} else {
						remainingCloud = aoiFraction
					}
					if maxCloud > 0 && remainingCloud*100 > maxCloud {
						msg := fmt.Sprintf("skip %s/%s: post-fill cloud %.0f%% > max_cloud_cover %.0f%%", scene.Date, index, remainingCloud*100, maxCloud)
						log.Printf("[job] %s: %s", jobID, msg)
						_ = rh.store.AppendJobError(ctx, jobID, msg)
					} else {
						var statsJSON []byte
						if result.Stats != nil {
							statsJSON, _ = json.Marshal(result.Stats)
						}
						if saveErr := rh.store.Save(ctx, bbox, scene.Date, index, w, h, result.PNG, result.RawValues, polygonHash, tokenPrefix, paletteHash, statsJSON, scene.CloudCover); saveErr != nil {
							msg := fmt.Sprintf("%s/%s: save: %v", scene.Date, index, saveErr)
							log.Printf("[job] %s: %s", jobID, msg)
							_ = rh.store.AppendJobError(ctx, jobID, msg)
						}
					}
				}
				_ = rh.store.IncrJobDone(ctx, jobID)
			}()
		}
	}

	wg.Wait()

	// Recolour tiles that have raw values stored under an old paletteHash.
	var pixelPoly [][2]float64
	if len(polygon) >= 3 {
		pixelPoly = geo.PolygonToPixels(polygon, bbox, w, h)
	}
	for _, alt := range alternateTiles {
		rawBytes, err := rh.store.GetNDVIRaw(ctx, alt.OldMinioKey)
		if err != nil {
			log.Printf("[job] %s: repalette %s/%s: get raw: %v (skip)", jobID, alt.Date, alt.Index, err)
			_ = rh.store.IncrJobDone(ctx, jobID)
			continue
		}
		vals := cache.DecodeRawFloat32(rawBytes)

		pngBytes, err := render.RenderIndexPNG(vals, alt.Index, w, h, pixelPoly, palettes[alt.Index])
		if err != nil {
			log.Printf("[job] %s: repalette %s/%s: render: %v (skip)", jobID, alt.Date, alt.Index, err)
			_ = rh.store.IncrJobDone(ctx, jobID)
			continue
		}

		if saveErr := rh.store.Save(ctx, bbox, alt.Date, alt.Index, w, h,
			pngBytes, vals, polygonHash, tokenPrefix, paletteHash,
			[]byte(alt.Stats), alt.CloudCover,
		); saveErr != nil {
			log.Printf("[job] %s: repalette %s/%s: save: %v", jobID, alt.Date, alt.Index, saveErr)
		} else {
			if delErr := rh.store.DeleteTile(ctx, alt.OldMinioKey); delErr != nil {
				log.Printf("[job] %s: repalette %s/%s: delete old: %v", jobID, alt.Date, alt.Index, delErr)
			}
			log.Printf("[job] %s: repalette %s/%s: ok", jobID, alt.Date, alt.Index)
		}
		_ = rh.store.IncrJobDone(ctx, jobID)
	}

	if err := rh.store.SetJobDone(ctx, jobID); err != nil {
		log.Printf("[job] %s: SetJobDone: %v", jobID, err)
	}
	log.Printf("[job] %s: done", jobID)
}
