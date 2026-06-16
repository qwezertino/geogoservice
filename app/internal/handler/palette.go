package handler

import (
	"encoding/json"
	"fmt"

	"github.com/qwezert/geogoservice/internal/cache"
	"github.com/qwezert/geogoservice/internal/render"
)

// validatePolygonPoints checks a JSON-decoded [lng, lat] polygon for the same
// bounds enforced on the single-tile endpoint's parsePolygon: longitude in
// [-180, 180] and latitude in (-90, 90). Without this, an out-of-range
// longitude silently projects outside the tile bbox, so the clip mask treats
// every pixel as "outside" and the rendered PNG comes back fully transparent
// with no error at all.
func validatePolygonPoints(poly [][2]float64) error {
	for i, pt := range poly {
		lng, lat := pt[0], pt[1]
		if lng < -180 || lng > 180 {
			return fmt.Errorf("longitude %v at pair %d out of range [-180, 180]", lng, i)
		}
		if lat <= -90 || lat >= 90 {
			return fmt.Errorf("latitude %v at pair %d out of range (-90, 90); poles are not supported", lat, i)
		}
	}
	return nil
}

// tokenPrefixFor returns the S3 path prefix for the given API key as
// "<id>-<sanitized-label>/". Returns "" for nil key (pre-auth / legacy tiles).
func tokenPrefixFor(key *cache.APIKey) string {
	if key == nil {
		return ""
	}
	return cache.TokenPrefix(key.ID, key.Label)
}

// paletteForIndex returns the palette stops and cache hash for the given index
// from the API key's settings. Returns an error when no custom palette is
// configured for the index — the caller must reject the request rather than
// silently fall back to render.DefaultPalette, which renders values below its
// built-in threshold as fully transparent and can look like a broken render.
func paletteForIndex(key *cache.APIKey, index string) ([]render.PaletteStop, string, error) {
	if key != nil && len(key.Settings) > 0 {
		var settings struct {
			Palettes map[string][]render.PaletteStop `json:"palettes"`
		}
		if json.Unmarshal(key.Settings, &settings) == nil {
			if stops := settings.Palettes[index]; len(stops) > 0 {
				return stops, render.PaletteHash(stops), nil
			}
		}
	}
	return nil, "", fmt.Errorf("no palette configured for index %q on this API key; set one via PUT /api/me/settings", index)
}
