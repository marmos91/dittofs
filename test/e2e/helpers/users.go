//go:build e2e

package helpers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// =============================================================================
// User Management Types
// =============================================================================

// User represents a user in the system (matches API response).
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
	CreatedAt          string            `json:"created_at,omitempty"`
	UpdatedAt          string            `json:"updated_at,omitempty"`
}

// TokenResponse represents the response from login/password change endpoints.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    string `json:"expires_at"`
}

// UserOption is a functional option for user operations.
type UserOption func(*userOptions)

type userOptions struct {
	email       string
	displayName string
	role        string
	uid         *uint32
	groups      []string
	enabled     *bool
}

// WithEmail sets the email for user creation/edit.
func WithEmail(email string) UserOption {
	return func(o *userOptions) {
		o.email = email
	}
}

// WithDisplayName sets the display name for user creation/edit.
func WithDisplayName(name string) UserOption {
	return func(o *userOptions) {
		o.displayName = name
	}
}

// WithRole sets the role for user creation/edit.
func WithRole(role string) UserOption {
	return func(o *userOptions) {
		o.role = role
	}
}

// WithUID sets the UID for user creation/edit.
func WithUID(uid uint32) UserOption {
	return func(o *userOptions) {
		o.uid = &uid
	}
}

// WithGroups sets the groups for user creation/edit.
func WithGroups(groups ...string) UserOption {
	return func(o *userOptions) {
		o.groups = groups
	}
}

// WithEnabled sets the enabled status for user creation/edit.
func WithEnabled(enabled bool) UserOption {
	return func(o *userOptions) {
		o.enabled = &enabled
	}
}

// =============================================================================
// User CRUD Methods
// =============================================================================

// CreateUser creates a new user via dfsctl.
// Returns the created user or an error with CLI output on failure.
func (r *CLIRunner) CreateUser(username, password string, opts ...UserOption) (*User, error) {
	o := &userOptions{}
	for _, opt := range opts {
		opt(o)
	}

	args := []string{"user", "create", "--username", username, "--password", password}

	if o.email != "" {
		args = append(args, "--email", o.email)
	}
	if o.displayName != "" {
		args = append(args, "--display-name", o.displayName)
	}
	if o.role != "" {
		args = append(args, "--role", o.role)
	}
	if o.uid != nil {
		args = append(args, "--uid", fmt.Sprintf("%d", *o.uid))
	}
	if len(o.groups) > 0 {
		args = append(args, "--groups", strings.Join(o.groups, ","))
	}
	if o.enabled != nil {
		args = append(args, "--enabled", fmt.Sprintf("%t", *o.enabled))
	}

	output, err := r.Run(args...)
	if err != nil {
		return nil, fmt.Errorf("create user failed: %w\noutput: %s", err, string(output))
	}

	var user User
	if err := ParseJSONResponse(output, &user); err != nil {
		return nil, err
	}

	return &user, nil
}

// GetUser retrieves a user by username.
func (r *CLIRunner) GetUser(username string) (*User, error) {
	output, err := r.Run("user", "get", username)
	if err != nil {
		return nil, fmt.Errorf("get user failed: %w\noutput: %s", err, string(output))
	}

	var user User
	if err := ParseJSONResponse(output, &user); err != nil {
		return nil, err
	}

	return &user, nil
}

// ListUsers retrieves all users.
func (r *CLIRunner) ListUsers() ([]*User, error) {
	output, err := r.Run("user", "list")
	if err != nil {
		return nil, fmt.Errorf("list users failed: %w\noutput: %s", err, string(output))
	}

	var users []*User
	if err := ParseJSONResponse(output, &users); err != nil {
		return nil, err
	}

	return users, nil
}

// EditUser updates an existing user.
// At least one option must be provided.
func (r *CLIRunner) EditUser(username string, opts ...UserOption) (*User, error) {
	o := &userOptions{}
	for _, opt := range opts {
		opt(o)
	}

	args := []string{"user", "edit", username}

	if o.email != "" {
		args = append(args, "--email", o.email)
	}
	if o.displayName != "" {
		args = append(args, "--display-name", o.displayName)
	}
	if o.role != "" {
		args = append(args, "--role", o.role)
	}
	if o.uid != nil {
		args = append(args, "--uid", fmt.Sprintf("%d", *o.uid))
	}
	if len(o.groups) > 0 {
		args = append(args, "--groups", strings.Join(o.groups, ","))
	}
	if o.enabled != nil {
		args = append(args, "--enabled", fmt.Sprintf("%t", *o.enabled))
	}

	output, err := r.Run(args...)
	if err != nil {
		return nil, fmt.Errorf("edit user failed: %w\noutput: %s", err, string(output))
	}

	var user User
	if err := ParseJSONResponse(output, &user); err != nil {
		return nil, err
	}

	return &user, nil
}

