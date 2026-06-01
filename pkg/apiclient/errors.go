package apiclient

import (
	"fmt"
)

// APIError represents an RFC 7807 problem+json error response from the API.
// The server emits {"type","title","status","detail"} (and optionally
// "code"/"hint"); StatusCode carries the HTTP status and is authoritative
// for classification.
type APIError struct {
	Type   string `json:"type,omitempty"`
	Title  string `json:"title,omitempty"`
	Status int    `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code,omitempty"`
	Hint   string `json:"hint,omitempty"`

	// StatusCode is the HTTP status code, authoritative for classification.
	StatusCode int `json:"-"`
}

// Error implements the error interface.
func (e *APIError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	if e.Title != "" {
		return e.Title
	}
	return fmt.Sprintf("request failed with status %d", e.StatusCode)
}

// IsAuthError returns true if this is an authentication/authorization error.
func (e *APIError) IsAuthError() bool {
	return e.StatusCode == 401 || e.StatusCode == 403
}

// IsNotFound returns true if this is a not found error.
func (e *APIError) IsNotFound() bool {
	return e.StatusCode == 404
}

// IsConflict returns true if this is a conflict error.
func (e *APIError) IsConflict() bool {
	return e.StatusCode == 409
}

// IsValidationError returns true if this is a validation error.
func (e *APIError) IsValidationError() bool {
	return e.StatusCode == 400 || e.StatusCode == 422
}
