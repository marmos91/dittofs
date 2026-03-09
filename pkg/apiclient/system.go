package apiclient

// DrainUploadsResponse represents the response from drain-uploads endpoint.
type DrainUploadsResponse struct {
	Status   string `json:"status"`
	Duration string `json:"duration"`
}

// DrainUploads waits for all in-flight block store uploads to complete.
// This is useful for benchmarking to ensure clean boundaries between workloads.
func (c *Client) DrainUploads() (*DrainUploadsResponse, error) {
	var resp DrainUploadsResponse
	if err := c.post("/api/v1/system/drain-uploads", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
