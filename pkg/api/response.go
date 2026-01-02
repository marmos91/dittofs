package api

import (
	"encoding/json"
	"net/http"
	"time"
)

// Response represents a standard API response wrapper.
//
// All API responses follow this structure for consistency:
//   - Status indicates the overall result ("healthy", "unhealthy", "ok", "error")
//   - Timestamp provides response time for debugging and caching
//   - Data contains the response payload (optional)
//   - Error contains error details when Status indicates failure (optional)
type Response struct {
	Status    string      `json:"status"`
	Timestamp time.Time   `json:"timestamp"`
	Data      interface{} `json:"data,omitempty"`
	Error     string      `json:"error,omitempty"`
}

// JSON writes a JSON response with the given status code.
//
// The response is written with Content-Type: application/json header.
// If encoding fails, an error response is written instead.
func JSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(data); err != nil {
		// If encoding fails, attempt to write a basic error
		// This is a last resort and may not succeed
		http.Error(w, `{"status":"error","error":"failed to encode response"}`, http.StatusInternalServerError)
	}
}

// HealthyResponse creates a successful health check response.
func HealthyResponse(data interface{}) Response {
	return Response{
		Status:    "healthy",
		Timestamp: time.Now().UTC(),
		Data:      data,
	}
}

// UnhealthyResponse creates a failed health check response.
func UnhealthyResponse(errMsg string) Response {
	return Response{
		Status:    "unhealthy",
		Timestamp: time.Now().UTC(),
		Error:     errMsg,
	}
}

// OKResponse creates a generic successful response.
func OKResponse(data interface{}) Response {
	return Response{
		Status:    "ok",
		Timestamp: time.Now().UTC(),
		Data:      data,
	}
}

// ErrorResponse creates a generic error response.
func ErrorResponse(errMsg string) Response {
	return Response{
		Status:    "error",
		Timestamp: time.Now().UTC(),
		Error:     errMsg,
	}
}
