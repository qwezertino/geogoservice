// Package stac provides a minimal client for querying a STAC API
// (Microsoft Planetary Computer / AWS Earth Search).
package stac

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/qwezert/geogoservice/internal/geo"
)

const (
	// PlanetaryComputerSTAC is the base URL for the Microsoft Planetary Computer STAC API.
	PlanetaryComputerSTAC = "https://planetarycomputer.microsoft.com/api/stac/v1"

	// Sentinel2Collection is the Sentinel-2 Level-2A collection identifier.
	Sentinel2Collection = "sentinel-2-l2a"

	maxCloudCover = 20.0

	// pcSignURL is the Planetary Computer SAS token signing endpoint.
	// All Azure Blob Storage URLs returned by the PC STAC API must be signed
	// before they can be accessed.
	pcSignURL = "https://planetarycomputer.microsoft.com/api/sas/v1/sign"

	// searchWindowDays is the ±radius around the requested date used when searching
	// for scenes. Sentinel-2 revisit period is ~5 days, so 15 days guarantees at
	// least 3 overpasses to choose from.
	searchWindowDays = 15
)

// BandURLs holds the HTTPS/S3 URLs for the Red (B04) and NIR (B08) bands.
type BandURLs struct {
	RedURL string
	NIRURL string
}

// Client is a minimal STAC API client.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient creates a new STAC Client targeting baseURL (defaults to Planetary Computer).
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = PlanetaryComputerSTAC
	}
	return &Client{
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{MaxIdleConnsPerHost: 10},
		},
		baseURL: baseURL,
	}
}

// stacSearchRequest is the JSON body sent to the STAC /search endpoint.
type stacSearchRequest struct {
	Collections []string               `json:"collections"`
	Datetime    string                 `json:"datetime"`
	BBox        [4]float64             `json:"bbox"`
	Query       map[string]interface{} `json:"query"`
	SortBy      []stacSortBy           `json:"sortby"`
	Limit       int                    `json:"limit"`
}

type stacSortBy struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
}

// stacSearchResponse is a partial deserialisation of the GeoJSON FeatureCollection
// returned by the STAC /search endpoint.
type stacSearchResponse struct {
	Features []stacFeature `json:"features"`
}

type stacFeature struct {
	Assets stacAssets `json:"assets"`
}

type stacAssets struct {
	B04 *stacAsset `json:"B04"`
	B08 *stacAsset `json:"B08"`
}

type stacAsset struct {
	Href string `json:"href"`
}

// FindBestScene queries the STAC API for the least-cloudy Sentinel-2 scene that
// intersects bbox (EPSG:4326) closest to date (YYYY-MM-DD). It searches a
// ±searchWindowDays window around the requested date and returns the COG URLs
// for the Red and NIR bands of the best matching scene.
func (c *Client) FindBestScene(ctx context.Context, bbox geo.BBox, date string) (*BandURLs, error) {
	// Parse the requested date and build a ±searchWindowDays interval.
	requestedDate, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return nil, fmt.Errorf("invalid date %q: %w", date, err)
	}
	from := requestedDate.AddDate(0, 0, -searchWindowDays)
	to := requestedDate.AddDate(0, 0, searchWindowDays)
	datetime := fmt.Sprintf("%sT00:00:00Z/%sT23:59:59Z",
		from.Format(time.DateOnly), to.Format(time.DateOnly))

	body := stacSearchRequest{
		Collections: []string{Sentinel2Collection},
		Datetime:    datetime,
		BBox:        [4]float64{bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY},
		Query: map[string]interface{}{
			"eo:cloud_cover": map[string]interface{}{
				"lt": maxCloudCover,
			},
		},
		SortBy: []stacSortBy{
			{Field: "properties.eo:cloud_cover", Direction: "asc"},
		},
		Limit: 20,
	}

	reqBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal STAC request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/search", io.NopCloser(bytesReader(reqBytes)))
	if err != nil {
		return nil, fmt.Errorf("build STAC HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(reqBytes))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("STAC HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("STAC API returned HTTP %d", resp.StatusCode)
	}

	var result stacSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode STAC response: %w", err)
	}

	if len(result.Features) == 0 {
		return nil, fmt.Errorf("no Sentinel-2 scenes found for bbox=%v date=%s ±%dd (cloud<%.0f%%)",
			bbox, date, searchWindowDays, maxCloudCover)
	}

	// Pick the first feature (STAC returns them ordered by cloud cover ascending
	// when using the eo:cloud_cover query filter on Planetary Computer).
	best := result.Features[0]
	if best.Assets.B04 == nil || best.Assets.B08 == nil {
		return nil, fmt.Errorf("selected STAC scene is missing B04 or B08 asset URLs")
	}

	// Planetary Computer requires SAS-signing all Azure Blob Storage URLs
	// before they can be accessed. Without a signed token the storage returns 409.
	redSigned, err := c.signHref(ctx, best.Assets.B04.Href)
	if err != nil {
		return nil, fmt.Errorf("sign Red band URL: %w", err)
	}
	nirSigned, err := c.signHref(ctx, best.Assets.B08.Href)
	if err != nil {
		return nil, fmt.Errorf("sign NIR band URL: %w", err)
	}

	return &BandURLs{
		RedURL: redSigned,
		NIRURL: nirSigned,
	}, nil
}

// signHref calls the Planetary Computer SAS signing endpoint and returns the
// signed URL that can be used directly with GDAL /vsicurl/.
func (c *Client) signHref(ctx context.Context, href string) (string, error) {
	signEndpoint := pcSignURL + "?href=" + url.QueryEscape(href)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signEndpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build sign request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("sign HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("PC sign API returned HTTP %d for %q", resp.StatusCode, href)
	}

	var signed struct {
		Href string `json:"href"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&signed); err != nil {
		return "", fmt.Errorf("decode sign response: %w", err)
	}
	if signed.Href == "" {
		return "", fmt.Errorf("PC sign API returned empty href for %q", href)
	}
	return signed.Href, nil
}

// bytesReader is a helper to turn []byte into an io.Reader without importing bytes.
func bytesReader(b []byte) io.Reader {
	return &byteSliceReader{data: b}
}

type byteSliceReader struct {
	data []byte
	pos  int
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
