package handlers

import (
	"encoding/json"
	"net/http"
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
