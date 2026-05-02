package geo

import (
	"fmt"

	"github.com/airbusgeo/godal"
)

// BBox is an axis-aligned bounding box.
type BBox struct {
	MinX, MinY, MaxX, MaxY float64
}

// Transform3857To4326 reprojects a bounding box from EPSG:3857 (Web Mercator)
// to EPSG:4326 (WGS84 degrees). It transforms all four corners and takes the
// union to handle non-linear projections near the poles.
func Transform3857To4326(bbox BBox) (BBox, error) {
	src, err := godal.NewSpatialRefFromEPSG(3857)
	if err != nil {
		return BBox{}, fmt.Errorf("create EPSG:3857 SR: %w", err)
	}
	defer src.Close()

	dst, err := godal.NewSpatialRefFromEPSG(4326)
	if err != nil {
		return BBox{}, fmt.Errorf("create EPSG:4326 SR: %w", err)
	}
	defer dst.Close()

	t, err := godal.NewTransform(src, dst)
	if err != nil {
		return BBox{}, fmt.Errorf("create coordinate transform: %w", err)
	}
	defer t.Close()

	// Transform the four corners of the bbox.
	xs := []float64{bbox.MinX, bbox.MaxX, bbox.MinX, bbox.MaxX}
	ys := []float64{bbox.MinY, bbox.MinY, bbox.MaxY, bbox.MaxY}
	zs := []float64{0, 0, 0, 0}

	if err := t.TransformEx(xs, ys, zs, nil); err != nil {
		return BBox{}, fmt.Errorf("transform coordinates: %w", err)
	}

	out := BBox{
		MinX: minFloat(xs),
		MinY: minFloat(ys),
		MaxX: maxFloat(xs),
		MaxY: maxFloat(ys),
	}
	return out, nil
}

func minFloat(vals []float64) float64 {
	m := vals[0]
	for _, v := range vals[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func maxFloat(vals []float64) float64 {
	m := vals[0]
	for _, v := range vals[1:] {
		if v > m {
			m = v
		}
	}
	return m
}
