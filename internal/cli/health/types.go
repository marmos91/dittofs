// Package health provides shared types for health check responses.
package health

// Response represents the API health response structure.
type Response struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
	Data      struct {
		Service   string `json:"service"`
		StartedAt string `json:"started_at"`
		Uptime    string `json:"uptime"`
		UptimeSec int64  `json:"uptime_sec"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}
