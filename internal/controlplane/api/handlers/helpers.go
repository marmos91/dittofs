package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
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

// MapStoreError maps a control plane store error to an HTTP status code and message.
func MapStoreError(err error) (int, string) {
	// Not found errors -> 404
	switch {
	case errors.Is(err, models.ErrUserNotFound):
		return http.StatusNotFound, "User not found"
	case errors.Is(err, models.ErrGroupNotFound):
		return http.StatusNotFound, "Group not found"
	case errors.Is(err, models.ErrShareNotFound):
		return http.StatusNotFound, "Share not found"
	case errors.Is(err, models.ErrStoreNotFound):
		return http.StatusNotFound, "Store not found"
	case errors.Is(err, models.ErrAdapterNotFound):
		return http.StatusNotFound, "Adapter not found"
	case errors.Is(err, models.ErrSettingNotFound):
		return http.StatusNotFound, "Setting not found"
	case errors.Is(err, models.ErrNetgroupNotFound):
		return http.StatusNotFound, "Netgroup not found"

	// Duplicate/conflict errors -> 409
	case errors.Is(err, models.ErrDuplicateUser):
		return http.StatusConflict, "User already exists"
	case errors.Is(err, models.ErrDuplicateGroup):
		return http.StatusConflict, "Group already exists"
	case errors.Is(err, models.ErrDuplicateShare):
		return http.StatusConflict, "Share already exists"
	case errors.Is(err, models.ErrDuplicateStore):
		return http.StatusConflict, "Store already exists"
	case errors.Is(err, models.ErrDuplicateAdapter):
		return http.StatusConflict, "Adapter already exists"
	case errors.Is(err, models.ErrDuplicateNetgroup):
		return http.StatusConflict, "Netgroup already exists"
	case errors.Is(err, models.ErrStoreInUse):
		return http.StatusConflict, "Store is referenced by shares"
	case errors.Is(err, models.ErrNetgroupInUse):
		return http.StatusConflict, "Netgroup is referenced by shares"

	// Forbidden errors -> 403
	case errors.Is(err, models.ErrUserDisabled):
		return http.StatusForbidden, "User account is disabled"
	case errors.Is(err, models.ErrGuestDisabled):
		return http.StatusForbidden, "Guest access is disabled"

	default:
		return http.StatusInternalServerError, "Internal server error"
	}
}

// HandleStoreError maps a store error to an HTTP response and writes it.
func HandleStoreError(w http.ResponseWriter, err error) {
	status, msg := MapStoreError(err)
	WriteProblem(w, status, http.StatusText(status), msg)
}
