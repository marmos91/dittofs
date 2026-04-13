package apiclient

import (
	"fmt"
	"net/url"
)

// IdentityMapping represents an identity mapping in the system.
type IdentityMapping struct {
	ID           string `json:"id"`
	ProviderName string `json:"provider_name"`
	Principal    string `json:"principal"`
	Username     string `json:"username"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// CreateIdentityMappingRequest is the request to create an identity mapping.
type CreateIdentityMappingRequest struct {
	ProviderName string `json:"provider_name"`
	Principal    string `json:"principal"`
	Username     string `json:"username"`
}

// ListIdentityMappings returns all identity mappings, optionally filtered by provider.
func (c *Client) ListIdentityMappings(provider string) ([]IdentityMapping, error) {
	path := "/api/v1/identity-mappings"
	if provider != "" {
		path += "?provider=" + url.QueryEscape(provider)
	}
	return listResources[IdentityMapping](c, path)
}

// CreateIdentityMapping creates a new identity mapping.
func (c *Client) CreateIdentityMapping(provider, principal, username string) (*IdentityMapping, error) {
	if provider == "" {
		provider = "kerberos"
	}
	req := &CreateIdentityMappingRequest{
		ProviderName: provider,
		Principal:    principal,
		Username:     username,
	}
	return createResource[IdentityMapping](c, "/api/v1/identity-mappings", req)
}

// DeleteIdentityMapping deletes an identity mapping by provider and principal.
func (c *Client) DeleteIdentityMapping(provider, principal string) error {
	if provider == "" {
		provider = "kerberos"
	}
	return deleteResource(c, fmt.Sprintf("/api/v1/identity-mappings/by-provider/%s/%s",
		url.PathEscape(provider), url.PathEscape(principal)))
}

// ListIdentityMappingsForUser returns all identity mappings for a user.
func (c *Client) ListIdentityMappingsForUser(username string) ([]IdentityMapping, error) {
	return listResources[IdentityMapping](c, fmt.Sprintf("/api/v1/identity-mappings/users/%s",
		url.PathEscape(username)))
}
