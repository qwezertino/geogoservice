package render

import (
	"context"
	"fmt"

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
	MaxCloudCover    float64
	// Polygon is an optional WGS-84 clipping polygon ([longitude, latitude] pairs).
	// Pixels outside the polygon are made transparent in the rendered PNG.
	Polygon []geo.LngLat
}

// RenderTile runs the full satellite → NDVI → PNG pipeline and returns PNG bytes.
// It does not interact with the cache — callers are responsible for that.
func RenderTile(ctx context.Context, p TileParams, stacClient *stac.Client) ([]byte, error) {
	// Transform request bbox (EPSG:3857) to WGS84 for STAC + GDAL.
	bbox4326, err := geo.Transform3857To4326(p.BBox)
	if err != nil {
		return nil, fmt.Errorf("transform bbox: %w", err)
	}

	// Find the best available satellite scene.
	bands, err := stacClient.FindBestScene(ctx, bbox4326, p.Date, p.SearchWindowDays, p.MaxCloudCover)
	if err != nil {
		return nil, fmt.Errorf("find scene: %w", err)
	}

	// Read Red and NIR bands via GDAL /vsicurl/ (HTTP range requests).
	redBuf, err := geo.ReadBandWindow(bands.RedURL, bbox4326, p.W, p.H)
	if err != nil {
		return nil, fmt.Errorf("read Red band: %w", err)
	}

	nirBuf, err := geo.ReadBandWindow(bands.NIRURL, bbox4326, p.W, p.H)
	if err != nil {
		return nil, fmt.Errorf("read NIR band: %w", err)
	}

	// Compute NDVI and encode as colour PNG.
	ndvi, err := ComputeNDVI(redBuf, nirBuf)
	if err != nil {
		return nil, fmt.Errorf("compute NDVI: %w", err)
	}

	var pixelPoly [][2]float64
	if len(p.Polygon) >= 3 {
		pixelPoly = geo.PolygonToPixels(p.Polygon, p.BBox, p.W, p.H)
	}

	pngBytes, err := RenderPNG(ndvi, p.W, p.H, pixelPoly)
	if err != nil {
		return nil, fmt.Errorf("encode PNG: %w", err)
	}

	return pngBytes, nil
}
