package apiclient

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
	return listResources[Group](c, "/api/v1/groups")
}

// GetGroup returns a group by name.
func (c *Client) GetGroup(name string) (*Group, error) {
	return getResource[Group](c, resourcePath("/api/v1/groups/%s", name))
}

// CreateGroup creates a new group.
func (c *Client) CreateGroup(req *CreateGroupRequest) (*Group, error) {
	return createResource[Group](c, "/api/v1/groups", req)
}

// UpdateGroup updates an existing group.
func (c *Client) UpdateGroup(name string, req *UpdateGroupRequest) (*Group, error) {
	return updateResource[Group](c, resourcePath("/api/v1/groups/%s", name), req)
}

// DeleteGroup deletes a group.
func (c *Client) DeleteGroup(name string) error {
	return deleteResource(c, resourcePath("/api/v1/groups/%s", name))
}

// AddGroupMember adds a user to a group.
func (c *Client) AddGroupMember(groupName, username string) error {
	req := map[string]string{"username": username}
	return c.post(resourcePath("/api/v1/groups/%s/members", groupName), req, nil)
}

// RemoveGroupMember removes a user from a group.
func (c *Client) RemoveGroupMember(groupName, username string) error {
	return deleteResource(c, resourcePath("/api/v1/groups/%s/members/%s", groupName, username))
}
