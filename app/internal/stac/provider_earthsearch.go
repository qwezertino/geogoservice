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

// esS2GDALOpts: Sentinel-2 COGs are plain HTTPS — no special S3 config needed.
var esS2GDALOpts = []string{
	"GDAL_HTTP_MAX_RETRY=3",
	"GDAL_HTTP_RETRY_DELAY=1",
}

// esLandsatGDALOpts: EarthSearch serves Landsat assets as s3://usgs-landsat/...
// which GDAL accesses via /vsis3/. The bucket is public (requester-pays disabled
// for anonymous access) so AWS_NO_SIGN_REQUEST bypasses credential lookup.
var esLandsatGDALOpts = []string{
	"AWS_NO_SIGN_REQUEST=YES",
	"GDAL_HTTP_MAX_RETRY=3",
	"GDAL_HTTP_RETRY_DELAY=1",
}

// esCollections is the ordered list of STAC collections queried by EarthSearch.
// Landsat is excluded: usgs-landsat S3 bucket is Requester Pays and requires
// AWS credentials. Re-enable by adding LandsatCollection once credentials are configured.
var esCollections = []string{Sentinel2Collection}

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
		SortBy:      []stacSortBy{{Field: "properties.eo:cloud_cover", Direction: "asc"}},
		Limit:       20,
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
// Handles both Sentinel-2 (nir key, HTTPS COGs) and Landsat (nir08 key,
// s3://usgs-landsat/... converted to /vsis3/ with unsigned access).
func esExtractBands(f stacRawFeature) (*BandURLs, error) {
	isLandsat := f.Collection == LandsatCollection

	href := func(a *stacAsset) string {
		if a == nil {
			return ""
		}
		if isLandsat {
			return s3ToVSIS3Landsat(a.Href)
		}
		return a.Href
	}

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

	opts := esS2GDALOpts
	if isLandsat {
		opts = esLandsatGDALOpts
	}

	bu := &BandURLs{
		RedURL:         href(red),
		NIRURL:         href(nir),
		GDALConfigOpts: opts,
		ProviderName:   f.Collection,
	}
	if b := f.Assets["blue"]; b != nil {
		bu.BlueURL = href(b)
	}
	if g := f.Assets["green"]; g != nil {
		bu.GreenURL = href(g)
	}
	if s := f.Assets["swir16"]; s != nil {
		bu.SWIRURL = href(s)
	}
	// SCL for Sentinel-2 cloud masking (Landsat uses qa_pixel — not supported yet).
	if scl := f.Assets["scl"]; scl != nil {
		bu.SCLURL = href(scl)
	}
	return bu, nil
}

// s3ToVSIS3Landsat converts an s3:// URI to a GDAL /vsis3/ path.
// EarthSearch returns Landsat COG assets as s3://usgs-landsat/... which
// GDAL must access via its /vsis3/ virtual filesystem.
func s3ToVSIS3Landsat(href string) string {
	const prefix = "s3://"
	if len(href) > len(prefix) && href[:len(prefix)] == prefix {
		return "/vsis3/" + href[len(prefix):]
	}
	return href
}
