package render

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/qwezert/geogoservice/internal/geo"
	"github.com/qwezert/geogoservice/internal/stac"
)

// TileParams describes a single render request.
type TileParams struct {
	BBox             geo.BBox
	Date             string
	Index            string
	W, H             int
	SearchWindowDays int
	MaxCloudCover    float64
	// Polygon is an optional WGS-84 clipping polygon ([longitude, latitude] pairs).
	// Pixels outside the polygon are made transparent in the rendered PNG.
	Polygon []geo.LngLat
}

// RenderTile runs the full satellite → index → PNG pipeline and returns a
// RenderResult. It does not interact with the cache — callers are responsible.
func RenderTile(ctx context.Context, p TileParams, stacClient *stac.Client) (*RenderResult, error) {
	t0 := time.Now()

	// Transform request bbox (EPSG:3857) to WGS84 for STAC + GDAL.
	bbox4326, err := geo.Transform3857To4326(p.BBox)
	if err != nil {
		return nil, fmt.Errorf("transform bbox: %w", err)
	}

	// Find the best available satellite scene.
	t1 := time.Now()
	bands, err := stacClient.FindBestScene(ctx, bbox4326, p.Date, p.SearchWindowDays, p.MaxCloudCover)
	if err != nil {
		return nil, fmt.Errorf("find scene: %w", err)
	}
	log.Printf("[pipeline] stac search:  %v", time.Since(t1).Round(time.Millisecond))

	t2 := time.Now()
	result, readErr := renderWithBandURLs(ctx, bands, p, bbox4326)
	if readErr != nil {
		log.Printf("[pipeline] render failed (%v provider=%q), trying fallback: %v",
			time.Since(t2).Round(time.Millisecond), bands.ProviderName, readErr)
		fallback, fbErr := stacClient.FindBestSceneFallback(ctx, bbox4326, p.Date, p.SearchWindowDays, p.MaxCloudCover, bands.ProviderName)
		if fbErr != nil {
			return nil, fmt.Errorf("render failed and fallback unavailable: %w (original: %v)", fbErr, readErr)
		}
		t2 = time.Now()
		result, readErr = renderWithBandURLs(ctx, fallback, p, bbox4326)
		if readErr != nil {
			return nil, fmt.Errorf("render failed on fallback provider %q: %w", fallback.ProviderName, readErr)
		}
		bands = fallback
	}
	log.Printf("[pipeline] render %s:   %v  provider=%s  TOTAL=%v",
		p.Index, time.Since(t2).Round(time.Millisecond), bands.ProviderName, time.Since(t0).Round(time.Millisecond))

	return result, nil
}

// RenderResult holds the rendered PNG, the raw float32 index buffer, and
// per-tile statistics computed by ComputeStats.
//
// For TCI (true-colour) tiles, RawValues and Stats are nil because there is
// no single numeric index to summarise.
type RenderResult struct {
	PNG       []byte     // colour-mapped PNG (≈ 40–60 KB)
	RawValues []float32  // row-major float32 index values, length = W×H; nil for TCI
	Stats     *TileStats // per-tile stats; nil for TCI
}

// RenderFromBands renders the requested spectral index from pre-fetched band
// URLs. Use this when FindAllScenes has already resolved the scene — it skips
// the STAC search step and goes straight to band reading.
func RenderFromBands(ctx context.Context, bands *stac.BandURLs, p TileParams) (*RenderResult, error) {
	t0 := time.Now()

	bbox4326, err := geo.Transform3857To4326(p.BBox)
	if err != nil {
		return nil, fmt.Errorf("transform bbox: %w", err)
	}

	result, err := renderWithBandURLs(ctx, bands, p, bbox4326)
	if err != nil {
		return nil, err
	}
	log.Printf("[pipeline] RenderFromBands %s index=%s provider=%s total=%v",
		p.Date, p.Index, bands.ProviderName, time.Since(t0).Round(time.Millisecond))
	return result, nil
}

// ─── renderWithBandURLs ───────────────────────────────────────────────────────

// bandReadResult is returned from each concurrent band-read goroutine.
type bandReadResult struct {
	buf []float32
	err error
	dur time.Duration
}

