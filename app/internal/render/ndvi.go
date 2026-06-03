// Package render implements NDVI computation and color-mapped PNG generation.
package render

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"sort"
	// sort is used by RenderTCIPNG for percentile normalization.
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

// ndviColorMap maps an NDVI value in [-1, 1] to an RGBA colour.
//
// Thresholds:
//   - [-1.0, 0.05) → fully transparent (water / non-vegetation)
//   - [0.05, 0.2)  → red → yellow gradient (sparse / stressed vegetation)
//   - [0.2, 1.0]   → light green → dark green gradient (healthy vegetation)
func ndviColorMap(v float32) color.RGBA {
	switch {
	case v < 0.05:
		// Transparent
		return color.RGBA{0, 0, 0, 0}

	case v < 0.2:
		// Gradient: red (255,0,0) → yellow (255,255,0)
		t := float64(v-0.05) / float64(0.2-0.05) // 0..1
		g := uint8(math.Round(t * 255))
		return color.RGBA{255, g, 0, 255}

	default:
		// Gradient: light green (144,238,144) → dark green (0,100,0)
		t := float64(v-0.2) / float64(1.0-0.2) // 0..1
		t = math.Min(1.0, math.Max(0.0, t))
		r := uint8(math.Round(144 * (1 - t)))
		g := uint8(math.Round(238 - (238-100)*t))
		b := uint8(math.Round(144 * (1 - t)))
		return color.RGBA{r, g, b, 255}
	}
}

// RenderPNG converts an NDVI float32 buffer (row-major, width×height) into a
// colour-mapped PNG byte slice using the project colour map.
// If maskPoly contains at least 3 pixel-space points, pixels outside the
// polygon are made fully transparent before encoding.
func RenderPNG(ndvi []float32, width, height int, maskPoly [][2]float64) ([]byte, error) {
	if len(ndvi) != width*height {
		return nil, fmt.Errorf("ndvi buffer length %d != width*height (%d×%d=%d)",
			len(ndvi), width, height, width*height)
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			idx := y*width + x
			img.SetRGBA(x, y, ndviColorMap(ndvi[idx]))
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

// ─── Color maps for other vegetation / moisture indexes ─────────────────────

// eviColorMap maps EVI ∈ [-1, 1] to an RGBA colour.
// Similar to NDVI but EVI saturates less in dense canopies; same threshold
// scheme applies.
func eviColorMap(v float32) color.RGBA {
	switch {
	case v < 0.05:
		return color.RGBA{0, 0, 0, 0}
	case v < 0.2:
		t := float64(v-0.05) / 0.15
		g := uint8(math.Round(t * 255))
		return color.RGBA{255, g, 0, 255}
	default:
		t := math.Min(1.0, float64(v-0.2)/0.8)
		r := uint8(math.Round(144 * (1 - t)))
		g := uint8(math.Round(238 - (138)*t))
		b := uint8(math.Round(144 * (1 - t)))
		return color.RGBA{r, g, b, 255}
	}
}

// gndviColorMap maps GNDVI ∈ [-1, 1] to RGBA.
// GNDVI is more sensitive to chlorophyll content than NDVI; we use a tighter
// low-vegetation threshold (0.15) because healthy crops have higher GNDVI.
func gndviColorMap(v float32) color.RGBA {
	switch {
	case v < 0.15:
		return color.RGBA{0, 0, 0, 0}
	case v < 0.3:
		t := float64(v-0.15) / 0.15
		g := uint8(math.Round(t * 255))
		return color.RGBA{255, g, 0, 255}
	default:
		t := math.Min(1.0, float64(v-0.3)/0.7)
		r := uint8(math.Round(144 * (1 - t)))
		g := uint8(math.Round(238 - 138*t))
		b := uint8(math.Round(144 * (1 - t)))
		return color.RGBA{r, g, b, 255}
	}
}

// cviColorMap maps CVI (Chlorophyll Vegetation Index) ∈ [0, ~10] to RGBA.
// CVI = NIR × Red / Green² — we normalise to [0, 10] and treat 0–1 as bare.
func cviColorMap(v float32) color.RGBA {
	if v < 1.0 {
		return color.RGBA{0, 0, 0, 0}
	}
	// Stretch 1–10 to 0–1.
	t := float64(v-1.0) / 9.0
	t = math.Min(1.0, math.Max(0.0, t))
	r := uint8(math.Round(144 * (1 - t)))
	g := uint8(math.Round(100 + 138*(1-t)))
	b := uint8(math.Round(144 * (1 - t)))
	return color.RGBA{r, g, b, 255}
}

// moistureColorMap maps soilMoisture (SWIR-based, ∈ [-1, 1]) to RGBA.
// Negative / zero → transparent. Moist → blue gradient.
func moistureColorMap(v float32) color.RGBA {
	if v <= 0.0 {
		return color.RGBA{0, 0, 0, 0}
	}
	t := math.Min(1.0, float64(v))
	// dry (light yellow) → moist (deep blue)
	r := uint8(math.Round(230 * (1 - t)))
	g := uint8(math.Round(220 * (1 - t)))
	b := uint8(math.Round(80 + 175*t))
	return color.RGBA{r, g, b, 255}
}

// colorMapFor returns the color-mapping function for a given index name.
func colorMapFor(index string) func(float32) color.RGBA {
	switch index {
	case "evi":
		return eviColorMap
	case "gndvi":
		return gndviColorMap
	case "cvi":
		return cviColorMap
	case "soilmoisture":
		return moistureColorMap
	default: // "ndvi" and any unknown
		return ndviColorMap
	}
}

// RenderIndexPNG is the generalised version of RenderPNG that accepts an index
// name to select the correct colour map. For index == "ndvi" it is identical
// to RenderPNG.
func RenderIndexPNG(values []float32, index string, width, height int, maskPoly [][2]float64) ([]byte, error) {
	if len(values) != width*height {
		return nil, fmt.Errorf("values buffer length %d != %d×%d=%d",
			len(values), width, height, width*height)
	}
	colorMap := colorMapFor(index)
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
	norm := func(band []float32) []float32 {
		cp := make([]float32, len(band))
		copy(cp, band)
		sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
		lo := cp[int(float64(len(cp))*0.02)]
		hi := cp[int(float64(len(cp))*0.98)]
		if hi == lo {
			hi = lo + 1
		}
		out := make([]float32, len(band))
		for i, v := range band {
			t := (v - lo) / (hi - lo)
			if t < 0 {
				t = 0
			} else if t > 1 {
				t = 1
			}
			out[i] = t
		}
		return out
	}
	r := norm(red)
	g := norm(green)
	b := norm(blue)

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
