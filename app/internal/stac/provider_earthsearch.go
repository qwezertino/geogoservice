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

// earthSearchProvider implements Provider for AWS Earth Search (Element 84).
// Sentinel-2 L2A COGs are stored on public S3 — no token or signing required.
// Asset keys differ from Planetary Computer: "red" (B04) and "nir" (B08).
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
		return nil, fmt.Errorf("earth search: %w", err)
	}

	best := features[0]
	// Earth Search uses band-name asset keys ("red", "nir") instead of
	// Sentinel band numbers ("B04", "B08").
	red := best.Assets["red"]
	nir := best.Assets["nir"]
	if red == nil || nir == nil {
		return nil, fmt.Errorf("earth search: scene missing red or nir assets")
	}

	// Earth Search COGs are on public S3 — no signing needed.
	bu := &BandURLs{
		RedURL: red.Href,
		NIRURL: nir.Href,
	}
	if scl := best.Assets["scl"]; scl != nil {
		bu.SCLURL = scl.Href
	}
	if b := best.Assets["blue"]; b != nil {
		bu.BlueURL = b.Href
	}
	if g := best.Assets["green"]; g != nil {
		bu.GreenURL = g.Href
	}
	if s := best.Assets["swir16"]; s != nil {
		bu.SWIRURL = s.Href
	}
	return bu, nil
}

// FindScenesInRange returns one SceneInfo per unique acquisition date in
// [startDate, endDate]. A single STAC /search is issued for the whole range.
func (p *earthSearchProvider) FindScenesInRange(ctx context.Context, bbox geo.BBox, startDate, endDate string, maxCloud float64) ([]SceneInfo, error) {
	return findScenesInRangeHelper(ctx, p.hc, esBaseURL, bbox, startDate, endDate, maxCloud,
		func(_ context.Context, f stacRawFeature) (*BandURLs, error) {
			red := f.Assets["red"]
			nir := f.Assets["nir"]
			if red == nil || nir == nil {
				return nil, fmt.Errorf("missing red or nir assets")
			}
			bu := &BandURLs{RedURL: red.Href, NIRURL: nir.Href}
			if scl := f.Assets["scl"]; scl != nil {
				bu.SCLURL = scl.Href
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
			return bu, nil
		})
}
