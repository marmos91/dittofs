package apiclient

import (
	"fmt"
	"net/url"
	"time"
)

// ClientInfo represents a connected NFS client returned by the API.
type ClientInfo struct {
	ClientID    string    `json:"client_id"`
	Address     string    `json:"address"`
	NFSVersion  string    `json:"nfs_version"`
	ConnectedAt time.Time `json:"connected_at"`
	LastRenewal time.Time `json:"last_renewal"`
	LeaseStatus string    `json:"lease_status"`
	Confirmed   bool      `json:"confirmed"`
	ImplName    string    `json:"impl_name,omitempty"`
	ImplDomain  string    `json:"impl_domain,omitempty"`
}

// ListClients returns all connected NFS clients.
func (c *Client) ListClients() ([]ClientInfo, error) {
	var clients []ClientInfo
	if err := c.get("/api/v1/adapters/nfs/clients", &clients); err != nil {
		return nil, err
	}
	return clients, nil
}

// EvictClient evicts a connected NFS client by hex client ID.
func (c *Client) EvictClient(clientID string) error {
	return c.delete(fmt.Sprintf("/api/v1/adapters/nfs/clients/%s", url.PathEscape(clientID)), nil)
}

// ConnectionInfo represents a single bound connection in a session.
type ConnectionInfo struct {
	ConnectionID uint64 `json:"connection_id"`
	Direction    string `json:"direction"`     // "fore", "back", "both"
	ConnType     string `json:"conn_type"`     // "TCP", "RDMA"
	BoundAt      string `json:"bound_at"`      // RFC3339
	LastActivity string `json:"last_activity"` // RFC3339
	Draining     bool   `json:"draining"`
}

// ConnectionSummary provides a per-direction breakdown of bound connections.
type ConnectionSummary struct {
	Fore  int `json:"fore"`
	Back  int `json:"back"`
	Both  int `json:"both"`
	Total int `json:"total"`
}

// SessionInfo represents an NFSv4.1 session returned by the API.
type SessionInfo struct {
	SessionID         string             `json:"session_id"`
	ClientID          string             `json:"client_id"`
	CreatedAt         time.Time          `json:"created_at"`
	ForeSlots         uint32             `json:"fore_slots"`
	BackSlots         uint32             `json:"back_slots"`
	Flags             uint32             `json:"flags"`
	BackChannel       bool               `json:"back_channel"`
	Connections       []ConnectionInfo   `json:"connections,omitempty"`
	ConnectionSummary *ConnectionSummary `json:"connection_summary,omitempty"`
}

// ListSessions returns all sessions for a given NFS client.
func (c *Client) ListSessions(clientID string) ([]SessionInfo, error) {
	var sessions []SessionInfo
	if err := c.get(fmt.Sprintf("/api/v1/adapters/nfs/clients/%s/sessions", url.PathEscape(clientID)), &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

// ForceDestroySession force-destroys a session by client ID and session ID.
func (c *Client) ForceDestroySession(clientID, sessionID string) error {
	return c.delete(fmt.Sprintf("/api/v1/adapters/nfs/clients/%s/sessions/%s",
		url.PathEscape(clientID), url.PathEscape(sessionID)), nil)
}
