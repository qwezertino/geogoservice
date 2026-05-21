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

// RenderTile runs the full satellite → NDVI → PNG pipeline and returns PNG bytes.
// It does not interact with the cache — callers are responsible for that.
func RenderTile(ctx context.Context, p TileParams, stacClient *stac.Client) ([]byte, error) {
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

	// readBands reads Red and NIR concurrently and returns both buffers.
	// Returns (red, nir, error).
	readBands := func(b *stac.BandURLs) ([]float32, []float32, time.Duration, time.Duration, error) {
		type bandResult struct {
			buf []float32
			err error
			dur time.Duration
		}
		redCh := make(chan bandResult, 1)
		nirCh := make(chan bandResult, 1)
		go func() {
			t := time.Now()
			buf, err := geo.ReadBandWindow(b.RedURL, b.GDALConfigOpts, bbox4326, p.W, p.H)
			redCh <- bandResult{buf, err, time.Since(t).Round(time.Millisecond)}
		}()
		go func() {
			t := time.Now()
			buf, err := geo.ReadBandWindow(b.NIRURL, b.GDALConfigOpts, bbox4326, p.W, p.H)
			nirCh <- bandResult{buf, err, time.Since(t).Round(time.Millisecond)}
		}()
		red := <-redCh
		nir := <-nirCh
		if red.err != nil {
			return nil, nil, red.dur, nir.dur, fmt.Errorf("read Red band: %w", red.err)
		}
		if nir.err != nil {
			return nil, nil, red.dur, nir.dur, fmt.Errorf("read NIR band: %w", nir.err)
		}
		return red.buf, nir.buf, red.dur, nir.dur, nil
	}

	// Read Red and NIR bands concurrently. On failure, retry once using a
	// different provider (skipping the one that just gave us broken URLs).
	t2 := time.Now()
	redBuf, nirBuf, redDur, nirDur, readErr := readBands(bands)
	if readErr != nil {
		log.Printf("[pipeline] band read failed (%v provider=%q), trying fallback: %v", time.Since(t2).Round(time.Millisecond), bands.ProviderName, readErr)
		fallback, fbErr := stacClient.FindBestSceneFallback(ctx, bbox4326, p.Date, p.SearchWindowDays, p.MaxCloudCover, bands.ProviderName)
		if fbErr != nil {
			return nil, fmt.Errorf("read bands failed and fallback unavailable: %w (original: %v)", fbErr, readErr)
		}
		t2 = time.Now()
		redBuf, nirBuf, redDur, nirDur, readErr = readBands(fallback)
		if readErr != nil {
			return nil, fmt.Errorf("read bands failed on fallback provider %q: %w", fallback.ProviderName, readErr)
		}
		bands = fallback
	}
	log.Printf("[pipeline] read Red:     %v  NIR: %v  (wall: %v, provider: %s)", redDur, nirDur, time.Since(t2).Round(time.Millisecond), bands.ProviderName)

	// Compute NDVI and encode as colour PNG.
	t3 := time.Now()
	ndvi, err := ComputeNDVI(redBuf, nirBuf)
	if err != nil {
		return nil, fmt.Errorf("compute NDVI: %w", err)
	}

	var pixelPoly [][2]float64
	if len(p.Polygon) >= 3 {
		pixelPoly = geo.PolygonToPixels(p.Polygon, p.BBox, p.W, p.H)
	}

	pngBytes, err := RenderPNG(ndvi, p.W, p.H, pixelPoly)
	if err != nil {
		return nil, fmt.Errorf("encode PNG: %w", err)
	}
	log.Printf("[pipeline] ndvi+png:    %v", time.Since(t3).Round(time.Millisecond))

	log.Printf("[pipeline] TOTAL:       %v  (%dx%d)", time.Since(t0).Round(time.Millisecond), p.W, p.H)

	return pngBytes, nil
}

// RenderResult holds both the colour-mapped PNG and the raw NDVI float32 buffer
// produced by RenderFromBands. The raw buffer is stored as a companion object in
// MinIO so that clients can do pixel-level NDVI lookups without re-rendering.
type RenderResult struct {
	PNG     []byte    // colour-mapped PNG (40–60 KB)
	NDVIRaw []float32 // row-major float32 NDVI values, length = W×H
}

// RenderFromBands renders Red+NIR → NDVI → PNG from pre-fetched band URLs.
// Use this when FindAllScenes has already resolved the scene — it skips the
// STAC search step and goes straight to band reading.
func RenderFromBands(ctx context.Context, bands *stac.BandURLs, p TileParams) (*RenderResult, error) {
	t0 := time.Now()

	bbox4326, err := geo.Transform3857To4326(p.BBox)
	if err != nil {
		return nil, fmt.Errorf("transform bbox: %w", err)
	}

	type bandResult struct {
		buf []float32
		err error
		dur time.Duration
	}
	redCh := make(chan bandResult, 1)
	nirCh := make(chan bandResult, 1)

	go func() {
		t := time.Now()
		buf, err := geo.ReadBandWindow(bands.RedURL, bands.GDALConfigOpts, bbox4326, p.W, p.H)
		redCh <- bandResult{buf, err, time.Since(t).Round(time.Millisecond)}
	}()
	go func() {
		t := time.Now()
		buf, err := geo.ReadBandWindow(bands.NIRURL, bands.GDALConfigOpts, bbox4326, p.W, p.H)
		nirCh <- bandResult{buf, err, time.Since(t).Round(time.Millisecond)}
	}()

	red := <-redCh
	nir := <-nirCh
	if red.err != nil {
		return nil, fmt.Errorf("read Red band: %w", red.err)
	}
	if nir.err != nil {
		return nil, fmt.Errorf("read NIR band: %w", nir.err)
	}
	log.Printf("[pipeline] RenderFromBands %s: Red %v NIR %v provider=%s total=%v",
		p.Date, red.dur, nir.dur, bands.ProviderName, time.Since(t0).Round(time.Millisecond))

	ndvi, err := ComputeNDVI(red.buf, nir.buf)
	if err != nil {
		return nil, fmt.Errorf("compute NDVI: %w", err)
	}

	var pixelPoly [][2]float64
	if len(p.Polygon) >= 3 {
		pixelPoly = geo.PolygonToPixels(p.Polygon, p.BBox, p.W, p.H)
	}

	pngBytes, err := RenderPNG(ndvi, p.W, p.H, pixelPoly)
	if err != nil {
		return nil, fmt.Errorf("encode PNG: %w", err)
	}
	return &RenderResult{PNG: pngBytes, NDVIRaw: ndvi}, nil
}
