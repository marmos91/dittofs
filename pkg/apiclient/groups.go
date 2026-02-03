package apiclient

import (
	"fmt"
)

// Group represents a group in the system.
type Group struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	GID         *uint32  `json:"gid,omitempty"`
	Description string   `json:"description,omitempty"`
	Members     []string `json:"members,omitempty"`
}

// CreateGroupRequest is the request to create a group.
type CreateGroupRequest struct {
	Name        string  `json:"name"`
	GID         *uint32 `json:"gid,omitempty"`
	Description string  `json:"description,omitempty"`
}

// UpdateGroupRequest is the request to update a group.
type UpdateGroupRequest struct {
	GID         *uint32 `json:"gid,omitempty"`
	Description *string `json:"description,omitempty"`
}

// ListGroups returns all groups.
func (c *Client) ListGroups() ([]Group, error) {
	var groups []Group
	if err := c.get("/api/v1/groups", &groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// GetGroup returns a group by name.
func (c *Client) GetGroup(name string) (*Group, error) {
	var group Group
	if err := c.get(fmt.Sprintf("/api/v1/groups/%s", name), &group); err != nil {
		return nil, err
	}
	return &group, nil
}

// CreateGroup creates a new group.
func (c *Client) CreateGroup(req *CreateGroupRequest) (*Group, error) {
	var group Group
	if err := c.post("/api/v1/groups", req, &group); err != nil {
		return nil, err
	}
	return &group, nil
}

// UpdateGroup updates an existing group.
func (c *Client) UpdateGroup(name string, req *UpdateGroupRequest) (*Group, error) {
	var group Group
	if err := c.put(fmt.Sprintf("/api/v1/groups/%s", name), req, &group); err != nil {
		return nil, err
	}
	return &group, nil
}

// DeleteGroup deletes a group.
func (c *Client) DeleteGroup(name string) error {
	return c.delete(fmt.Sprintf("/api/v1/groups/%s", name), nil)
}

// AddGroupMember adds a user to a group.
func (c *Client) AddGroupMember(groupName, username string) error {
	req := map[string]string{"username": username}
	return c.post(fmt.Sprintf("/api/v1/groups/%s/members", groupName), req, nil)
}

// RemoveGroupMember removes a user from a group.
func (c *Client) RemoveGroupMember(groupName, username string) error {
	return c.delete(fmt.Sprintf("/api/v1/groups/%s/members/%s", groupName, username), nil)
}
