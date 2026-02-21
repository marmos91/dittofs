package apiclient

import "fmt"

// ClientInfo represents a connected NFS client returned by the API.
type ClientInfo struct {
	ClientID    string `json:"client_id"`
	Address     string `json:"address"`
	NFSVersion  string `json:"nfs_version"`
	ConnectedAt string `json:"connected_at"`
	LastRenewal string `json:"last_renewal"`
	LeaseStatus string `json:"lease_status"`
	Confirmed   bool   `json:"confirmed"`
	ImplName    string `json:"impl_name,omitempty"`
	ImplDomain  string `json:"impl_domain,omitempty"`
}

// ListClients returns all connected NFS clients.
func (c *Client) ListClients() ([]ClientInfo, error) {
	var clients []ClientInfo
	if err := c.get("/api/v1/clients", &clients); err != nil {
		return nil, err
	}
	return clients, nil
}

// EvictClient evicts a connected NFS client by hex client ID.
func (c *Client) EvictClient(clientID string) error {
	return c.delete(fmt.Sprintf("/api/v1/clients/%s", clientID), nil)
}
