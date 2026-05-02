package geo

import (
	"crypto/sha256"
	"fmt"
	"math"
	"strings"
)

// LngLat is a WGS-84 coordinate pair: [longitude, latitude] in degrees.
type LngLat [2]float64

// PolygonToPixels projects a WGS-84 polygon onto pixel coordinates within the
// EPSG:3857 tile bounding box. The returned [][2]float64 contains [x, y] pairs
// in pixel space where (0, 0) is the top-left corner of the tile image.
func PolygonToPixels(poly []LngLat, bbox3857 BBox, w, h int) [][2]float64 {
	px := make([][2]float64, len(poly))
	fw := float64(w)
	fh := float64(h)
	for i, pt := range poly {
		x3857, y3857 := wgs84To3857(pt[0], pt[1])
		px[i] = [2]float64{
			(x3857 - bbox3857.MinX) / (bbox3857.MaxX - bbox3857.MinX) * fw,
			(bbox3857.MaxY - y3857) / (bbox3857.MaxY - bbox3857.MinY) * fh,
		}
	}
	return px
}

// PolygonHash returns a stable 8-character hex string that uniquely identifies
// the polygon for use in cache keys. Returns "" for nil/empty input.
func PolygonHash(poly []LngLat) string {
	if len(poly) == 0 {
		return ""
	}
	var b strings.Builder
	for _, pt := range poly {
		fmt.Fprintf(&b, "%.6f,%.6f;", pt[0], pt[1])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", sum[:4])
}

// wgs84To3857 converts WGS-84 longitude/latitude (degrees) to EPSG:3857
// (Web Mercator, metres) using the standard spherical Mercator formula.
func wgs84To3857(lng, lat float64) (x, y float64) {
	const R = 6378137.0
	x = lng * math.Pi / 180 * R
	sinLat := math.Sin(lat * math.Pi / 180)
	y = math.Log((1+sinLat)/(1-sinLat)) / 2 * R
	return
}
