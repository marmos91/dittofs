package config

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/identity"
)

// CreateUserStore creates a UserStore from the configuration.
//
// This converts the configuration types (UserConfig, GroupConfig, GuestUserConfig)
// to the identity package types and creates a ConfigUserStore.
func (c *Config) CreateUserStore() (identity.UserStore, error) {
	// Convert groups
	groups := make([]*identity.Group, 0, len(c.Groups))
	for _, gc := range c.Groups {
		group, err := convertGroupConfig(&gc)
		if err != nil {
			return nil, fmt.Errorf("invalid group %q: %w", gc.Name, err)
		}
		groups = append(groups, group)
	}

	// Convert users
	users := make([]*identity.User, 0, len(c.Users))
	for _, uc := range c.Users {
		user, err := convertUserConfig(&uc)
		if err != nil {
			return nil, fmt.Errorf("invalid user %q: %w", uc.Username, err)
		}
		users = append(users, user)
	}

	// Convert guest config
	var guestConfig *identity.GuestConfig
	if c.Guest.Enabled {
		guestConfig = convertGuestConfig(&c.Guest)
	}

	return identity.NewConfigUserStore(users, groups, guestConfig)
}

// convertGroupConfig converts GroupConfig to identity.Group.
func convertGroupConfig(gc *GroupConfig) (*identity.Group, error) {
	sharePerms := make(map[string]identity.SharePermission)
	for shareName, permStr := range gc.SharePermissions {
		perm := identity.ParseSharePermission(permStr)
		if !perm.IsValid() {
			return nil, fmt.Errorf("invalid permission %q for share %q", permStr, shareName)
		}
		sharePerms[shareName] = perm
	}

	return &identity.Group{
		Name:             gc.Name,
		GID:              gc.GID,
		SID:              gc.SID,
		SharePermissions: sharePerms,
		Description:      gc.Description,
	}, nil
}

// convertUserConfig converts UserConfig to identity.User.
func convertUserConfig(uc *UserConfig) (*identity.User, error) {
	sharePerms := make(map[string]identity.SharePermission)
	for shareName, permStr := range uc.SharePermissions {
		perm := identity.ParseSharePermission(permStr)
		if !perm.IsValid() {
			return nil, fmt.Errorf("invalid permission %q for share %q", permStr, shareName)
		}
		sharePerms[shareName] = perm
	}

	return &identity.User{
		Username:         uc.Username,
		PasswordHash:     uc.PasswordHash,
		NTHash:           uc.NTHash,
		Enabled:          uc.Enabled,
		UID:              uc.UID,
		GID:              uc.GID,
		GIDs:             uc.GIDs,
		SID:              uc.SID,
		GroupSIDs:        uc.GroupSIDs,
		Groups:           uc.Groups,
		SharePermissions: sharePerms,
		DisplayName:      uc.DisplayName,
		Email:            uc.Email,
	}, nil
}

// convertGuestConfig converts GuestUserConfig to identity.GuestConfig.
func convertGuestConfig(gc *GuestUserConfig) *identity.GuestConfig {
	sharePerms := make(map[string]identity.SharePermission)
	for shareName, permStr := range gc.SharePermissions {
		perm := identity.ParseSharePermission(permStr)
		sharePerms[shareName] = perm
	}

	return &identity.GuestConfig{
		Enabled:          gc.Enabled,
		UID:              gc.UID,
		GID:              gc.GID,
		SharePermissions: sharePerms,
	}
}

// GetDefaultPermission returns the default permission for a share as identity.SharePermission.
func (sc *ShareConfig) GetDefaultPermission() identity.SharePermission {
	if sc.DefaultPermission == "" {
		return identity.PermissionNone
	}
	return identity.ParseSharePermission(sc.DefaultPermission)
}
