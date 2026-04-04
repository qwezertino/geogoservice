// Package geo – GDAL read helpers for COG tiles via /vsicurl/.
package geo

import (
	"fmt"
	"math"

	"github.com/airbusgeo/godal"
)

// ReadBandWindow uses GDAL's /vsicurl/ virtual file system to perform HTTP
// byte-range requests against a Cloud Optimized GeoTIFF (COG) and reads
// only the pixels that overlap with the requested bounding box (EPSG:4326).
//
// Parameters:
//   - cogURL  : HTTPS or S3 URL of the COG file.
//   - bbox    : Desired area in EPSG:4326 degrees.
//   - outW, outH : Desired output raster dimensions.
//
// Returns a float32 slice of length outW*outH (row-major), or an error.
func ReadBandWindow(cogURL string, bbox BBox, outW, outH int) ([]float32, error) {
	vsicurlPath := "/vsicurl/" + cogURL

	ds, err := godal.Open(vsicurlPath)
	if err != nil {
		return nil, fmt.Errorf("open COG %q via vsicurl: %w", cogURL, err)
	}
	defer ds.Close()

	// Retrieve the geotransform: [originX, pixelW, rotX, originY, rotY, pixelH]
	gt, err := ds.GeoTransform()
	if err != nil {
		return nil, fmt.Errorf("get geotransform for %q: %w", cogURL, err)
	}

	rasterW := ds.Structure().SizeX
	rasterH := ds.Structure().SizeY

	// Convert geographic bbox to pixel window using the inverse geotransform.
	// gt[0]=originX, gt[1]=pixelW, gt[3]=originY, gt[5]=pixelH (negative)
	pixMinX := int(math.Floor((bbox.MinX - gt[0]) / gt[1]))
	pixMaxX := int(math.Ceil((bbox.MaxX - gt[0]) / gt[1]))
	pixMinY := int(math.Floor((bbox.MaxY - gt[3]) / gt[5]))
	pixMaxY := int(math.Ceil((bbox.MinY - gt[3]) / gt[5]))

	// Clamp to valid raster extent
	pixMinX = clampInt(pixMinX, 0, rasterW)
	pixMaxX = clampInt(pixMaxX, 0, rasterW)
	pixMinY = clampInt(pixMinY, 0, rasterH)
	pixMaxY = clampInt(pixMaxY, 0, rasterH)

	winW := pixMaxX - pixMinX
	winH := pixMaxY - pixMinY
	if winW <= 0 || winH <= 0 {
		return nil, fmt.Errorf("bbox does not intersect COG extent for %q", cogURL)
	}

	// Read band 1 with GDAL's built-in resampling to target outW×outH.
	bands := ds.Bands()
	if len(bands) == 0 {
		return nil, fmt.Errorf("COG %q has no bands", cogURL)
	}
	band := bands[0]

	buf := make([]float32, outW*outH)
	if err := band.Read(pixMinX, pixMinY, buf, outW, outH,
		godal.Window(winW, winH),
	); err != nil {
		return nil, fmt.Errorf("read band window from %q: %w", cogURL, err)
	}

	return buf, nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
