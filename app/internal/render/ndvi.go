// Package render implements NDVI computation and color-mapped PNG generation.
package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"sort"
)

// ─── Per-tile statistics ──────────────────────────────────────────────────────

// TileStats holds computed statistics for a single spectral index tile.
// Serialised as JSONB in tile_cache.stats and returned in job results.
type TileStats struct {
	Min       float32      `json:"min"`
	Max       float32      `json:"max"`
	Mean      float32      `json:"mean"`
	Histogram []HistBucket `json:"histogram"`
}

// HistBucket is a single bin of an index histogram.
type HistBucket struct {
	Lo    float32 `json:"lo"`
	Count int     `json:"count"`
}

const (
	histBuckets         = 20
	histLo      float32 = -1.0
	histHi      float32 = 1.0
)

// ComputeStats computes min/max/mean and a 20-bucket histogram over [-1, 1]
// from a float32 index buffer. NaN/Inf pixels are skipped.
// Returns nil if no valid pixels exist or the slice is empty.
func ComputeStats(values []float32) *TileStats {
	if len(values) == 0 {
		return nil
	}
	bucketWidth := (histHi - histLo) / float32(histBuckets)
	counts := make([]int, histBuckets)
	var sum float64
	var n int
	minV := float32(math.MaxFloat32)
	maxV := float32(-math.MaxFloat32)

	for _, v := range values {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		n++
		sum += f
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
		idx := int((v - histLo) / bucketWidth)
		if idx < 0 {
			idx = 0
		} else if idx >= histBuckets {
			idx = histBuckets - 1
		}
		counts[idx]++
	}
	if n == 0 {
		return nil
	}
	buckets := make([]HistBucket, histBuckets)
	for i := range counts {
		buckets[i] = HistBucket{
			Lo:    histLo + float32(i)*bucketWidth,
			Count: counts[i],
		}
	}
	return &TileStats{
		Min:       minV,
		Max:       maxV,
		Mean:      float32(sum / float64(n)),
		Histogram: buckets,
	}
}

// ComputeNDVI calculates the per-pixel NDVI from parallel red and NIR float32 buffers.
// Both slices must have the same length.
// Formula: NDVI = (NIR - Red) / (NIR + Red)
// Values are clamped to [-1, 1]. Pixels where (NIR + Red) == 0 are set to 0.
func ComputeNDVI(red, nir []float32) ([]float32, error) {
	if len(red) != len(nir) {
		return nil, fmt.Errorf("red (%d) and NIR (%d) buffers must have equal length", len(red), len(nir))
	}

	ndvi := make([]float32, len(red))
	for i := range red {
		r := float64(red[i])
		n := float64(nir[i])
		sum := n + r
		if sum == 0 {
			ndvi[i] = 0
			continue
		}
		v := (n - r) / sum
		// Clamp to [-1, 1]
		ndvi[i] = float32(math.Max(-1.0, math.Min(1.0, v)))
	}
	return ndvi, nil
}

// ─── Palette types ────────────────────────────────────────────────────────────

// PaletteStop maps a scalar index value to an RGBA colour.
// Stops are linearly interpolated; values outside the stop range clamp to the
// nearest endpoint. Transparent-to-opaque steps can be approximated by placing
// two stops 0.001 apart.
type PaletteStop struct {
	V    float32  `json:"v"`
	RGBA [4]uint8 `json:"rgba"`
}

// DefaultPalette returns the built-in gradient stops for the given index name.
// These reproduce the hardcoded colour maps that were previously in the source.
func DefaultPalette(index string) []PaletteStop {
	switch index {
	case "evi":
		return []PaletteStop{
			{V: 0.050, RGBA: [4]uint8{0, 0, 0, 0}},
			{V: 0.051, RGBA: [4]uint8{255, 0, 0, 255}},
			{V: 0.200, RGBA: [4]uint8{255, 255, 0, 255}},
			{V: 0.201, RGBA: [4]uint8{144, 238, 144, 255}},
			{V: 1.000, RGBA: [4]uint8{0, 100, 0, 255}},
		}
	case "gndvi":
		return []PaletteStop{
			{V: 0.150, RGBA: [4]uint8{0, 0, 0, 0}},
			{V: 0.151, RGBA: [4]uint8{255, 0, 0, 255}},
			{V: 0.300, RGBA: [4]uint8{255, 255, 0, 255}},
			{V: 0.301, RGBA: [4]uint8{144, 238, 144, 255}},
			{V: 1.000, RGBA: [4]uint8{0, 100, 0, 255}},
		}
	case "cvi":
		return []PaletteStop{
			{V: 1.0, RGBA: [4]uint8{0, 0, 0, 0}},
			{V: 1.001, RGBA: [4]uint8{144, 238, 144, 255}},
			{V: 10.0, RGBA: [4]uint8{0, 100, 0, 255}},
		}
	case "soilmoisture":
		return []PaletteStop{
			{V: 0.0, RGBA: [4]uint8{0, 0, 0, 0}},
			{V: 0.001, RGBA: [4]uint8{230, 220, 80, 255}},
			{V: 1.0, RGBA: [4]uint8{0, 55, 255, 255}},
		}
	default: // "ndvi" and unknown
		return []PaletteStop{
			{V: 0.050, RGBA: [4]uint8{0, 0, 0, 0}},
			{V: 0.051, RGBA: [4]uint8{255, 0, 0, 255}},
			{V: 0.200, RGBA: [4]uint8{255, 255, 0, 255}},
			{V: 0.201, RGBA: [4]uint8{144, 238, 144, 255}},
			{V: 1.000, RGBA: [4]uint8{0, 100, 0, 255}},
		}
	}
}

