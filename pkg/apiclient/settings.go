package apiclient

import (
	"fmt"
)

// Setting represents a server setting.
type Setting struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"` // "string", "int", "bool", etc.
}

// ListSettings returns all settings.
func (c *Client) ListSettings() ([]Setting, error) {
	var settings []Setting
	if err := c.get("/api/v1/settings", &settings); err != nil {
		return nil, err
	}
	return settings, nil
}

// GetSetting returns a setting by key.
func (c *Client) GetSetting(key string) (*Setting, error) {
	var setting Setting
	if err := c.get(fmt.Sprintf("/api/v1/settings/%s", key), &setting); err != nil {
		return nil, err
	}
	return &setting, nil
}

// SetSetting sets a setting value.
func (c *Client) SetSetting(key, value string) (*Setting, error) {
	req := map[string]string{"value": value}
	var setting Setting
	if err := c.put(fmt.Sprintf("/api/v1/settings/%s", key), req, &setting); err != nil {
		return nil, err
	}
	return &setting, nil
}

// DeleteSetting deletes a setting (resets to default).
func (c *Client) DeleteSetting(key string) error {
	return c.delete(fmt.Sprintf("/api/v1/settings/%s", key), nil)
}