// readBandAsync launches a goroutine to read one band window and returns a
// buffered channel carrying the result.
func readBandAsync(url string, opts []string, bbox geo.BBox, w, h int) <-chan bandReadResult {
	ch := make(chan bandReadResult, 1)
	go func() {
		t := time.Now()
		buf, err := geo.ReadBandWindow(url, opts, bbox, w, h)
		ch <- bandReadResult{buf, err, time.Since(t).Round(time.Millisecond)}
	}()
	return ch
}

// renderWithBandURLs dispatches to the correct computation path based on
// p.Index. All band reads are kicked off concurrently; only the channels
// needed by the selected index are drained.
func renderWithBandURLs(_ context.Context, bands *stac.BandURLs, p TileParams, bbox4326 geo.BBox) (*RenderResult, error) {
	index := p.Index
	if index == "" {
		index = "ndvi"
	}

	var pixelPoly [][2]float64
	if len(p.Polygon) >= 3 {
		pixelPoly = geo.PolygonToPixels(p.Polygon, p.BBox, p.W, p.H)
	}

	opts := bands.GDALConfigOpts
	w, h := p.W, p.H

	switch index {
	case "tci":
		// True colour: Red (B04), Green (B03), Blue (B02).
		if bands.RedURL == "" || bands.GreenURL == "" || bands.BlueURL == "" {
			return nil, fmt.Errorf("TCI requires Red, Green, Blue bands but one or more URLs are empty")
		}
		redCh := readBandAsync(bands.RedURL, opts, bbox4326, w, h)
		greenCh := readBandAsync(bands.GreenURL, opts, bbox4326, w, h)
		blueCh := readBandAsync(bands.BlueURL, opts, bbox4326, w, h)
		red, green, blue := <-redCh, <-greenCh, <-blueCh
		if red.err != nil {
			return nil, fmt.Errorf("TCI read Red: %w", red.err)
		}
		if green.err != nil {
			return nil, fmt.Errorf("TCI read Green: %w", green.err)
		}
		if blue.err != nil {
			return nil, fmt.Errorf("TCI read Blue: %w", blue.err)
		}
		pngBytes, err := RenderTCIPNG(red.buf, green.buf, blue.buf, w, h, pixelPoly)
		if err != nil {
			return nil, fmt.Errorf("render TCI PNG: %w", err)
		}
		// TCI has no scalar index — Stats and RawValues stay nil.
		return &RenderResult{PNG: pngBytes}, nil

	case "evi":
		if bands.RedURL == "" || bands.NIRURL == "" || bands.BlueURL == "" {
			return nil, fmt.Errorf("EVI requires Red, NIR, Blue bands")
		}
		redCh := readBandAsync(bands.RedURL, opts, bbox4326, w, h)
		nirCh := readBandAsync(bands.NIRURL, opts, bbox4326, w, h)
		blueCh := readBandAsync(bands.BlueURL, opts, bbox4326, w, h)
		red, nir, blue := <-redCh, <-nirCh, <-blueCh
		if red.err != nil {
			return nil, fmt.Errorf("EVI read Red: %w", red.err)
		}
		if nir.err != nil {
			return nil, fmt.Errorf("EVI read NIR: %w", nir.err)
		}
		if blue.err != nil {
			return nil, fmt.Errorf("EVI read Blue: %w", blue.err)
		}
		vals, err := ComputeEVI(red.buf, nir.buf, blue.buf)
		if err != nil {
			return nil, fmt.Errorf("compute EVI: %w", err)
		}
		png, err := RenderIndexPNG(vals, index, w, h, pixelPoly)
		if err != nil {
			return nil, fmt.Errorf("encode EVI PNG: %w", err)
		}
		return &RenderResult{PNG: png, RawValues: vals, Stats: ComputeStats(vals)}, nil

	case "gndvi":
		if bands.NIRURL == "" || bands.GreenURL == "" {
			return nil, fmt.Errorf("GNDVI requires NIR and Green bands")
		}
		nirCh := readBandAsync(bands.NIRURL, opts, bbox4326, w, h)
		greenCh := readBandAsync(bands.GreenURL, opts, bbox4326, w, h)
		nir, green := <-nirCh, <-greenCh
		if nir.err != nil {
			return nil, fmt.Errorf("GNDVI read NIR: %w", nir.err)
		}
		if green.err != nil {
			return nil, fmt.Errorf("GNDVI read Green: %w", green.err)
		}
		vals, err := ComputeGNDVI(nir.buf, green.buf)
		if err != nil {
			return nil, fmt.Errorf("compute GNDVI: %w", err)
		}
		png, err := RenderIndexPNG(vals, index, w, h, pixelPoly)
		if err != nil {
			return nil, fmt.Errorf("encode GNDVI PNG: %w", err)
		}
		return &RenderResult{PNG: png, RawValues: vals, Stats: ComputeStats(vals)}, nil

	case "cvi":
		if bands.RedURL == "" || bands.NIRURL == "" || bands.GreenURL == "" {
			return nil, fmt.Errorf("CVI requires Red, NIR, Green bands")
		}
		redCh := readBandAsync(bands.RedURL, opts, bbox4326, w, h)
		nirCh := readBandAsync(bands.NIRURL, opts, bbox4326, w, h)
		greenCh := readBandAsync(bands.GreenURL, opts, bbox4326, w, h)
		red, nir, green := <-redCh, <-nirCh, <-greenCh
		if red.err != nil {
			return nil, fmt.Errorf("CVI read Red: %w", red.err)
		}
		if nir.err != nil {
			return nil, fmt.Errorf("CVI read NIR: %w", nir.err)
		}
		if green.err != nil {
			return nil, fmt.Errorf("CVI read Green: %w", green.err)
		}
		vals, err := ComputeCVI(red.buf, nir.buf, green.buf)
		if err != nil {
			return nil, fmt.Errorf("compute CVI: %w", err)
		}
		png, err := RenderIndexPNG(vals, index, w, h, pixelPoly)
		if err != nil {
			return nil, fmt.Errorf("encode CVI PNG: %w", err)
		}
		return &RenderResult{PNG: png, RawValues: vals, Stats: ComputeStats(vals)}, nil

	case "soilmoisture":
		if bands.NIRURL == "" || bands.SWIRURL == "" {
			return nil, fmt.Errorf("soilMoisture requires NIR and SWIR bands")
		}
		nirCh := readBandAsync(bands.NIRURL, opts, bbox4326, w, h)
		swirCh := readBandAsync(bands.SWIRURL, opts, bbox4326, w, h)
		nir, swir := <-nirCh, <-swirCh
		if nir.err != nil {
			return nil, fmt.Errorf("soilMoisture read NIR: %w", nir.err)
		}
		if swir.err != nil {
			return nil, fmt.Errorf("soilMoisture read SWIR: %w", swir.err)
		}
		vals, err := ComputeSoilMoisture(nir.buf, swir.buf)
		if err != nil {
			return nil, fmt.Errorf("compute soilMoisture: %w", err)
		}
		png, err := RenderIndexPNG(vals, index, w, h, pixelPoly)
		if err != nil {
			return nil, fmt.Errorf("encode soilMoisture PNG: %w", err)
		}
		return &RenderResult{PNG: png, RawValues: vals, Stats: ComputeStats(vals)}, nil

	default: // "ndvi" and any unknown index name
		if bands.RedURL == "" || bands.NIRURL == "" {
			return nil, fmt.Errorf("NDVI requires Red and NIR bands")
		}
		redCh := readBandAsync(bands.RedURL, opts, bbox4326, w, h)
		nirCh := readBandAsync(bands.NIRURL, opts, bbox4326, w, h)
		red, nir := <-redCh, <-nirCh
		if red.err != nil {
			return nil, fmt.Errorf("NDVI read Red: %w", red.err)
		}
		if nir.err != nil {
			return nil, fmt.Errorf("NDVI read NIR: %w", nir.err)
		}
		vals, err := ComputeNDVI(red.buf, nir.buf)
		if err != nil {
			return nil, fmt.Errorf("compute NDVI: %w", err)
		}
		png, err := RenderIndexPNG(vals, "ndvi", w, h, pixelPoly)
		if err != nil {
			return nil, fmt.Errorf("encode NDVI PNG: %w", err)
		}
		return &RenderResult{PNG: png, RawValues: vals, Stats: ComputeStats(vals)}, nil
	}
}