// DeleteUser deletes a user by username.
// Uses --force to skip confirmation prompt.
func (r *CLIRunner) DeleteUser(username string) error {
	output, err := r.Run("user", "delete", username, "--force")
	if err != nil {
		return fmt.Errorf("delete user failed: %w\noutput: %s", err, string(output))
	}

	return nil
}

// ChangeOwnPassword changes the current user's password.
// Returns new tokens if the password change invalidates the current token.
func (r *CLIRunner) ChangeOwnPassword(currentPassword, newPassword string) (*TokenResponse, error) {
	output, err := r.Run("user", "change-password", "--current", currentPassword, "--new", newPassword)
	if err != nil {
		return nil, fmt.Errorf("change password failed: %w\noutput: %s", err, string(output))
	}

	// change-password may return new tokens or just a success message
	// The CLI prints "Password changed successfully" on success
	// If tokens are returned in the output, parse them
	if len(output) == 0 || !strings.Contains(string(output), "access_token") {
		return nil, nil
	}

	var tokens TokenResponse
	if err := ParseJSONResponse(output, &tokens); err != nil {
		// Not an error if no tokens in response - password change succeeded
		return nil, nil
	}

	return &tokens, nil
}

// ResetPassword resets a user's password (admin operation).
// This sets the user's password and marks them as needing to change it on next login.
func (r *CLIRunner) ResetPassword(username, newPassword string) error {
	output, err := r.Run("user", "password", username, "--password", newPassword)
	if err != nil {
		return fmt.Errorf("reset password failed: %w\noutput: %s", err, string(output))
	}

	return nil
}

// Login authenticates with the server and returns a new CLIRunner with the token.
// Returns the access token on success.
func (r *CLIRunner) Login(serverURL, username, password string) (string, error) {
	// The login command doesn't support --output json and --token flags
	output, err := r.RunRaw(
		"login",
		"--server", serverURL,
		"--username", username,
		"--password", password,
	)
	if err != nil {
		return "", fmt.Errorf("login failed: %w\noutput: %s", err, string(output))
	}

	// Extract token from credentials file
	token, err := extractTokenFromCredentialsFile(serverURL)
	if err != nil {
		return "", fmt.Errorf("failed to extract token after login: %w", err)
	}

	return token, nil
}

// LoginAsUser creates a new CLIRunner logged in as the specified user.
func (r *CLIRunner) LoginAsUser(serverURL, username, password string) (*CLIRunner, error) {
	// Create a new runner for this user
	newRunner := NewCLIRunner(serverURL, "")

	token, err := newRunner.Login(serverURL, username, password)
	if err != nil {
		return nil, err
	}

	newRunner.SetToken(token)
	return newRunner, nil
}

// extractTokenFromCredentialsFile reads the token from the credentials file.
// This is the internal version used by other helpers.
func extractTokenFromCredentialsFile(serverURL string) (string, error) {
	return ExtractTokenFromCredentialsFile(serverURL)
}

// ExtractTokenFromCredentialsFile reads the token from the credentials file.
// Exported version for use in tests that need direct token extraction.
func ExtractTokenFromCredentialsFile(serverURL string) (string, error) {
	// Get credentials file path (matches internal/cli/credentials/store.go)
	// Uses XDG_CONFIG_HOME or ~/.config
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home dir: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}

	credFile := filepath.Join(configHome, "dfsctl", "config.json")
	data, err := os.ReadFile(credFile)
	if err != nil {
		return "", fmt.Errorf("failed to read credentials file: %w", err)
	}

	var creds map[string]interface{}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("failed to parse credentials file: %w", err)
	}

	contexts, ok := creds["contexts"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("no contexts found in credentials file")
	}

	for _, ctx := range contexts {
		ctxMap, ok := ctx.(map[string]interface{})
		if !ok {
			continue
		}
		// Field name is "server_url" in JSON (snake_case)
		if ctxMap["server_url"] == serverURL {
			if token, ok := ctxMap["access_token"].(string); ok {
				return token, nil
			}
		}
	}

	return "", fmt.Errorf("no token found for server %s in credentials file", serverURL)
}