// PaletteHash returns an 8-char hex fingerprint of the stops slice.
// Returns "" for a nil/empty slice (signals "default palette, no extra cache key").
func PaletteHash(stops []PaletteStop) string {
	if len(stops) == 0 {
		return ""
	}
	b, _ := json.Marshal(stops)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:4])
}

// colorMapFromStops builds a colour-mapping function from sorted gradient stops.
// Values below the first stop or above the last stop clamp to the endpoint colour.
// Between stops all four RGBA channels are linearly interpolated.
func colorMapFromStops(stops []PaletteStop) func(float32) color.RGBA {
	sorted := make([]PaletteStop, len(stops))
	copy(sorted, stops)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].V < sorted[j].V })

	toRGBA := func(s PaletteStop) color.RGBA {
		return color.RGBA{s.RGBA[0], s.RGBA[1], s.RGBA[2], s.RGBA[3]}
	}
	lerp := func(a, b uint8, t float64) uint8 {
		return uint8(math.Round(float64(a)*(1-t) + float64(b)*t))
	}

	return func(v float32) color.RGBA {
		if v <= sorted[0].V {
			return toRGBA(sorted[0])
		}
		last := sorted[len(sorted)-1]
		if v >= last.V {
			return toRGBA(last)
		}
		for i := 1; i < len(sorted); i++ {
			if v <= sorted[i].V {
				lo, hi := sorted[i-1], sorted[i]
				t := float64(v-lo.V) / float64(hi.V-lo.V)
				return color.RGBA{
					lerp(lo.RGBA[0], hi.RGBA[0], t),
					lerp(lo.RGBA[1], hi.RGBA[1], t),
					lerp(lo.RGBA[2], hi.RGBA[2], t),
					lerp(lo.RGBA[3], hi.RGBA[3], t),
				}
			}
		}
		return toRGBA(last)
	}
}

// applyPolygonMask sets the alpha of every pixel whose centre falls outside
// poly (pixel-space coordinates) to zero (fully transparent).
func applyPolygonMask(img *image.RGBA, poly [][2]float64) {
	b := img.Bounds()
	transparent := color.RGBA{}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if !pointInPolygon(float64(x)+0.5, float64(y)+0.5, poly) {
				img.SetRGBA(x, y, transparent)
			}
		}
	}
}

// pointInPolygon reports whether (px, py) is inside the polygon using the
// ray-casting algorithm. Assumes poly has at least 3 points.
func pointInPolygon(px, py float64, poly [][2]float64) bool {
	n := len(poly)
	inside := false
	j := n - 1
	for i := 0; i < n; i++ {
		xi, yi := poly[i][0], poly[i][1]
		xj, yj := poly[j][0], poly[j][1]
		if ((yi > py) != (yj > py)) && (px < (xj-xi)*(py-yi)/(yj-yi)+xi) {
			inside = !inside
		}
		j = i
	}
	return inside
}

// ─── Spectral index computation functions ─────────────────────────────────────

// ComputeEVI computes Enhanced Vegetation Index:
//
//	EVI = 2.5 × (NIR - Red) / (NIR + 6×Red - 7.5×Blue + 1)
//
// Values are clamped to [-1, 1].
func ComputeEVI(red, nir, blue []float32) ([]float32, error) {
	if len(red) != len(nir) || len(red) != len(blue) {
		return nil, fmt.Errorf("band buffers must have equal length (red=%d nir=%d blue=%d)", len(red), len(nir), len(blue))
	}
	out := make([]float32, len(red))
	for i := range red {
		r := float64(red[i])
		n := float64(nir[i])
		b := float64(blue[i])
		denom := n + 6*r - 7.5*b + 1
		var v float64
		if denom == 0 {
			v = 0
		} else {
			v = 2.5 * (n - r) / denom
		}
		out[i] = float32(math.Max(-1, math.Min(1, v)))
	}
	return out, nil
}

