package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/qwezert/geogoservice/internal/cache"
)

type contextKey int

const contextKeyAPIKey contextKey = 0

// APIKeyFromContext retrieves the validated APIKey injected by RequireAPIKey.
func APIKeyFromContext(ctx context.Context) *cache.APIKey {
	k, _ := ctx.Value(contextKeyAPIKey).(*cache.APIKey)
	return k
}

// RequireAPIKey validates X-API-Key or Authorization: Bearer header.
// On success injects *cache.APIKey into the request context.
func RequireAPIKey(store *cache.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractToken(r)
			if token == "" {
				writeJSONError(w, http.StatusUnauthorized, "missing API key")
				return
			}
			key, err := store.GetAPIKeyByToken(r.Context(), token)
			if err != nil {
				if errors.Is(err, cache.ErrNotFound) {
					writeJSONError(w, http.StatusUnauthorized, "invalid or inactive API key")
					return
				}
				writeJSONError(w, http.StatusInternalServerError, "auth check failed")
				return
			}
			store.TouchAPIKeyLastUsed(r.Context(), token)
			ctx := context.WithValue(r.Context(), contextKeyAPIKey, key)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAdmin validates the X-Admin-Token header against adminToken.
// Returns 503 when adminToken is empty (misconfigured deployment).
func RequireAdmin(adminToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" {
				writeJSONError(w, http.StatusServiceUnavailable, "admin endpoints not configured")
				return
			}
			if r.Header.Get("X-Admin-Token") != adminToken {
				writeJSONError(w, http.StatusForbidden, "invalid admin token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// chain applies middleware right-to-left so the first argument is outermost.
func chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

func extractToken(r *http.Request) string {
	if v := r.Header.Get("X-API-Key"); v != "" {
		return v
	}
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		return strings.TrimPrefix(v, "Bearer ")
	}
	return ""
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, msg)
}
