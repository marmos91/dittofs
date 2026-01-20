package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/marmos91/dittofs/pkg/identity"
)

// decodeJSONBody decodes a JSON request body into the provided pointer.
// Returns true if successful, false if decoding fails (error response is written automatically).
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		BadRequest(w, "Invalid request body")
		return false
	}
	return true
}

// getUserOrError fetches a user by username and handles common errors.
// Returns the user and true if successful.
// Returns nil and false if user not found (writes 404) or on error (writes 500).
func getUserOrError(w http.ResponseWriter, store identity.IdentityStore, username string) (*identity.User, bool) {
	user, err := store.GetUser(username)
	if err != nil {
		if errors.Is(err, identity.ErrUserNotFound) {
			NotFound(w, "User not found")
			return nil, false
		}
		InternalServerError(w, "Failed to get user")
		return nil, false
	}
	return user, true
}

// getUserOrUnauthorized fetches a user by username, returning 401 if not found.
// Used for auth-related endpoints where user absence means invalid auth.
// Returns the user and true if successful.
// Returns nil and false if user not found (writes 401) or on error (writes 500).
func getUserOrUnauthorized(w http.ResponseWriter, store identity.IdentityStore, username string) (*identity.User, bool) {
	user, err := store.GetUser(username)
	if err != nil {
		if errors.Is(err, identity.ErrUserNotFound) {
			Unauthorized(w, "User no longer exists")
			return nil, false
		}
		InternalServerError(w, "Failed to get user")
		return nil, false
	}
	return user, true
}
