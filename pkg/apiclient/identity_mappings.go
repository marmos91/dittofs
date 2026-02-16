package apiclient

import (
	"fmt"
	"net/url"
)

// IdentityMapping represents an identity mapping in the system.
type IdentityMapping struct {
	ID        string `json:"id"`
	Principal string `json:"principal"`
	Username  string `json:"username"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// CreateIdentityMappingRequest is the request to create an identity mapping.
type CreateIdentityMappingRequest struct {
	Principal string `json:"principal"`
	Username  string `json:"username"`
}

// ListIdentityMappings returns all identity mappings.
func (c *Client) ListIdentityMappings() ([]IdentityMapping, error) {
	var mappings []IdentityMapping
	if err := c.get("/api/v1/identity-mappings", &mappings); err != nil {
		return nil, err
	}
	return mappings, nil
}

// CreateIdentityMapping creates a new identity mapping.
func (c *Client) CreateIdentityMapping(principal, username string) (*IdentityMapping, error) {
	req := &CreateIdentityMappingRequest{
		Principal: principal,
		Username:  username,
	}
	var mapping IdentityMapping
	if err := c.post("/api/v1/identity-mappings", req, &mapping); err != nil {
		return nil, err
	}
	return &mapping, nil
}

// DeleteIdentityMapping deletes an identity mapping by principal.
func (c *Client) DeleteIdentityMapping(principal string) error {
	path := fmt.Sprintf("/api/v1/identity-mappings/%s", url.PathEscape(principal))
	return c.delete(path, nil)
}
