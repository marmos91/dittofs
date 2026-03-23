package apiclient

import (
	"fmt"
	"net/url"
	"time"
)

// ClientRecord represents a connected protocol client returned by the unified API.
type ClientRecord struct {
	ClientID     string      `json:"client_id"`
	Protocol     string      `json:"protocol"`
	Address      string      `json:"address"`
	User         string      `json:"user"`
	ConnectedAt  time.Time   `json:"connected_at"`
	LastActivity time.Time   `json:"last_activity"`
	Shares       []string    `json:"shares"`
	NFS          *NfsDetails `json:"nfs,omitempty"`
	SMB          *SmbDetails `json:"smb,omitempty"`
}

// NfsDetails holds NFS-specific client information.
type NfsDetails struct {
	Version    string `json:"version"`
	AuthFlavor string `json:"auth_flavor"`
	UID        uint32 `json:"uid"`
	GID        uint32 `json:"gid"`
}

// SmbDetails holds SMB-specific client information.
type SmbDetails struct {
	SessionID uint64 `json:"session_id"`
	Dialect   string `json:"dialect"`
	Domain    string `json:"domain,omitempty"`
	Signed    bool   `json:"signed"`
	Encrypted bool   `json:"encrypted"`
}

// ListClientsOption configures a ListClients call.
type ListClientsOption func(*listClientsOpts)

type listClientsOpts struct {
	protocol string
	share    string
}

// WithProtocol filters client list by protocol ("nfs" or "smb").
func WithProtocol(protocol string) ListClientsOption {
	return func(o *listClientsOpts) { o.protocol = protocol }
}

// WithShare filters client list by share name.
func WithShare(share string) ListClientsOption {
	return func(o *listClientsOpts) { o.share = share }
}

// ListClients returns all connected clients, optionally filtered.
func (c *Client) ListClients(opts ...ListClientsOption) ([]ClientRecord, error) {
	var o listClientsOpts
	for _, opt := range opts {
		opt(&o)
	}

	query := url.Values{}
	if o.protocol != "" {
		query.Set("protocol", o.protocol)
	}
	if o.share != "" {
		query.Set("share", o.share)
	}

	path := "/api/v1/clients"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}

	return listResources[ClientRecord](c, path)
}

// DisconnectClient disconnects a client by ID with protocol-specific teardown.
func (c *Client) DisconnectClient(clientID string) error {
	return deleteResource(c, fmt.Sprintf("/api/v1/clients/%s", url.PathEscape(clientID)))
}

// ClientInfo is deprecated: use ClientRecord instead.
// Kept for backward compatibility with existing CLI code.
type ClientInfo = ClientRecord

// EvictClient is deprecated: use DisconnectClient instead.
func (c *Client) EvictClient(clientID string) error {
	return c.DisconnectClient(clientID)
}

// --- Legacy NFS-specific session types (kept for NFS session endpoints) ---

// ConnectionInfo represents a single bound connection in a session.
type ConnectionInfo struct {
	ConnectionID uint64 `json:"connection_id"`
	Direction    string `json:"direction"`
	ConnType     string `json:"conn_type"`
	BoundAt      string `json:"bound_at"`
	LastActivity string `json:"last_activity"`
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
	return listResources[SessionInfo](c, fmt.Sprintf("/api/v1/adapters/nfs/clients/%s/sessions", url.PathEscape(clientID)))
}

// ForceDestroySession force-destroys a session by client ID and session ID.
func (c *Client) ForceDestroySession(clientID, sessionID string) error {
	return deleteResource(c, fmt.Sprintf("/api/v1/adapters/nfs/clients/%s/sessions/%s",
		url.PathEscape(clientID), url.PathEscape(sessionID)))
}
