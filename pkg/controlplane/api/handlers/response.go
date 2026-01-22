package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
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
//
// The response is written with Content-Type: application/json header.
// Encoding is done to a buffer first to ensure we can return an error
// response if encoding fails (before headers are sent).
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	// Encode to buffer first to catch encoding errors before sending headers
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(data); err != nil {
		logger.Error("Failed to encode JSON response", "error", err)
		http.Error(w, `{"status":"error","error":"failed to encode response"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
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
