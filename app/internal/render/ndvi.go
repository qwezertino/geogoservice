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
func RenderPNG(ndvi []float32, width, height int) ([]byte, error) {
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

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode PNG: %w", err)
	}
	return buf.Bytes(), nil
}
