package apiclient

import (
	"fmt"
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
	var netgroup Netgroup
	if err := c.get(fmt.Sprintf("/api/v1/adapters/nfs/netgroups/%s", name), &netgroup); err != nil {
		return nil, err
	}
	return &netgroup, nil
}

// CreateNetgroup creates a new netgroup.
func (c *Client) CreateNetgroup(name string) (*Netgroup, error) {
	req := map[string]string{"name": name}
	var netgroup Netgroup
	if err := c.post("/api/v1/adapters/nfs/netgroups", req, &netgroup); err != nil {
		return nil, err
	}
	return &netgroup, nil
}

// DeleteNetgroup deletes a netgroup by name.
func (c *Client) DeleteNetgroup(name string) error {
	return c.delete(fmt.Sprintf("/api/v1/adapters/nfs/netgroups/%s", name), nil)
}

// AddNetgroupMember adds a member to a netgroup.
func (c *Client) AddNetgroupMember(netgroupName, memberType, memberValue string) (*NetgroupMember, error) {
	req := map[string]string{
		"type":  memberType,
		"value": memberValue,
	}
	var member NetgroupMember
	if err := c.post(fmt.Sprintf("/api/v1/adapters/nfs/netgroups/%s/members", netgroupName), req, &member); err != nil {
		return nil, err
	}
	return &member, nil
}

// RemoveNetgroupMember removes a member from a netgroup.
func (c *Client) RemoveNetgroupMember(netgroupName, memberID string) error {
	return c.delete(fmt.Sprintf("/api/v1/adapters/nfs/netgroups/%s/members/%s", netgroupName, memberID), nil)
}
