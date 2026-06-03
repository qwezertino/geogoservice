package handler

import (
	"fmt"
	"net/http"
	"strings"
)

// ServeImage handles GET /api/images?key=<minio_key>.
//
// Streams the PNG stored in MinIO for the given key.
// Used as a proxy by the PHP backend so the frontend can display
// job-rendered tiles without direct MinIO access.
//
// Cache-Control is set to immutable — the key already encodes all render
// parameters, so the content never changes.
func (rh *RenderHandler) ServeImage(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing required query parameter: key", http.StatusBadRequest)
		return
	}
	if strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	data, err := rh.store.GetObject(ctx, key)
	if err != nil {
		if isNotFoundError(err) {
			http.Error(w, "image not found", http.StatusNotFound)
			return
		}
		fmt.Printf("[images] get key=%q: %v\n", key, err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
