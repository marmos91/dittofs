//go:build e2e

package helpers

import (
	"fmt"
	"strings"
)

// =============================================================================
// Share Management Types
// =============================================================================

// Share represents a share returned from the API.
type Share struct {
	Name              string `json:"name"`
	MetadataStoreID   string `json:"metadata_store_id"`
	PayloadStoreID    string `json:"payload_store_id"`
	ReadOnly          bool   `json:"read_only"`
	DefaultPermission string `json:"default_permission"`
	Description       string `json:"description,omitempty"`
}

// ShareOption is a functional option for share operations.
type ShareOption func(*shareOptions)

type shareOptions struct {
	readOnly          *bool
	defaultPermission string
	description       string
}

// WithShareReadOnly sets the read-only flag for the share.
func WithShareReadOnly(readOnly bool) ShareOption {
	return func(o *shareOptions) {
		o.readOnly = &readOnly
	}
}

// WithShareDefaultPermission sets the default permission level for the share.
// Valid values: "none", "read", "read-write", "admin"
func WithShareDefaultPermission(permission string) ShareOption {
	return func(o *shareOptions) {
		o.defaultPermission = permission
	}
}

// WithShareDescription sets the description for the share.
func WithShareDescription(desc string) ShareOption {
	return func(o *shareOptions) {
		o.description = desc
	}
}

// =============================================================================
// Share CRUD Methods
// =============================================================================

// CreateShare creates a new share via the CLI.
// Returns the created share or an error with CLI output on failure.
// Share names MUST have a leading slash (e.g., "/myshare").
func (r *CLIRunner) CreateShare(name, metadataStore, payloadStore string, opts ...ShareOption) (*Share, error) {
	options := &shareOptions{}
	for _, opt := range opts {
		opt(options)
	}

	args := []string{"share", "create", "--name", name, "--metadata", metadataStore, "--payload", payloadStore}

	if options.readOnly != nil {
		args = append(args, "--read-only", fmt.Sprintf("%t", *options.readOnly))
	}
	if options.defaultPermission != "" {
		args = append(args, "--default-permission", options.defaultPermission)
	}
	if options.description != "" {
		args = append(args, "--description", options.description)
	}

	output, err := r.Run(args...)
	if err != nil {
		return nil, err
	}

	var share Share
	if err := ParseJSONResponse(output, &share); err != nil {
		return nil, err
	}

	return &share, nil
}

// ListShares lists all shares via the CLI.
func (r *CLIRunner) ListShares() ([]*Share, error) {
	output, err := r.Run("share", "list")
	if err != nil {
		return nil, err
	}

	var shares []*Share
	if err := ParseJSONResponse(output, &shares); err != nil {
		return nil, err
	}

	return shares, nil
}

// GetShare retrieves a share by name.
// Since there's no dedicated 'share get' command in the CLI, this lists all
// shares and filters by name.
func (r *CLIRunner) GetShare(name string) (*Share, error) {
	shares, err := r.ListShares()
	if err != nil {
		return nil, err
	}

	for _, s := range shares {
		if s.Name == name {
			return s, nil
		}
	}

	return nil, fmt.Errorf("share not found: %s", name)
}

// EditShare edits an existing share via the CLI.
// At least one option must be provided.
func (r *CLIRunner) EditShare(name string, opts ...ShareOption) (*Share, error) {
	options := &shareOptions{}
	for _, opt := range opts {
		opt(options)
	}

	args := []string{"share", "edit", name}
	hasUpdate := false

	if options.readOnly != nil {
		args = append(args, "--read-only", fmt.Sprintf("%t", *options.readOnly))
		hasUpdate = true
	}
	if options.defaultPermission != "" {
		args = append(args, "--default-permission", options.defaultPermission)
		hasUpdate = true
	}
	if options.description != "" {
		args = append(args, "--description", options.description)
		hasUpdate = true
	}

	if !hasUpdate {
		return nil, fmt.Errorf("at least one option (WithShareReadOnly, WithShareDefaultPermission, or WithShareDescription) is required for EditShare")
	}

	output, err := r.Run(args...)
	if err != nil {
		return nil, err
	}

	var share Share
	if err := ParseJSONResponse(output, &share); err != nil {
		return nil, err
	}

	return &share, nil
}

// DeleteShare deletes a share via the CLI.
// Uses --force to skip confirmation prompt.
func (r *CLIRunner) DeleteShare(name string) error {
	_, err := r.Run("share", "delete", name, "--force")
	return err
}

// =============================================================================
// Share Permission Types
// =============================================================================

// SharePermission represents a permission on a share.
type SharePermission struct {
	Type  string `json:"type"`  // "user" or "group"
	Name  string `json:"name"`  // username or group name
	Level string `json:"level"` // "none", "read", "read-write", "admin"
}

// =============================================================================
// Share Permission Methods
// =============================================================================

// GrantUserPermission grants a permission level to a user on a share.
// Level must be one of: "none", "read", "read-write", "admin".
func (r *CLIRunner) GrantUserPermission(shareName, username, level string) error {
	_, err := r.Run("share", "permission", "grant", shareName, "--user", username, "--level", level)
	return err
}

// GrantGroupPermission grants a permission level to a group on a share.
// Level must be one of: "none", "read", "read-write", "admin".
func (r *CLIRunner) GrantGroupPermission(shareName, groupName, level string) error {
	_, err := r.Run("share", "permission", "grant", shareName, "--group", groupName, "--level", level)
	return err
}

// RevokeUserPermission revokes a user's permission from a share.
func (r *CLIRunner) RevokeUserPermission(shareName, username string) error {
	_, err := r.Run("share", "permission", "revoke", shareName, "--user", username)
	return err
}

// RevokeGroupPermission revokes a group's permission from a share.
func (r *CLIRunner) RevokeGroupPermission(shareName, groupName string) error {
	_, err := r.Run("share", "permission", "revoke", shareName, "--group", groupName)
	return err
}

// ListSharePermissions returns all permissions configured on a share.
// Returns an empty slice (not error) if no permissions are configured.
func (r *CLIRunner) ListSharePermissions(shareName string) ([]*SharePermission, error) {
	output, err := r.Run("share", "permission", "list", shareName)
	if err != nil {
		return nil, err
	}

	// Handle empty output or "null" JSON response
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" || trimmed == "null" || trimmed == "[]" {
		return []*SharePermission{}, nil
	}

	var perms []*SharePermission
	if err := ParseJSONResponse(output, &perms); err != nil {
		return nil, err
	}

	// Ensure we never return nil slice
	if perms == nil {
		return []*SharePermission{}, nil
	}

	return perms, nil
}