// ComputeGNDVI computes Green NDVI:
//
//	GNDVI = (NIR - Green) / (NIR + Green)
//
// Values are clamped to [-1, 1].
func ComputeGNDVI(nir, green []float32) ([]float32, error) {
	if len(nir) != len(green) {
		return nil, fmt.Errorf("nir (%d) and green (%d) buffers must have equal length", len(nir), len(green))
	}
	out := make([]float32, len(nir))
	for i := range nir {
		n := float64(nir[i])
		g := float64(green[i])
		s := n + g
		if s == 0 {
			out[i] = 0
			continue
		}
		v := (n - g) / s
		out[i] = float32(math.Max(-1, math.Min(1, v)))
	}
	return out, nil
}

// ComputeCVI computes Chlorophyll Vegetation Index:
//
//	CVI = NIR × Red / Green²
//
// Range: [0, ~10]. Values above 10 are clamped to 10. Pixels where Green == 0
// are set to 0.
func ComputeCVI(red, nir, green []float32) ([]float32, error) {
	if len(red) != len(nir) || len(red) != len(green) {
		return nil, fmt.Errorf("band buffers must have equal length (red=%d nir=%d green=%d)", len(red), len(nir), len(green))
	}
	out := make([]float32, len(red))
	for i := range red {
		n := float64(nir[i])
		r := float64(red[i])
		g := float64(green[i])
		if g == 0 {
			out[i] = 0
			continue
		}
		v := n * r / (g * g)
		out[i] = float32(math.Max(0, math.Min(10, v)))
	}
	return out, nil
}

// ComputeSoilMoisture computes a moisture index (NDWI variant) from NIR and SWIR:
//
//	soilMoisture = (NIR - SWIR) / (NIR + SWIR)
//
// Values are clamped to [-1, 1].
func ComputeSoilMoisture(nir, swir []float32) ([]float32, error) {
	if len(nir) != len(swir) {
		return nil, fmt.Errorf("nir (%d) and swir (%d) buffers must have equal length", len(nir), len(swir))
	}
	out := make([]float32, len(nir))
	for i := range nir {
		n := float64(nir[i])
		s := float64(swir[i])
		sum := n + s
		if sum == 0 {
			out[i] = 0
			continue
		}
		v := (n - s) / sum
		out[i] = float32(math.Max(-1, math.Min(1, v)))
	}
	return out, nil
}

// RenderIndexPNG converts a float32 index buffer into a colour-mapped PNG.
// palette selects the gradient stops; nil uses DefaultPalette(index).
func RenderIndexPNG(values []float32, index string, width, height int, maskPoly [][2]float64, palette []PaletteStop) ([]byte, error) {
	if len(values) != width*height {
		return nil, fmt.Errorf("values buffer length %d != %d×%d=%d",
			len(values), width, height, width*height)
	}
	stops := palette
	if len(stops) == 0 {
		stops = DefaultPalette(index)
	}
	colorMap := colorMapFromStops(stops)
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, colorMap(values[y*width+x]))
		}
	}
	if len(maskPoly) >= 3 {
		applyPolygonMask(img, maskPoly)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode PNG: %w", err)
	}
	return buf.Bytes(), nil
}

// RenderTCIPNG produces a true-colour RGB PNG from red, green, blue float32
// bands using 2nd/98th percentile stretch per channel (linear normalization).
func RenderTCIPNG(red, green, blue []float32, width, height int, maskPoly [][2]float64) ([]byte, error) {
	n := width * height
	if len(red) != n || len(green) != n || len(blue) != n {
		return nil, fmt.Errorf("band slice lengths must all be %d", n)
	}
	// Shared sort buffer — reused for all three channels to avoid 3 extra allocs.
	cp := make([]float32, n)
	// Output slices packed into a single allocation.
	outBuf := make([]float32, n*3)
	r, g, b := outBuf[:n], outBuf[n:2*n], outBuf[2*n:]

	norm := func(band, out []float32) {
		copy(cp, band)
		sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
		lo := cp[int(float64(n)*0.02)]
		hi := cp[int(float64(n)*0.98)]
		if hi == lo {
			hi = lo + 1
		}
		for i, v := range band {
			t := (v - lo) / (hi - lo)
			if t < 0 {
				t = 0
			} else if t > 1 {
				t = 1
			}
			out[i] = t
		}
	}
	norm(red, r)
	norm(green, g)
	norm(blue, b)

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := y*width + x
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(math.Round(float64(r[i]) * 255)),
				G: uint8(math.Round(float64(g[i]) * 255)),
				B: uint8(math.Round(float64(b[i]) * 255)),
				A: 255,
			})
		}
	}
	if len(maskPoly) >= 3 {
		applyPolygonMask(img, maskPoly)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode TCI PNG: %w", err)
	}
	return buf.Bytes(), nil
}
