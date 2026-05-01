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

	// pcTokenURL issues a collection-level SAS token valid for ~1 hour.
	// One token covers all assets in the collection — no per-URL signing needed.
	pcTokenURL = "https://planetarycomputer.microsoft.com/api/sas/v1/token/" + Sentinel2Collection

	// tokenExpiryBuffer ensures we refresh the token before it actually expires.
	tokenExpiryBuffer = 5 * time.Minute
)

// planetaryComputerProvider implements Provider for Microsoft Planetary Computer.
// It caches the collection-level SAS token so that concurrent requests share
// a single token without stampeding the token endpoint.
type planetaryComputerProvider struct {
	hc *http.Client

	tokenMu      sync.Mutex
	cachedToken  string
	tokenExpires time.Time
}

func newPlanetaryComputerProvider(hc *http.Client) *planetaryComputerProvider {
	return &planetaryComputerProvider{hc: hc}
}

func (p *planetaryComputerProvider) Name() string { return ProviderPlanetaryComputer }

func (p *planetaryComputerProvider) FindBestScene(ctx context.Context, bbox geo.BBox, date string, windowDays int, maxCloud float64) (*BandURLs, error) {
	datetime, err := buildDatetimeInterval(date, windowDays)
	if err != nil {
		return nil, err
	}

	features, err := doSearch(ctx, p.hc, pcBaseURL, stacSearchRequest{
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
		return nil, fmt.Errorf("planetary computer search: %w", err)
	}

	best := features[0]
	b04 := best.Assets["B04"]
	b08 := best.Assets["B08"]
	if b04 == nil || b08 == nil {
		return nil, fmt.Errorf("planetary computer: scene missing B04 or B08 assets")
	}

	token, err := p.getToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("planetary computer: fetch SAS token: %w", err)
	}

	return &BandURLs{
		RedURL: applyToken(b04.Href, token),
		NIRURL: applyToken(b08.Href, token),
	}, nil
}

// getToken returns a valid SAS token, refreshing from PC when expired.
func (p *planetaryComputerProvider) getToken(ctx context.Context) (string, error) {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()

	if p.cachedToken != "" && time.Now().Before(p.tokenExpires) {
		return p.cachedToken, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pcTokenURL, nil)
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

	p.cachedToken = tokenResp.Token
	p.tokenExpires = expiry.Add(-tokenExpiryBuffer)
	return p.cachedToken, nil
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
