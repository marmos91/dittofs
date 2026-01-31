package apiclient

import (
	"time"
)

// LoginRequest represents a login request.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// TokenResponse represents the response from login/refresh endpoints.
type TokenResponse struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresIn    int64     `json:"expires_in"` // seconds
	ExpiresAt    time.Time `json:"expires_at"`
}

// ExpiresInDuration returns ExpiresIn as a time.Duration.
func (t *TokenResponse) ExpiresInDuration() time.Duration {
	return time.Duration(t.ExpiresIn) * time.Second
}

// Login authenticates with the server and returns tokens.
func (c *Client) Login(username, password string) (*TokenResponse, error) {
	req := LoginRequest{
		Username: username,
		Password: password,
	}

	var resp TokenResponse
	if err := c.post("/api/v1/auth/login", req, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// RefreshToken refreshes the access token using the refresh token.
func (c *Client) RefreshToken(refreshToken string) (*TokenResponse, error) {
	req := struct {
		RefreshToken string `json:"refresh_token"`
	}{
		RefreshToken: refreshToken,
	}

	var resp TokenResponse
	if err := c.post("/api/v1/auth/refresh", req, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// Logout invalidates the current tokens.
func (c *Client) Logout() error {
	return c.post("/api/v1/auth/logout", nil, nil)
}
