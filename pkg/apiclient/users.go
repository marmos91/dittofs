package apiclient

import (
	"fmt"
	"time"
)

// User represents a user in the system.
type User struct {
	ID                 string            `json:"id"`
	Username           string            `json:"username"`
	DisplayName        string            `json:"display_name,omitempty"`
	Email              string            `json:"email,omitempty"`
	Role               string            `json:"role"`
	UID                *uint32           `json:"uid,omitempty"`
	Groups             []string          `json:"groups,omitempty"`
	Enabled            bool              `json:"enabled"`
	MustChangePassword bool              `json:"must_change_password"`
	SharePermissions   map[string]string `json:"share_permissions,omitempty"`
	CreatedAt          time.Time         `json:"created_at,omitempty"`
	UpdatedAt          time.Time         `json:"updated_at,omitempty"`
}

// CreateUserRequest is the request to create a user.
type CreateUserRequest struct {
	Username         string            `json:"username"`
	Password         string            `json:"password"`
	Email            string            `json:"email,omitempty"`
	DisplayName      string            `json:"display_name,omitempty"`
	Role             string            `json:"role,omitempty"`
	UID              *uint32           `json:"uid,omitempty"`
	Groups           []string          `json:"groups,omitempty"`
	Enabled          *bool             `json:"enabled,omitempty"`
	SharePermissions map[string]string `json:"share_permissions,omitempty"`
}

// UpdateUserRequest is the request to update a user.
type UpdateUserRequest struct {
	Email            *string            `json:"email,omitempty"`
	DisplayName      *string            `json:"display_name,omitempty"`
	Role             *string            `json:"role,omitempty"`
	UID              *uint32            `json:"uid,omitempty"`
	Groups           *[]string          `json:"groups,omitempty"`
	Enabled          *bool              `json:"enabled,omitempty"`
	SharePermissions *map[string]string `json:"share_permissions,omitempty"`
}

// ChangePasswordRequest is the request to change a password.
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password,omitempty"`
	NewPassword     string `json:"new_password"`
}

// ListUsers returns all users.
func (c *Client) ListUsers() ([]User, error) {
	var users []User
	if err := c.get("/api/v1/users", &users); err != nil {
		return nil, err
	}
	return users, nil
}

// GetUser returns a user by username.
func (c *Client) GetUser(username string) (*User, error) {
	var user User
	if err := c.get(fmt.Sprintf("/api/v1/users/%s", username), &user); err != nil {
		return nil, err
	}
	return &user, nil
}

// CreateUser creates a new user.
func (c *Client) CreateUser(req *CreateUserRequest) (*User, error) {
	var user User
	if err := c.post("/api/v1/users", req, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

// UpdateUser updates an existing user.
func (c *Client) UpdateUser(username string, req *UpdateUserRequest) (*User, error) {
	var user User
	if err := c.put(fmt.Sprintf("/api/v1/users/%s", username), req, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

// DeleteUser deletes a user.
func (c *Client) DeleteUser(username string) error {
	return c.delete(fmt.Sprintf("/api/v1/users/%s", username), nil)
}

// ResetUserPassword resets a user's password (admin operation).
func (c *Client) ResetUserPassword(username, newPassword string) error {
	req := &ChangePasswordRequest{NewPassword: newPassword}
	return c.post(fmt.Sprintf("/api/v1/users/%s/password", username), req, nil)
}

// ChangeOwnPassword changes the current user's password.
// Returns new tokens that should be saved to update credentials.
func (c *Client) ChangeOwnPassword(currentPassword, newPassword string) (*TokenResponse, error) {
	req := &ChangePasswordRequest{
		CurrentPassword: currentPassword,
		NewPassword:     newPassword,
	}
	var resp TokenResponse
	if err := c.post("/api/v1/users/me/password", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetCurrentUser returns the currently authenticated user.
func (c *Client) GetCurrentUser() (*User, error) {
	var user User
	if err := c.get("/api/v1/auth/me", &user); err != nil {
		return nil, err
	}
	return &user, nil
}
