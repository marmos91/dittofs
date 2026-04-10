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

// identityMappingsPath is the API path for identity mappings.
// Mappings are shared across protocols (NFS and SMB) — the /nfs/ segment
// is the canonical path but the same data is accessible via /smb/.
const identityMappingsPath = "/api/v1/adapters/nfs/identity-mappings"

// ListIdentityMappings returns all identity mappings.
func (c *Client) ListIdentityMappings() ([]IdentityMapping, error) {
	return listResources[IdentityMapping](c, identityMappingsPath)
}

// CreateIdentityMapping creates a new identity mapping.
func (c *Client) CreateIdentityMapping(principal, username string) (*IdentityMapping, error) {
	req := &CreateIdentityMappingRequest{
		Principal: principal,
		Username:  username,
	}
	return createResource[IdentityMapping](c, identityMappingsPath, req)
}

// DeleteIdentityMapping deletes an identity mapping by principal.
func (c *Client) DeleteIdentityMapping(principal string) error {
	return deleteResource(c, fmt.Sprintf("%s/%s", identityMappingsPath, url.PathEscape(principal)))
}
