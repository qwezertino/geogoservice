package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ServeDelete handles DELETE /api/tiles?key=<minio_key>
//
// Removes the tile identified by its MinIO object key from all three stores:
// MinIO, PostgreSQL, and Redis (if enabled).
//
// The minio_key is returned by GET /api/catalog in the "minio_key" field.
//
// Example:
//
//	DELETE /api/tiles?key=ndvi/2026-04-01/3430440.234946_5873412.602688_3430578.179895_5873519.801814_512x398.png
//
// Response 200: {"deleted": "<key>"}
// Response 400: bad request (missing or suspicious key)
// Response 500: storage error
func (rh *RenderHandler) ServeDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing required parameter: key", http.StatusBadRequest)
		return
	}
	// Basic sanity check — prevent path traversal.
	if strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}

	if err := rh.store.DeleteTile(r.Context(), key); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		fmt.Printf("[delete] %v\n", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"deleted": key})
}
