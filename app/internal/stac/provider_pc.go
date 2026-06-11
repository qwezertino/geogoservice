package stac

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/qwezert/geogoservice/internal/geo"
)

const (
	pcBaseURL = "https://planetarycomputer.microsoft.com/api/stac/v1"

	pcTokenBaseURL = "https://planetarycomputer.microsoft.com/api/sas/v1/token/"

	// tokenExpiryBuffer ensures we refresh the token before it actually expires.
	tokenExpiryBuffer = 5 * time.Minute
)

// pcCollections is the ordered list of STAC collections queried by PC.
// Landsat is temporarily disabled pending AWS credential configuration.
// Re-enable by adding LandsatCollection to this slice.
var pcCollections = []string{Sentinel2Collection}

// planetaryComputerProvider implements Provider for Microsoft Planetary Computer.
// Caches per-collection SAS tokens so concurrent requests share a single token
// without stampeding the token endpoint.
type planetaryComputerProvider struct {
	hc *http.Client

	tokenMu     sync.Mutex
	tokenCache  map[string]string    // collection → SAS token
	tokenExpiry map[string]time.Time // collection → expiry
}

func newPlanetaryComputerProvider(hc *http.Client) *planetaryComputerProvider {
	return &planetaryComputerProvider{
		hc:          hc,
		tokenCache:  make(map[string]string),
		tokenExpiry: make(map[string]time.Time),
	}
}

func (p *planetaryComputerProvider) Name() string { return ProviderPlanetaryComputer }

func (p *planetaryComputerProvider) FindBestScene(ctx context.Context, bbox geo.BBox, date string, windowDays int) (*BandURLs, error) {
	datetime, err := buildDatetimeInterval(date, windowDays)
	if err != nil {
		return nil, err
	}

	features, err := doSearch(ctx, p.hc, pcBaseURL, stacSearchRequest{
		Collections: pcCollections,
		Datetime:    datetime,
		BBox:        [4]float64{bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY},
		SortBy:      []stacSortBy{{Field: "properties.eo:cloud_cover", Direction: "asc"}},
		Limit:       20,
	})
	if err != nil {
		return nil, fmt.Errorf("planetary computer search: %w", err)
	}

	// Prefer S2 over Landsat.
	best := features[0]
	for _, f := range features {
		if f.Collection == Sentinel2Collection {
			best = f
			break
		}
	}

	return p.pcExtractBands(ctx, best)
}

// FindScenesInRange returns one SceneInfo per unique acquisition date in
// [startDate, endDate], combining Sentinel-2 and Landsat scenes.
func (p *planetaryComputerProvider) FindScenesInRange(ctx context.Context, bbox geo.BBox, startDate, endDate string) ([]SceneInfo, error) {
	return findScenesInRangeHelper(ctx, p.hc, pcBaseURL, bbox, startDate, endDate, pcCollections,
		func(ctx context.Context, f stacRawFeature) (*BandURLs, error) {
			return p.pcExtractBands(ctx, f)
		})
}

// pcExtractBands builds BandURLs from a PC feature, handling both S2 and Landsat.
func (p *planetaryComputerProvider) pcExtractBands(ctx context.Context, f stacRawFeature) (*BandURLs, error) {
	collection := f.Collection
	if collection == "" {
		collection = Sentinel2Collection // legacy fallback
	}

	token, err := p.getToken(ctx, collection)
	if err != nil {
		return nil, fmt.Errorf("fetch SAS token for %s: %w", collection, err)
	}

	var bu *BandURLs
	if collection == Sentinel2Collection {
		b04 := f.Assets["B04"]
		b08 := f.Assets["B08"]
		if b04 == nil || b08 == nil {
			return nil, fmt.Errorf("PC S2: missing B04 or B08 assets")
		}
		bu = &BandURLs{
			RedURL: applyToken(b04.Href, token),
			NIRURL: applyToken(b08.Href, token),
		}
		if scl := f.Assets["SCL"]; scl != nil {
			bu.SCLURL = applyToken(scl.Href, token)
		}
		if b := f.Assets["B02"]; b != nil {
			bu.BlueURL = applyToken(b.Href, token)
		}
		if g := f.Assets["B03"]; g != nil {
			bu.GreenURL = applyToken(g.Href, token)
		}
		if s := f.Assets["B11"]; s != nil {
			bu.SWIRURL = applyToken(s.Href, token)
		}
	} else {
		// Landsat — same lowercase asset keys as EarthSearch.
		red := f.Assets["red"]
		nir := f.Assets["nir08"]
		if red == nil || nir == nil {
			return nil, fmt.Errorf("PC Landsat: missing red or nir08 assets")
		}
		bu = &BandURLs{
			RedURL: applyToken(red.Href, token),
			NIRURL: applyToken(nir.Href, token),
		}
		if b := f.Assets["blue"]; b != nil {
			bu.BlueURL = applyToken(b.Href, token)
		}
		if g := f.Assets["green"]; g != nil {
			bu.GreenURL = applyToken(g.Href, token)
		}
		if s := f.Assets["swir16"]; s != nil {
			bu.SWIRURL = applyToken(s.Href, token)
		}
	}
	bu.ProviderName = collection
	return bu, nil
}

// getToken returns a valid SAS token for the given collection, refreshing when expired.
func (p *planetaryComputerProvider) getToken(ctx context.Context, collection string) (string, error) {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()

	if tok := p.tokenCache[collection]; tok != "" && time.Now().Before(p.tokenExpiry[collection]) {
		return tok, nil
	}

	tokenURL := pcTokenBaseURL + collection
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}

	resp, err := p.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("token HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("PC token API returned HTTP %d", resp.StatusCode)
	}

	var tokenResp struct {
		Token  string `json:"token"`
		Expiry string `json:"msft:expiry"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}

	expiry, err := time.Parse(time.RFC3339, tokenResp.Expiry)
	if err != nil {
		expiry = time.Now().Add(30 * time.Minute)
	}

	p.tokenCache[collection] = tokenResp.Token
	p.tokenExpiry[collection] = expiry.Add(-tokenExpiryBuffer)
	return p.tokenCache[collection], nil
}

// applyToken appends a SAS token query string to an Azure Blob URL.
func applyToken(href, token string) string {
	if token == "" {
		return href
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return href + "?" + token
	}
	if parsed.RawQuery != "" {
		parsed.RawQuery = parsed.RawQuery + "&" + token
	} else {
		parsed.RawQuery = token
	}
	return parsed.String()
}
