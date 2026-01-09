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
func GetClaimsFromContext(ctx context.Context) *auth.Claims {
	claims, ok := ctx.Value(claimsContextKey).(*auth.Claims)
	if !ok {
		return nil
	}
	return claims
}

// JWTAuth is a middleware that validates Bearer tokens in the Authorization header.
// If valid, the claims are stored in the request context.
// If invalid or missing, returns 401 Unauthorized.
func JWTAuth(jwtService *auth.JWTService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract token from Authorization header
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, "Authorization header required", http.StatusUnauthorized)
				return
			}

			// Expect "Bearer <token>"
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				http.Error(w, "Invalid Authorization header format", http.StatusUnauthorized)
				return
			}

			tokenString := parts[1]

			// Validate the token
			claims, err := jwtService.ValidateAccessToken(tokenString)
			if err != nil {
				http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
				return
			}

			// Store claims in context and continue
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
func RequirePasswordChange(allowedPaths ...string) func(http.Handler) http.Handler {
	allowedSet := make(map[string]bool)
	for _, path := range allowedPaths {
		allowedSet[path] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := GetClaimsFromContext(r.Context())
			if claims == nil {
				http.Error(w, "Authentication required", http.StatusUnauthorized)
				return
			}

			// Check if this path is allowed regardless of password change requirement
			if allowedSet[r.URL.Path] {
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
			// Extract token from Authorization header
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				// No auth header - continue without claims
				next.ServeHTTP(w, r)
				return
			}

			// Try to parse Bearer token
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				// Invalid format - continue without claims
				next.ServeHTTP(w, r)
				return
			}

			tokenString := parts[1]

			// Try to validate the token
			claims, err := jwtService.ValidateAccessToken(tokenString)
			if err != nil {
				// Invalid token - continue without claims
				next.ServeHTTP(w, r)
				return
			}

			// Store claims in context and continue
			ctx := context.WithValue(r.Context(), claimsContextKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
