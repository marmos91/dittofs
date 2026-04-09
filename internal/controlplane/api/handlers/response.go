package handlers

import (
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
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// healthyResponse creates a successful health check response.
func healthyResponse(data any) Response {
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

// degradedResponse creates a health check response indicating degraded operation.
// Used when the server is functional but some remote stores are unreachable.
// Returns status "degraded" (not "unhealthy") because edge deployments are
// expected to operate offline -- K8s probes should NOT restart the pod.
func degradedResponse(data any) Response {
	return Response{
		Status:    "degraded",
		Timestamp: time.Now().UTC(),
		Data:      data,
	}
}
