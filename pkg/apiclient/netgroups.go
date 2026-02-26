package apiclient

import (
	"time"
)

// Netgroup represents a netgroup resource.
type Netgroup struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Members   []NetgroupMember `json:"members,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// NetgroupMember represents a member of a netgroup.
type NetgroupMember struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// ListNetgroups returns all netgroups.
func (c *Client) ListNetgroups() ([]*Netgroup, error) {
	var netgroups []*Netgroup
	if err := c.get("/api/v1/adapters/nfs/netgroups", &netgroups); err != nil {
		return nil, err
	}
	return netgroups, nil
}

// GetNetgroup returns a netgroup by name.
func (c *Client) GetNetgroup(name string) (*Netgroup, error) {
	return getResource[Netgroup](c, resourcePath("/api/v1/adapters/nfs/netgroups/%s", name))
}

// CreateNetgroup creates a new netgroup.
func (c *Client) CreateNetgroup(name string) (*Netgroup, error) {
	req := map[string]string{"name": name}
	return createResource[Netgroup](c, "/api/v1/adapters/nfs/netgroups", req)
}

// DeleteNetgroup deletes a netgroup by name.
func (c *Client) DeleteNetgroup(name string) error {
	return deleteResource(c, resourcePath("/api/v1/adapters/nfs/netgroups/%s", name))
}

// AddNetgroupMember adds a member to a netgroup.
func (c *Client) AddNetgroupMember(netgroupName, memberType, memberValue string) (*NetgroupMember, error) {
	req := map[string]string{
		"type":  memberType,
		"value": memberValue,
	}
	return createResource[NetgroupMember](c, resourcePath("/api/v1/adapters/nfs/netgroups/%s/members", netgroupName), req)
}

// RemoveNetgroupMember removes a member from a netgroup.
func (c *Client) RemoveNetgroupMember(netgroupName, memberID string) error {
	return deleteResource(c, resourcePath("/api/v1/adapters/nfs/netgroups/%s/members/%s", netgroupName, memberID))
}
