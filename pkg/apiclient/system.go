package apiclient

import "time"

// DrainUploadsResponse represents the response from drain-uploads endpoint.
type DrainUploadsResponse struct {
	Status   string `json:"status"`
	Duration string `json:"duration"`
}

// drainUploadsHTTPTimeout bounds the client-side wait for drain-uploads. The
// endpoint blocks until every queued upload flushes or the server's own 5-minute
// cap fires, so the base 30s client timeout would kill a large drain long before
// the server answers (the failure that made cold-read benchmarks read warm).
const drainUploadsHTTPTimeout = 6 * time.Minute

// DrainUploads waits for all in-flight block store uploads to complete.
// This is useful for benchmarking to ensure clean boundaries between workloads.
func (c *Client) DrainUploads() (*DrainUploadsResponse, error) {
	var resp DrainUploadsResponse
	if err := c.doWithTimeout("POST", "/api/v1/system/drain-uploads", nil, &resp, drainUploadsHTTPTimeout); err != nil {
		return nil, err
	}
	return &resp, nil
}
