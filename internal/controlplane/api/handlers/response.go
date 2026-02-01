package handlers

import (
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

// writeJSON writes a JSON response with the given status code.
// Deprecated: Use WriteJSON from problem.go instead.
// This function is kept for backward compatibility with health endpoints.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	WriteJSON(w, status, data)
}

// healthyResponse creates a successful health check response.
func healthyResponse(data interface{}) Response {
	return Response{
		Status:    "healthy",
		Timestamp: time.Now().UTC(),
		Data:      data,
	}
}

// unhealthyResponse creates a failed health check response with an error message.
func unhealthyResponse(errMsg string) Response {
	return Response{
		Status:    "unhealthy",
		Timestamp: time.Now().UTC(),
		Error:     errMsg,
	}
}

// unhealthyResponseWithData creates a failed health check response with data payload.
func unhealthyResponseWithData(data interface{}) Response {
	return Response{
		Status:    "unhealthy",
		Timestamp: time.Now().UTC(),
		Data:      data,
	}
}
