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
//   - cogURL  : HTTPS or S3 URL of the COG file (may include SAS query params).
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

	// -- Transform input bbox (EPSG:4326) to the COG's native CRS (e.g. UTM) --
	//
	// Sentinel-2 COGs use UTM projections (e.g. EPSG:32633), so we must convert
	// our lon/lat bbox to the same CRS before computing pixel offsets.
	wgs84, err := godal.NewSpatialRefFromEPSG(4326)
	if err != nil {
		return nil, fmt.Errorf("create WGS84 SRS: %w", err)
	}
	defer wgs84.Close()

	nativeSRS, err := godal.NewSpatialRefFromWKT(ds.Projection())
	if err != nil {
		return nil, fmt.Errorf("parse dataset SRS: %w", err)
	}
	defer nativeSRS.Close()

	ct, err := godal.NewTransform(wgs84, nativeSRS)
	if err != nil {
		return nil, fmt.Errorf("create coord transform: %w", err)
	}
	defer ct.Close()

	// Transform all 4 corners and take the union to handle rotated projections.
	xs := []float64{bbox.MinX, bbox.MaxX, bbox.MinX, bbox.MaxX}
	ys := []float64{bbox.MinY, bbox.MinY, bbox.MaxY, bbox.MaxY}
	zs := []float64{0, 0, 0, 0}
	if err := ct.TransformEx(xs, ys, zs, nil); err != nil {
		return nil, fmt.Errorf("transform bbox to native CRS: %w", err)
	}

	nativeMinX := minFloat(xs)
	nativeMaxX := maxFloat(xs)
	nativeMinY := minFloat(ys)
	nativeMaxY := maxFloat(ys)

	// Convert native-CRS bbox to pixel window using the inverse geotransform.
	// gt[0]=originX, gt[1]=pixelW, gt[3]=originY, gt[5]=pixelH (negative)
	pixMinX := int(math.Floor((nativeMinX - gt[0]) / gt[1]))
	pixMaxX := int(math.Ceil((nativeMaxX - gt[0]) / gt[1]))
	pixMinY := int(math.Floor((nativeMaxY - gt[3]) / gt[5]))
	pixMaxY := int(math.Ceil((nativeMinY - gt[3]) / gt[5]))

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
