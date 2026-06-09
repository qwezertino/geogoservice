package stac

import (
	"context"
	"fmt"
	"net/http"

	"github.com/qwezert/geogoservice/internal/geo"
)

const (
	esBaseURL = "https://earth-search.aws.element84.com/v1"
)

var esGDALOpts = []string{
	"GDAL_HTTP_MAX_RETRY=3",
	"GDAL_HTTP_RETRY_DELAY=1",
}

// esCollections is the ordered list of STAC collections queried by EarthSearch.
// Sentinel-2 is listed first so it gets priority in the dedup logic.
var esCollections = []string{Sentinel2Collection, LandsatCollection}

// earthSearchProvider implements Provider for AWS Earth Search (Element 84).
// Queries both Sentinel-2 L2A and Landsat Collection 2 Level 2 in a single
// request. Sentinel-2 is preferred when both cover the same date.
// No authentication required — COGs are on public S3.
type earthSearchProvider struct {
	hc *http.Client
}

func newEarthSearchProvider(hc *http.Client) *earthSearchProvider {
	return &earthSearchProvider{hc: hc}
}

func (p *earthSearchProvider) Name() string { return ProviderEarthSearch }

func (p *earthSearchProvider) FindBestScene(ctx context.Context, bbox geo.BBox, date string, windowDays int, maxCloud float64) (*BandURLs, error) {
	datetime, err := buildDatetimeInterval(date, windowDays)
	if err != nil {
		return nil, err
	}

	features, err := doSearch(ctx, p.hc, esBaseURL, stacSearchRequest{
		Collections: esCollections,
		Datetime:    datetime,
		BBox:        [4]float64{bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY},
		Query: map[string]interface{}{
			"eo:cloud_cover": map[string]interface{}{"lt": maxCloud},
		},
		SortBy: []stacSortBy{{Field: "properties.eo:cloud_cover", Direction: "asc"}},
		Limit:  20,
	})
	if err != nil {
		return nil, fmt.Errorf("earth search: %w", err)
	}

	// Prefer S2 over Landsat in the result list.
	best := features[0]
	for _, f := range features {
		if f.Collection == Sentinel2Collection {
			best = f
			break
		}
	}

	return esExtractBands(best)
}

// FindScenesInRange returns one SceneInfo per unique acquisition date in
// [startDate, endDate], combining Sentinel-2 and Landsat scenes.
func (p *earthSearchProvider) FindScenesInRange(ctx context.Context, bbox geo.BBox, startDate, endDate string, maxCloud float64) ([]SceneInfo, error) {
	return findScenesInRangeHelper(ctx, p.hc, esBaseURL, bbox, startDate, endDate, maxCloud, esCollections,
		func(_ context.Context, f stacRawFeature) (*BandURLs, error) {
			return esExtractBands(f)
		})
}

// esExtractBands builds BandURLs from an EarthSearch feature.
// Handles both Sentinel-2 (nir key) and Landsat (nir08 key).
func esExtractBands(f stacRawFeature) (*BandURLs, error) {
	red := f.Assets["red"]
	if red == nil {
		return nil, fmt.Errorf("missing red asset")
	}

	// Sentinel-2 uses "nir" (B08); Landsat uses "nir08" (B5).
	nir := f.Assets["nir"]
	if nir == nil {
		nir = f.Assets["nir08"]
	}
	if nir == nil {
		return nil, fmt.Errorf("missing nir/nir08 asset")
	}

	bu := &BandURLs{
		RedURL:         red.Href,
		NIRURL:         nir.Href,
		GDALConfigOpts: esGDALOpts,
		ProviderName:   f.Collection,
	}
	if b := f.Assets["blue"]; b != nil {
		bu.BlueURL = b.Href
	}
	if g := f.Assets["green"]; g != nil {
		bu.GreenURL = g.Href
	}
	if s := f.Assets["swir16"]; s != nil {
		bu.SWIRURL = s.Href
	}
	// SCL for Sentinel-2 cloud masking (Landsat uses qa_pixel — not supported yet).
	if scl := f.Assets["scl"]; scl != nil {
		bu.SCLURL = scl.Href
	}
	return bu, nil
}
