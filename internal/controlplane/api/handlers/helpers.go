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
//
// This centralizes the error-to-HTTP-status translation that was previously
// duplicated across handlers. It uses errors.Is() to match sentinel errors
// from the models package.
//
// Returns:
//   - (status int, message string) suitable for writing an HTTP error response
//
// Mapping:
//   - ErrUserNotFound, ErrGroupNotFound, ErrShareNotFound, ErrStoreNotFound,
//     ErrAdapterNotFound, ErrSettingNotFound, ErrNetgroupNotFound -> 404
//   - ErrDuplicateUser, ErrDuplicateGroup, ErrDuplicateShare,
//     ErrDuplicateStore, ErrDuplicateAdapter, ErrDuplicateNetgroup -> 409
//   - ErrStoreInUse, ErrNetgroupInUse -> 409
//   - ErrUserDisabled, ErrGuestDisabled -> 403
//   - Default -> 500 "Internal server error"
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
//
// This is a convenience function that combines MapStoreError with WriteProblem.
// Handlers can replace their per-error switch blocks with a single call:
//
//	if err := h.store.DeleteUser(ctx, username); err != nil {
//	    HandleStoreError(w, err)
//	    return
//	}
func HandleStoreError(w http.ResponseWriter, err error) {
	status, msg := MapStoreError(err)
	WriteProblem(w, status, http.StatusText(status), msg)
}
