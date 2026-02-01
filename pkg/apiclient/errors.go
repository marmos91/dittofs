package apiclient

import (
	"fmt"
)

// APIError represents an error response from the API.
type APIError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// Error implements the error interface.
func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return e.Message
}

// IsAuthError returns true if this is an authentication error.
func (e *APIError) IsAuthError() bool {
	return e.Code == "UNAUTHORIZED" || e.Code == "FORBIDDEN"
}

// IsNotFound returns true if this is a not found error.
func (e *APIError) IsNotFound() bool {
	return e.Code == "NOT_FOUND"
}

// IsConflict returns true if this is a conflict error.
func (e *APIError) IsConflict() bool {
	return e.Code == "CONFLICT"
}

// IsValidationError returns true if this is a validation error.
func (e *APIError) IsValidationError() bool {
	return e.Code == "VALIDATION_ERROR"
}
