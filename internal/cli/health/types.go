// Package health provides shared types for health check responses.
package health

// Response represents the API health response structure.
type Response struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
	Data      struct {
		Service       string         `json:"service"`
		StartedAt     string         `json:"started_at"`
		Uptime        string         `json:"uptime"`
		UptimeSec     int64          `json:"uptime_sec"`
		StorageHealth *StorageHealth `json:"storage_health,omitempty"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// StorageHealth represents aggregate remote store health.
type StorageHealth struct {
	Status       string        `json:"status"`
	TotalPending int           `json:"total_pending"`
	Shares       []ShareHealth `json:"shares,omitempty"`
}

// ShareHealth represents a single share's remote store health.
type ShareHealth struct {
	Name                string  `json:"name"`
	RemoteHealthy       bool    `json:"remote_healthy"`
	OutageDurationSec   float64 `json:"outage_duration_seconds,omitempty"`
	PendingUploads      int     `json:"pending_uploads,omitempty"`
	OfflineReadsBlocked int64   `json:"offline_reads_blocked,omitempty"`
}
