package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/qwezert/geogoservice/internal/cache"
)

// ServeGetMe handles GET /api/me.
// Returns the API key record for the authenticated caller.
func (rh *RenderHandler) ServeGetMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	key := APIKeyFromContext(r.Context())
	if key == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(key); err != nil {
		fmt.Printf("[me] encode response: %v\n", err)
	}
}

// ServeUpdateSettings handles PUT /api/me/settings.
// Body must be a valid JSON object; it replaces the caller's settings entirely.
func (rh *RenderHandler) ServeUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	key := APIKeyFromContext(r.Context())
	if key == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if !json.Valid(body) {
		writeJSONError(w, http.StatusBadRequest, "body must be valid JSON")
		return
	}

	if err := rh.store.UpdateAPIKeySettings(r.Context(), key.Token, body); err != nil {
		if errors.Is(err, cache.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "API key not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "failed to update settings")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"settings":%s}`, body)
}
