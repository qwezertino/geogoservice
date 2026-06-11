// Package geo – GDAL read helpers for COG tiles via /vsicurl/.
package geo

import (
	"fmt"
	"math"
	"strings"

	"github.com/airbusgeo/godal"
)

// ReadBandWindow uses GDAL's virtual filesystems to read pixels from a
// Cloud Optimized GeoTIFF or JP2 file, fetching only the bytes that overlap
// the requested bounding box (EPSG:4326).
//
// Parameters:
//   - cogURL    : URL or GDAL virtual-filesystem path of the file. Plain HTTPS
//     URLs are automatically wrapped with /vsicurl/. Paths already starting
//     with /vsis (e.g. /vsis3/bucket/key for CDSE) are used as-is.
//   - configOpts: Optional "KEY=VALUE" GDAL config strings applied as
//     thread-local options for this Open call. Safe for concurrent use.
//   - bbox      : Desired area in EPSG:4326 degrees.
//   - outW, outH: Desired output raster dimensions.
//
// Returns a float32 slice of length outW*outH (row-major), or an error.
func ReadBandWindow(cogURL string, configOpts []string, bbox BBox, outW, outH int) ([]float32, error) {
	// Prepend /vsicurl/ only for plain HTTP(S) URLs; GDAL /vsis* paths are
	// already rooted at a virtual filesystem.
	vsicurlPath := cogURL
	if strings.HasPrefix(cogURL, "http://") || strings.HasPrefix(cogURL, "https://") {
		vsicurlPath = "/vsicurl/" + cogURL
	}

	var openOpts []godal.OpenOption
	for _, opt := range configOpts {
		// ConfigOption uses CPLSetThreadLocalConfigOption internally — safe for
		// concurrent goroutines.
		openOpts = append(openOpts, godal.ConfigOption(opt))
	}

	ds, err := godal.Open(vsicurlPath, openOpts...)
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

// AOICloudFraction reads the Sentinel-2 Scene Classification Layer (SCL) band
// at 64×64 resolution over bbox and returns the fraction (0–1) of pixels
// classified as no data, cloud, or shadow (SCL 0 = no data, 3 = cloud shadow,
// 8 = medium probability, 9 = high probability, 10 = thin cirrus).
//
// Returns 0, nil when sclURL is empty so callers can treat a missing SCL as
// "cloud-free" rather than blocking the render.
func AOICloudFraction(sclURL string, configOpts []string, bbox BBox) (float64, error) {
	if sclURL == "" {
		return 0, nil
	}
	// 64×64 is more than enough to characterise cloud cover over a farm AOI;
	// at Sentinel-2 20 m SCL resolution this still covers km-scale areas.
	const sclReadSize = 64
	pixels, err := ReadBandWindow(sclURL, configOpts, bbox, sclReadSize, sclReadSize)
	if err != nil {
		return 0, fmt.Errorf("read SCL: %w", err)
	}
	if len(pixels) == 0 {
		return 0, nil
	}
	var cloud int
	for _, v := range pixels {
		// Round float32 SCL value to nearest integer class.
		cls := int(v + 0.5)
		if cls == 0 || cls == 3 || cls == 8 || cls == 9 || cls == 10 {
			cloud++
		}
	}
	return float64(cloud) / float64(len(pixels)), nil
}

// ReadSCLCloudMask reads the Sentinel-2 SCL band at w×h resolution over bbox
// and returns a per-pixel boolean mask where true = no data, cloud, or shadow
// (SCL class 0=no data, 3=cloud shadow, 8=medium cloud, 9=high cloud, 10=thin cirrus).
func ReadSCLCloudMask(sclURL string, configOpts []string, bbox BBox, w, h int) ([]bool, error) {
	pixels, err := ReadBandWindow(sclURL, configOpts, bbox, w, h)
	if err != nil {
		return nil, fmt.Errorf("read SCL: %w", err)
	}
	mask := make([]bool, len(pixels))
	for i, v := range pixels {
		cls := int(v + 0.5)
		mask[i] = cls == 0 || cls == 3 || cls == 8 || cls == 9 || cls == 10
	}
	return mask, nil
}
