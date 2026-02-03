//go:build e2e

package helpers

import (
	"fmt"
)

// =============================================================================
// Group Management Types
// =============================================================================

// Group represents a group returned from the API.
type Group struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	GID         *uint32  `json:"gid,omitempty"`
	Description string   `json:"description,omitempty"`
	Members     []string `json:"members,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	UpdatedAt   string   `json:"updated_at,omitempty"`
}

// GroupOption is a functional option for group operations.
type GroupOption func(*groupOptions)

type groupOptions struct {
	description string
	gid         uint32
	hasGID      bool
}

// WithGroupDescription sets the group description.
func WithGroupDescription(desc string) GroupOption {
	return func(o *groupOptions) {
		o.description = desc
	}
}

// WithGroupGID sets the group GID.
func WithGroupGID(gid uint32) GroupOption {
	return func(o *groupOptions) {
		o.gid = gid
		o.hasGID = true
	}
}

// =============================================================================
// Group CRUD Methods
// =============================================================================

// CreateGroup creates a new group via the CLI.
// Returns the created group parsed from JSON output.
func (r *CLIRunner) CreateGroup(name string, opts ...GroupOption) (*Group, error) {
	options := &groupOptions{}
	for _, opt := range opts {
		opt(options)
	}

	args := []string{"group", "create", "--name", name}

	if options.description != "" {
		args = append(args, "--description", options.description)
	}
	if options.hasGID {
		args = append(args, "--gid", fmt.Sprintf("%d", options.gid))
	}

	output, err := r.Run(args...)
	if err != nil {
		return nil, err
	}

	var group Group
	if err := ParseJSONResponse(output, &group); err != nil {
		return nil, err
	}

	return &group, nil
}

// GetGroup retrieves a group by name.
// Since there's no dedicated 'group get' command in the CLI, this lists all
// groups and filters by name.
func (r *CLIRunner) GetGroup(name string) (*Group, error) {
	groups, err := r.ListGroups()
	if err != nil {
		return nil, err
	}

	for _, g := range groups {
		if g.Name == name {
			return g, nil
		}
	}

	return nil, fmt.Errorf("group not found: %s", name)
}

// ListGroups lists all groups via the CLI.
func (r *CLIRunner) ListGroups() ([]*Group, error) {
	output, err := r.Run("group", "list")
	if err != nil {
		return nil, err
	}

	var groups []*Group
	if err := ParseJSONResponse(output, &groups); err != nil {
		return nil, err
	}

	return groups, nil
}

// EditGroup edits an existing group via the CLI.
// Note: The CLI does not support renaming groups, only updating description and GID.
func (r *CLIRunner) EditGroup(name string, opts ...GroupOption) (*Group, error) {
	options := &groupOptions{}
	for _, opt := range opts {
		opt(options)
	}

	args := []string{"group", "edit", name}

	// Only add flags that were explicitly set
	if options.description != "" {
		args = append(args, "--description", options.description)
	}
	if options.hasGID {
		args = append(args, "--gid", fmt.Sprintf("%d", options.gid))
	}

	// If no options were provided, the CLI might enter interactive mode
	// which won't work in tests. We should at least have one option.
	if !options.hasGID && options.description == "" {
		return nil, fmt.Errorf("at least one option (WithGroupDescription or WithGroupGID) is required for EditGroup")
	}

	output, err := r.Run(args...)
	if err != nil {
		return nil, err
	}

	var group Group
	if err := ParseJSONResponse(output, &group); err != nil {
		return nil, err
	}

	return &group, nil
}

// DeleteGroup deletes a group via the CLI.
// Uses --force to skip confirmation prompt.
func (r *CLIRunner) DeleteGroup(name string) error {
	_, err := r.Run("group", "delete", name, "--force")
	return err
}

// AddGroupMember adds a user to a group via the CLI.
func (r *CLIRunner) AddGroupMember(groupName, username string) error {
	_, err := r.Run("group", "add-user", groupName, username)
	return err
}

// RemoveGroupMember removes a user from a group via the CLI.
func (r *CLIRunner) RemoveGroupMember(groupName, username string) error {
	_, err := r.Run("group", "remove-user", groupName, username)
	return err
}
