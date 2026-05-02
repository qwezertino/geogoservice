// Package render implements NDVI computation and color-mapped PNG generation.
package render

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

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
