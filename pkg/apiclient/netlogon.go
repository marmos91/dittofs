package apiclient

// NetlogonStatus reports the live NETLOGON machine-account and secure-channel
// state as returned by GetNetlogonStatus. Times are RFC3339 strings and are
// empty when not applicable (never rotated / rotation disabled).
type NetlogonStatus struct {
	Enabled          bool     `json:"enabled"`
	Provider         string   `json:"provider"` // "offline" | "online-join"
	AccountName      string   `json:"account_name,omitempty"`
	Realm            string   `json:"realm,omitempty"`
	NetBIOSDomain    string   `json:"netbios_domain,omitempty"`
	DCAddresses      []string `json:"dc_addresses,omitempty"`
	Joined           bool     `json:"joined"`
	ChannelConnected bool     `json:"channel_connected"`
	RotationEnabled  bool     `json:"rotation_enabled"`
	RotationInterval string   `json:"rotation_interval,omitempty"`
	LastRotation     string   `json:"last_rotation,omitempty"`
	NextRotation     string   `json:"next_rotation,omitempty"`
}

// NetlogonRotateResult is the outcome of a forced machine-password rotation,
// carrying the post-rotation status.
type NetlogonRotateResult struct {
	OK      bool           `json:"ok"`
	Message string         `json:"message"`
	Status  NetlogonStatus `json:"status"`
}

// GetNetlogonStatus returns the live NETLOGON machine-account / secure-channel
// state (admin only). Returns a 404 APIError when NTLM pass-through is not
// configured on the server.
func (c *Client) GetNetlogonStatus() (*NetlogonStatus, error) {
	var out NetlogonStatus
	if err := c.get("/api/v1/netlogon/status", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RotateNetlogon forces a machine-password rotation now (admin only). It is only
// valid for the online-join provider; for the offline/static provider the server
// returns a 409 APIError.
func (c *Client) RotateNetlogon() (*NetlogonRotateResult, error) {
	var out NetlogonRotateResult
	if err := c.post("/api/v1/netlogon/rotate", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
