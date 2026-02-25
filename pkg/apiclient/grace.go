package apiclient

// GraceStatusResponse represents the grace period status returned by the API.
type GraceStatusResponse struct {
	Active           bool    `json:"active"`
	RemainingSeconds float64 `json:"remaining_seconds"`
	TotalDuration    string  `json:"total_duration,omitempty"`
	ExpectedClients  int     `json:"expected_clients"`
	ReclaimedClients int     `json:"reclaimed_clients"`
	StartedAt        string  `json:"started_at,omitempty"`
	Message          string  `json:"message"`
}

// GraceStatus returns the current grace period status.
// This endpoint is unauthenticated (no token required).
func (c *Client) GraceStatus() (*GraceStatusResponse, error) {
	var resp GraceStatusResponse
	if err := c.get("/api/v1/grace", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ForceEndGrace force-ends the grace period (admin only).
func (c *Client) ForceEndGrace() error {
	return c.post("/api/v1/adapters/nfs/grace/end", nil, nil)
}
