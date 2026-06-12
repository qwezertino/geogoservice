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
	// Polygon is an optional WGS-84 clipping polygon ([longitude, latitude] pairs).
	// Pixels outside the polygon are made transparent in the rendered PNG.
	Polygon []geo.LngLat
	// Palette overrides the default gradient colour map for the index.
	// nil means use DefaultPalette(Index).
	Palette []PaletteStop
	// FillScenes lists neighbor scenes sorted by temporal proximity (closest first).
	// Cloudy pixels identified via the SCL band are replaced with clear values
	// from these scenes before the spectral index is computed.
	FillScenes []*stac.BandURLs
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
	bands, err := stacClient.FindBestScene(ctx, bbox4326, p.Date, p.SearchWindowDays)
	if err != nil {
		return nil, fmt.Errorf("find scene: %w", err)
	}
	log.Printf("[pipeline] stac search:  %v", time.Since(t1).Round(time.Millisecond))

	t2 := time.Now()
	result, readErr := renderWithBandURLs(ctx, bands, p, bbox4326)
	if readErr != nil {
		log.Printf("[pipeline] render failed (%v provider=%q), trying fallback: %v",
			time.Since(t2).Round(time.Millisecond), bands.ProviderName, readErr)
		fallback, fbErr := stacClient.FindBestSceneFallback(ctx, bbox4326, p.Date, p.SearchWindowDays, bands.ProviderName)
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
	PNG            []byte     // colour-mapped PNG (≈ 40–60 KB)
	RawValues      []float32  // row-major float32 index values, length = W×H; nil for TCI
	Stats          *TileStats // per-tile stats; nil for TCI
	RemainingCloud float64    // fraction (0–1) of pixels still cloudy after fill; 0 if fill was not applied
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

// ─── Cloud fill ──────────────────────────────────────────────────────────────

// applyCloudFill replaces cloudy pixels in bufs with values from the nearest
// clear neighbors listed in fills (sorted by temporal proximity).
//
// For each fill candidate it reads the SCL mask at the output resolution. If
// the candidate has a clear pixel where the primary scene is cloudy, it reads
// the fill bands (same order as bufs, URLs returned by urlFn) and copies them.
// Iteration stops when all cloudy pixels have been filled or candidates run out.
// Remaining unfilled pixels stay as-is (the index computation will produce NaN
// or a low value, which the palette maps to transparent).
// applyCloudFill returns the fraction (0–1) of pixels that are still cloudy
// after fill. Returns 0 when fill is not attempted (no SCL, no candidates).
func applyCloudFill(
	sclURL string,
	opts []string,
	fills []*stac.BandURLs,
	bbox geo.BBox,
	w, h int,
	date, index string,
	bufs [][]float32,
	urlFn func(*stac.BandURLs) []string,
) float64 {
	if sclURL == "" || len(fills) == 0 || len(bufs) == 0 {
		return 0
	}

	mask, err := geo.ReadSCLCloudMask(sclURL, opts, bbox, w, h)
	if err != nil {
		log.Printf("[fill] %s/%s: read primary SCL: %v (skipping fill)", date, index, err)
		return 0
	}

	cloudy := 0
	for _, c := range mask {
		if c {
			cloudy++
		}
	}
	if cloudy == 0 {
		return 0
	}

	filled := 0
	for _, fill := range fills {
		if cloudy == 0 {
			break
		}
		if fill.SCLURL == "" {
			continue
		}

		fillMask, err := geo.ReadSCLCloudMask(fill.SCLURL, fill.GDALConfigOpts, bbox, w, h)
		if err != nil {
			continue
		}

		// Check whether this fill scene covers at least one of our cloudy pixels.
		useful := false
		for i, c := range mask {
			if c && i < len(fillMask) && !fillMask[i] {
				useful = true
				break
			}
		}
		if !useful {
			continue
		}

		// Read fill bands concurrently.
		type bandRes struct {
			buf []float32
			err error
		}
		urls := urlFn(fill)
		chs := make([]chan bandRes, len(urls))
		for k, u := range urls {
			k, u := k, u
			ch := make(chan bandRes, 1)
			chs[k] = ch
			go func() {
				buf, err := geo.ReadBandWindow(u, fill.GDALConfigOpts, bbox, w, h)
				ch <- bandRes{buf, err}
			}()
		}
		fillBufs := make([][]float32, len(urls))
		ok := true
		for k, ch := range chs {
			r := <-ch
			if r.err != nil {
				ok = false
			} else {
				fillBufs[k] = r.buf
			}
		}
		if !ok {
			continue
		}

		// Apply per-pixel fill.
		for i := range mask {
			if !mask[i] {
				continue
			}
			if i >= len(fillMask) || fillMask[i] {
				continue
			}
			for k := range bufs {
				bufs[k][i] = fillBufs[k][i]
			}
			mask[i] = false
			cloudy--
			filled++
		}
	}

	if filled > 0 {
		log.Printf("[fill] %s/%s: filled %d px, %d still cloudy", date, index, filled, cloudy)
	}
	return float64(cloudy) / float64(len(mask))
}

// ─── renderWithBandURLs ───────────────────────────────────────────────────────

// bandReadResult is returned from each concurrent band-read goroutine.
type bandReadResult struct {
	buf []float32
	err error
	dur time.Duration
}

// readBandAsync launches a goroutine to read one band window and returns a
// buffered channel carrying the result. It checks ctx before starting the
// (blocking) GDAL call so cancelled requests don't queue up GDAL work.
func readBandAsync(ctx context.Context, url string, opts []string, bbox geo.BBox, w, h int) <-chan bandReadResult {
	ch := make(chan bandReadResult, 1)
	go func() {
		if err := ctx.Err(); err != nil {
			ch <- bandReadResult{err: err}
			return
		}
		t := time.Now()
		buf, err := geo.ReadBandWindow(url, opts, bbox, w, h)
		ch <- bandReadResult{buf, err, time.Since(t).Round(time.Millisecond)}
	}()
	return ch
}

// renderWithBandURLs dispatches to the correct computation path based on
// p.Index. All band reads are kicked off concurrently; only the channels
// needed by the selected index are drained.
func renderWithBandURLs(ctx context.Context, bands *stac.BandURLs, p TileParams, bbox4326 geo.BBox) (*RenderResult, error) {
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
		redCh := readBandAsync(ctx, bands.RedURL, opts, bbox4326, w, h)
		greenCh := readBandAsync(ctx, bands.GreenURL, opts, bbox4326, w, h)
		blueCh := readBandAsync(ctx, bands.BlueURL, opts, bbox4326, w, h)
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
		rc := applyCloudFill(bands.SCLURL, opts, p.FillScenes, bbox4326, w, h, p.Date, index,
			[][]float32{red.buf, green.buf, blue.buf},
			func(b *stac.BandURLs) []string { return []string{b.RedURL, b.GreenURL, b.BlueURL} })
		pngBytes, err := RenderTCIPNG(red.buf, green.buf, blue.buf, w, h, pixelPoly)
		if err != nil {
			return nil, fmt.Errorf("render TCI PNG: %w", err)
		}
		return &RenderResult{PNG: pngBytes, RemainingCloud: rc}, nil

	case "evi":
		if bands.RedURL == "" || bands.NIRURL == "" || bands.BlueURL == "" {
			return nil, fmt.Errorf("EVI requires Red, NIR, Blue bands")
		}
		redCh := readBandAsync(ctx, bands.RedURL, opts, bbox4326, w, h)
		nirCh := readBandAsync(ctx, bands.NIRURL, opts, bbox4326, w, h)
		blueCh := readBandAsync(ctx, bands.BlueURL, opts, bbox4326, w, h)
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
		rc := applyCloudFill(bands.SCLURL, opts, p.FillScenes, bbox4326, w, h, p.Date, index,
			[][]float32{red.buf, nir.buf, blue.buf},
			func(b *stac.BandURLs) []string { return []string{b.RedURL, b.NIRURL, b.BlueURL} })
		vals, err := ComputeEVI(red.buf, nir.buf, blue.buf)
		if err != nil {
			return nil, fmt.Errorf("compute EVI: %w", err)
		}
		png, err := RenderIndexPNG(vals, index, w, h, pixelPoly, p.Palette)
		if err != nil {
			return nil, fmt.Errorf("encode EVI PNG: %w", err)
		}
		return &RenderResult{PNG: png, RawValues: vals, Stats: ComputeStats(vals), RemainingCloud: rc}, nil

	case "gndvi":
		if bands.NIRURL == "" || bands.GreenURL == "" {
			return nil, fmt.Errorf("GNDVI requires NIR and Green bands")
		}
		nirCh := readBandAsync(ctx, bands.NIRURL, opts, bbox4326, w, h)
		greenCh := readBandAsync(ctx, bands.GreenURL, opts, bbox4326, w, h)
		nir, green := <-nirCh, <-greenCh
		if nir.err != nil {
			return nil, fmt.Errorf("GNDVI read NIR: %w", nir.err)
		}
		if green.err != nil {
			return nil, fmt.Errorf("GNDVI read Green: %w", green.err)
		}
		rc := applyCloudFill(bands.SCLURL, opts, p.FillScenes, bbox4326, w, h, p.Date, index,
			[][]float32{nir.buf, green.buf},
			func(b *stac.BandURLs) []string { return []string{b.NIRURL, b.GreenURL} })
		vals, err := ComputeGNDVI(nir.buf, green.buf)
		if err != nil {
			return nil, fmt.Errorf("compute GNDVI: %w", err)
		}
		png, err := RenderIndexPNG(vals, index, w, h, pixelPoly, p.Palette)
		if err != nil {
			return nil, fmt.Errorf("encode GNDVI PNG: %w", err)
		}
		return &RenderResult{PNG: png, RawValues: vals, Stats: ComputeStats(vals), RemainingCloud: rc}, nil

	case "cvi":
		if bands.RedURL == "" || bands.NIRURL == "" || bands.GreenURL == "" {
			return nil, fmt.Errorf("CVI requires Red, NIR, Green bands")
		}
		redCh := readBandAsync(ctx, bands.RedURL, opts, bbox4326, w, h)
		nirCh := readBandAsync(ctx, bands.NIRURL, opts, bbox4326, w, h)
		greenCh := readBandAsync(ctx, bands.GreenURL, opts, bbox4326, w, h)
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
		rc := applyCloudFill(bands.SCLURL, opts, p.FillScenes, bbox4326, w, h, p.Date, index,
			[][]float32{red.buf, nir.buf, green.buf},
			func(b *stac.BandURLs) []string { return []string{b.RedURL, b.NIRURL, b.GreenURL} })
		vals, err := ComputeCVI(red.buf, nir.buf, green.buf)
		if err != nil {
			return nil, fmt.Errorf("compute CVI: %w", err)
		}
		png, err := RenderIndexPNG(vals, index, w, h, pixelPoly, p.Palette)
		if err != nil {
			return nil, fmt.Errorf("encode CVI PNG: %w", err)
		}
		return &RenderResult{PNG: png, RawValues: vals, Stats: ComputeStats(vals), RemainingCloud: rc}, nil

	case "soilmoisture":
		if bands.NIRURL == "" || bands.SWIRURL == "" {
			return nil, fmt.Errorf("soilMoisture requires NIR and SWIR bands")
		}
		nirCh := readBandAsync(ctx, bands.NIRURL, opts, bbox4326, w, h)
		swirCh := readBandAsync(ctx, bands.SWIRURL, opts, bbox4326, w, h)
		nir, swir := <-nirCh, <-swirCh
		if nir.err != nil {
			return nil, fmt.Errorf("soilMoisture read NIR: %w", nir.err)
		}
		if swir.err != nil {
			return nil, fmt.Errorf("soilMoisture read SWIR: %w", swir.err)
		}
		rc := applyCloudFill(bands.SCLURL, opts, p.FillScenes, bbox4326, w, h, p.Date, index,
			[][]float32{nir.buf, swir.buf},
			func(b *stac.BandURLs) []string { return []string{b.NIRURL, b.SWIRURL} })
		vals, err := ComputeSoilMoisture(nir.buf, swir.buf)
		if err != nil {
			return nil, fmt.Errorf("compute soilMoisture: %w", err)
		}
		png, err := RenderIndexPNG(vals, index, w, h, pixelPoly, p.Palette)
		if err != nil {
			return nil, fmt.Errorf("encode soilMoisture PNG: %w", err)
		}
		return &RenderResult{PNG: png, RawValues: vals, Stats: ComputeStats(vals), RemainingCloud: rc}, nil

	default: // "ndvi" and any unknown index name
		if bands.RedURL == "" || bands.NIRURL == "" {
			return nil, fmt.Errorf("NDVI requires Red and NIR bands")
		}
		redCh := readBandAsync(ctx, bands.RedURL, opts, bbox4326, w, h)
		nirCh := readBandAsync(ctx, bands.NIRURL, opts, bbox4326, w, h)
		red, nir := <-redCh, <-nirCh
		if red.err != nil {
			return nil, fmt.Errorf("NDVI read Red: %w", red.err)
		}
		if nir.err != nil {
			return nil, fmt.Errorf("NDVI read NIR: %w", nir.err)
		}
		rc := applyCloudFill(bands.SCLURL, opts, p.FillScenes, bbox4326, w, h, p.Date, index,
			[][]float32{red.buf, nir.buf},
			func(b *stac.BandURLs) []string { return []string{b.RedURL, b.NIRURL} })
		vals, err := ComputeNDVI(red.buf, nir.buf)
		if err != nil {
			return nil, fmt.Errorf("compute NDVI: %w", err)
		}
		png, err := RenderIndexPNG(vals, "ndvi", w, h, pixelPoly, p.Palette)
		if err != nil {
			return nil, fmt.Errorf("encode NDVI PNG: %w", err)
		}
		return &RenderResult{PNG: png, RawValues: vals, Stats: ComputeStats(vals), RemainingCloud: rc}, nil
	}
}
