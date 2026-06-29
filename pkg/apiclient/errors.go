package apiclient

import (
	"fmt"
	"sort"
	"strings"
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

	// Errors carries per-field validation messages (field -> reason) for 422
	// responses. Without it, callers only see the generic Detail ("One or more
	// settings are outside valid range") and lose which field/value was wrong.
	Errors map[string]string `json:"errors,omitempty"`

	// StatusCode is the HTTP status code, authoritative for classification.
	StatusCode int `json:"-"`
}

// Error implements the error interface.
func (e *APIError) Error() string {
	base := e.Detail
	if base == "" {
		base = e.Title
	}
	if base == "" {
		base = fmt.Sprintf("request failed with status %d", e.StatusCode)
	}
	if len(e.Errors) > 0 {
		return base + " (" + e.fieldErrors() + ")"
	}
	return base
}

// fieldErrors renders the per-field validation messages in a stable order.
func (e *APIError) fieldErrors() string {
	keys := make([]string, 0, len(e.Errors))
	for k := range e.Errors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+e.Errors[k])
	}
	return strings.Join(parts, "; ")
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
