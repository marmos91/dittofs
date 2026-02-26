package apiclient

// Setting represents a server setting.
type Setting struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"` // "string", "int", "bool", etc.
}

// ListSettings returns all settings.
func (c *Client) ListSettings() ([]Setting, error) {
	return listResources[Setting](c, "/api/v1/settings")
}

// GetSetting returns a setting by key.
func (c *Client) GetSetting(key string) (*Setting, error) {
	return getResource[Setting](c, resourcePath("/api/v1/settings/%s", key))
}

// SetSetting sets a setting value.
func (c *Client) SetSetting(key, value string) (*Setting, error) {
	req := map[string]string{"value": value}
	return updateResource[Setting](c, resourcePath("/api/v1/settings/%s", key), req)
}

// DeleteSetting deletes a setting (resets to default).
func (c *Client) DeleteSetting(key string) error {
	return deleteResource(c, resourcePath("/api/v1/settings/%s", key))
}
