package handler

import (
	"encoding/json"

	"github.com/qwezert/geogoservice/internal/cache"
	"github.com/qwezert/geogoservice/internal/render"
)

// paletteForIndex returns the palette stops and cache hash for the given index
// from the API key's settings. Falls back to render.DefaultPalette(index) with
// hash "" when no custom palette is configured (preserving existing cache keys).
func paletteForIndex(key *cache.APIKey, index string) ([]render.PaletteStop, string) {
	if key != nil && len(key.Settings) > 0 {
		var settings struct {
			Palettes map[string][]render.PaletteStop `json:"palettes"`
		}
		if json.Unmarshal(key.Settings, &settings) == nil {
			if stops := settings.Palettes[index]; len(stops) > 0 {
				return stops, render.PaletteHash(stops)
			}
		}
	}
	return render.DefaultPalette(index), ""
}
