package stac

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/qwezert/geogoservice/internal/geo"
)

const (
	cdseSTACBaseURL   = "https://stac.dataspace.copernicus.eu/v1"
	cdseS3Endpoint    = "eodata.dataspace.copernicus.eu"
	cdseDefaultRegion = "default"
)

// cdseProvider implements Provider for the Copernicus Data Space Ecosystem
// (https://dataspace.copernicus.eu).
//
// Sentinel-2 L2A assets are stored as JPEG 2000 files in an S3-compatible
// object store at eodata.dataspace.copernicus.eu. GDAL accesses them via
// its built-in /vsis3/ virtual filesystem using long-lived S3 key pairs.
//
// Generate S3 credentials (free) at:
//
//	https://eodata-s3keysmanager.dataspace.copernicus.eu/
type cdseProvider struct {
	hc          *http.Client
	s3AccessKey string
	s3SecretKey string
}

func newCDSEProvider(hc *http.Client, s3AccessKey, s3SecretKey string) *cdseProvider {
	return &cdseProvider{hc: hc, s3AccessKey: s3AccessKey, s3SecretKey: s3SecretKey}
}

func (p *cdseProvider) Name() string { return ProviderCDSE }

func (p *cdseProvider) FindBestScene(
	ctx context.Context,
	bbox geo.BBox,
	date string,
	windowDays int,
	maxCloud float64,
) (*BandURLs, error) {
	datetime, err := buildDatetimeInterval(date, windowDays)
	if err != nil {
		return nil, err
	}

	// The CDSE STAC catalogue is publicly searchable — no credentials needed.
	features, err := doSearch(ctx, p.hc, cdseSTACBaseURL, stacSearchRequest{
		Collections: []string{Sentinel2Collection},
		Datetime:    datetime,
		BBox:        [4]float64{bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY},
		Query: map[string]interface{}{
			"eo:cloud_cover": map[string]interface{}{"lt": maxCloud},
		},
		SortBy: []stacSortBy{{Field: "properties.eo:cloud_cover", Direction: "asc"}},
		Limit:  20,
	})
	if err != nil {
		return nil, fmt.Errorf("cdse: %w", err)
	}

	best := features[0]
	// CDSE Sentinel-2 L2A uses resolution-suffixed asset keys at 10 m.
	b04 := best.Assets["B04_10m"]
	b08 := best.Assets["B08_10m"]
	if b04 == nil || b08 == nil {
		return nil, fmt.Errorf("cdse: scene missing B04_10m or B08_10m assets")
	}

	// GDAL /vsis3/ config options for the CDSE S3-compatible endpoint.
	// These are applied as thread-local options per-Open call — safe for
	// concurrent requests using different providers.
	opts := []string{
		"AWS_S3_ENDPOINT=" + cdseS3Endpoint,
		"AWS_ACCESS_KEY_ID=" + p.s3AccessKey,
		"AWS_SECRET_ACCESS_KEY=" + p.s3SecretKey,
		"AWS_VIRTUAL_HOSTING=FALSE",
		"AWS_DEFAULT_REGION=" + cdseDefaultRegion,
		"AWS_HTTPS=YES",
	}

	bu := &BandURLs{
		RedURL:         s3ToVSIS3(b04.Href),
		NIRURL:         s3ToVSIS3(b08.Href),
		GDALConfigOpts: opts,
	}
	if scl := best.Assets["SCL_20m"]; scl != nil {
		bu.SCLURL = s3ToVSIS3(scl.Href)
	}
	if b := best.Assets["B02_10m"]; b != nil {
		bu.BlueURL = s3ToVSIS3(b.Href)
	}
	if g := best.Assets["B03_10m"]; g != nil {
		bu.GreenURL = s3ToVSIS3(g.Href)
	}
	if s := best.Assets["B11_20m"]; s != nil {
		bu.SWIRURL = s3ToVSIS3(s.Href)
	}
	return bu, nil
}

// FindScenesInRange returns one SceneInfo per unique acquisition date in
// [startDate, endDate]. A single STAC /search is issued for the whole range.
func (p *cdseProvider) FindScenesInRange(ctx context.Context, bbox geo.BBox, startDate, endDate string, maxCloud float64) ([]SceneInfo, error) {
	opts := []string{
		"AWS_S3_ENDPOINT=" + cdseS3Endpoint,
		"AWS_ACCESS_KEY_ID=" + p.s3AccessKey,
		"AWS_SECRET_ACCESS_KEY=" + p.s3SecretKey,
		"AWS_VIRTUAL_HOSTING=FALSE",
		"AWS_DEFAULT_REGION=" + cdseDefaultRegion,
		"AWS_HTTPS=YES",
	}
	return findScenesInRangeHelper(ctx, p.hc, cdseSTACBaseURL, bbox, startDate, endDate, maxCloud,
		func(_ context.Context, f stacRawFeature) (*BandURLs, error) {
			b04 := f.Assets["B04_10m"]
			b08 := f.Assets["B08_10m"]
			if b04 == nil || b08 == nil {
				return nil, fmt.Errorf("missing B04_10m or B08_10m assets")
			}
			bu := &BandURLs{
				RedURL:         s3ToVSIS3(b04.Href),
				NIRURL:         s3ToVSIS3(b08.Href),
				GDALConfigOpts: opts,
			}
			if scl := f.Assets["SCL_20m"]; scl != nil {
				bu.SCLURL = s3ToVSIS3(scl.Href)
			}
			if b := f.Assets["B02_10m"]; b != nil {
				bu.BlueURL = s3ToVSIS3(b.Href)
			}
			if g := f.Assets["B03_10m"]; g != nil {
				bu.GreenURL = s3ToVSIS3(g.Href)
			}
			if s := f.Assets["B11_20m"]; s != nil {
				bu.SWIRURL = s3ToVSIS3(s.Href)
			}
			return bu, nil
		})
}

// s3ToVSIS3 converts a CDSE S3 URI to a GDAL /vsis3/ path.
//
//	s3://eodata/Sentinel-2/...  →  /vsis3/eodata/Sentinel-2/...
func s3ToVSIS3(s3URI string) string {
	const prefix = "s3://"
	if strings.HasPrefix(s3URI, prefix) {
		return "/vsis3/" + s3URI[len(prefix):]
	}
	return s3URI
}
