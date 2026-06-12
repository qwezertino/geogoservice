package handler

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/qwezert/geogoservice/internal/cache"
)

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ServeCreateAPIKey handles POST /api/admin/keys.
// Body (optional): {"label": "my-client"}
// Returns 201 with the full APIKey JSON including the token.
func (rh *RenderHandler) ServeCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var body struct {
		Label string `json:"label"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}

	token, err := generateToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	key, err := rh.store.CreateAPIKey(r.Context(), token, body.Label)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to create API key")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(key); err != nil {
		fmt.Printf("[admin] encode create response: %v\n", err)
	}
}

// ServeListAPIKeys handles GET /api/admin/keys.
// Returns a JSON array of all API keys (active and inactive).
func (rh *RenderHandler) ServeListAPIKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	keys, err := rh.store.ListAPIKeys(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to list API keys")
		return
	}

	if keys == nil {
		keys = []cache.APIKey{}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(keys); err != nil {
		fmt.Printf("[admin] encode list response: %v\n", err)
	}
}

// ServeDeleteAPIKey handles DELETE /api/admin/keys/{token}.
// Deactivates the key; does not hard-delete the row.
func (rh *RenderHandler) ServeDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	token := r.PathValue("token")
	if token == "" {
		writeJSONError(w, http.StatusBadRequest, "token is required")
		return
	}

	if err := rh.store.DeactivateAPIKey(r.Context(), token); err != nil {
		if errors.Is(err, cache.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "API key not found or already inactive")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "failed to deactivate API key")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"deactivated":%q}`, token)
}
