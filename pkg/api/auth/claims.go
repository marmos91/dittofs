// Package auth provides JWT authentication functionality for the DittoFS API.
package auth

import (
	"slices"

	"github.com/golang-jwt/jwt/v5"
)

// TokenType indicates whether a token is an access token or refresh token.
type TokenType string

const (
	// TokenTypeAccess is a short-lived token used for API authorization.
	TokenTypeAccess TokenType = "access"
	// TokenTypeRefresh is a long-lived token used to obtain new access tokens.
	TokenTypeRefresh TokenType = "refresh"
)

// Claims represents JWT claims for DittoFS authentication.
//
// This uses abstract identity (username, role, groups) rather than
// protocol-specific identity (UID/GID/SID). Protocol-specific identity
// is resolved per-share via ShareIdentityMapping.
type Claims struct {
	jwt.RegisteredClaims

	// UserID is the unique identifier (UUID) for the user.
	UserID string `json:"uid"`

	// Username is the human-readable username.
	Username string `json:"username"`

	// Role is the user's role ("admin" or "user").
	Role string `json:"role"`

	// Groups is the list of DittoFS group names the user belongs to.
	Groups []string `json:"groups,omitempty"`

	// TokenType indicates whether this is an access or refresh token.
	TokenType TokenType `json:"token_type"`

	// MustChangePassword indicates the user must change their password.
	// When true, most API operations are blocked until password is changed.
	MustChangePassword bool `json:"must_change_password,omitempty"`
}

// IsAccessToken returns true if this is an access token.
func (c *Claims) IsAccessToken() bool {
	return c.TokenType == TokenTypeAccess
}

// IsRefreshToken returns true if this is a refresh token.
func (c *Claims) IsRefreshToken() bool {
	return c.TokenType == TokenTypeRefresh
}

// IsAdmin returns true if the user has admin role.
func (c *Claims) IsAdmin() bool {
	return c.Role == "admin"
}

// HasGroup returns true if the user belongs to the specified group.
func (c *Claims) HasGroup(groupName string) bool {
	return slices.Contains(c.Groups, groupName)
}
