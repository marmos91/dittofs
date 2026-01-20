// Package middleware provides HTTP middleware for the DittoFS API.
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/marmos91/dittofs/pkg/api/auth"
)

// Context key type for storing claims
type contextKey string

const claimsContextKey contextKey = "claims"

// GetClaimsFromContext retrieves JWT claims from the request context.
// Returns nil if no claims are present.
//
// This function should only be called within API handler code that runs
// after the JWTAuth middleware has processed the request. If called before
// authentication, or in routes without JWTAuth middleware, it will return nil.
func GetClaimsFromContext(ctx context.Context) *auth.Claims {
	claims, ok := ctx.Value(claimsContextKey).(*auth.Claims)
	if !ok {
		return nil
	}
	return claims
}

// extractBearerToken extracts the token from a Bearer Authorization header.
// Returns the token string and true if successful, or empty string and false if not.
func extractBearerToken(r *http.Request) (string, bool) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return "", false
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}

	return parts[1], true
}

// JWTAuth is a middleware that validates Bearer tokens in the Authorization header.
// If valid, the claims are stored in the request context.
// If invalid or missing, returns 401 Unauthorized.
func JWTAuth(jwtService *auth.JWTService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenString, ok := extractBearerToken(r)
			if !ok {
				http.Error(w, "Authorization header required", http.StatusUnauthorized)
				return
			}

			claims, err := jwtService.ValidateAccessToken(tokenString)
			if err != nil {
				http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), claimsContextKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAdmin is a middleware that blocks non-admin users.
// Must be used after JWTAuth middleware.
func RequireAdmin() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := GetClaimsFromContext(r.Context())
			if claims == nil {
				http.Error(w, "Authentication required", http.StatusUnauthorized)
				return
			}

			if !claims.IsAdmin() {
				http.Error(w, "Admin access required", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequirePasswordChange is a middleware that blocks users who must change their password.
// Allows access to specified paths even when password change is required.
// Must be used after JWTAuth middleware.
//
// Note: Path matching uses exact string comparison against r.URL.Path.
// Paths should not include trailing slashes unless the route explicitly has them.
// If the router is mounted at a sub-path, provide the full path including the prefix.
func RequirePasswordChange(allowedPaths ...string) func(http.Handler) http.Handler {
	allowedSet := make(map[string]bool)
	for _, path := range allowedPaths {
		// Normalize by removing trailing slash (except for root "/")
		normalized := strings.TrimSuffix(path, "/")
		if normalized == "" {
			normalized = "/"
		}
		allowedSet[normalized] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := GetClaimsFromContext(r.Context())
			if claims == nil {
				http.Error(w, "Authentication required", http.StatusUnauthorized)
				return
			}

			// Normalize request path by removing trailing slash
			requestPath := strings.TrimSuffix(r.URL.Path, "/")
			if requestPath == "" {
				requestPath = "/"
			}

			// Check if this path is allowed regardless of password change requirement
			if allowedSet[requestPath] {
				next.ServeHTTP(w, r)
				return
			}

			// Block if user must change password
			if claims.MustChangePassword {
				http.Error(w, "Password change required", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// OptionalJWTAuth is like JWTAuth but doesn't require authentication.
// If a valid token is present, claims are stored in context.
// If no token or invalid token, request continues without claims.
func OptionalJWTAuth(jwtService *auth.JWTService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenString, ok := extractBearerToken(r)
			if !ok {
				// No valid token - continue without claims
				next.ServeHTTP(w, r)
				return
			}

			claims, err := jwtService.ValidateAccessToken(tokenString)
			if err != nil {
				// Invalid token - continue without claims
				next.ServeHTTP(w, r)
				return
			}

			ctx := context.WithValue(r.Context(), claimsContextKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
