/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DittoFSClient is a minimal HTTP client for the DittoFS REST API.
// It implements only the 4 methods needed by the operator: Login, CreateUser,
// RefreshToken, and DeleteUser. This avoids importing the full pkg/apiclient
// dependency tree into the operator module.
type DittoFSClient struct {
	baseURL    string
	httpClient *http.Client
	token      string
}

// NewDittoFSClient creates a new DittoFS API client with the given base URL.
func NewDittoFSClient(baseURL string) *DittoFSClient {
	return &DittoFSClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// SetToken sets the authentication token for subsequent requests.
func (c *DittoFSClient) SetToken(token string) {
	c.token = token
}

// do performs an HTTP request and decodes the response.
func (c *DittoFSClient) do(method, path string, body, result any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		var apiErr DittoFSAPIError
		if json.Unmarshal(respBody, &apiErr) == nil && apiErr.Message != "" {
			return &apiErr
		}
		return fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}

// DittoFSAPIError represents an error response from the DittoFS API.
type DittoFSAPIError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// Error implements the error interface.
func (e *DittoFSAPIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return e.Message
}

// IsConflict returns true if this is a conflict error (e.g., user already exists).
func (e *DittoFSAPIError) IsConflict() bool {
	return e.Code == "CONFLICT"
}

// IsAuthError returns true if this is an authentication/authorization error.
func (e *DittoFSAPIError) IsAuthError() bool {
	return e.Code == "UNAUTHORIZED" || e.Code == "FORBIDDEN"
}

// IsNotFound returns true if this is a not-found error.
func (e *DittoFSAPIError) IsNotFound() bool {
	return e.Code == "NOT_FOUND"
}

// LoginRequest represents a login request to the DittoFS API.
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

// Login authenticates with the DittoFS API and returns tokens.
func (c *DittoFSClient) Login(username, password string) (*TokenResponse, error) {
	req := LoginRequest{
		Username: username,
		Password: password,
	}

	var resp TokenResponse
	if err := c.do(http.MethodPost, "/api/v1/auth/login", req, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// RefreshToken refreshes the access token using the refresh token.
func (c *DittoFSClient) RefreshToken(refreshToken string) (*TokenResponse, error) {
	req := struct {
		RefreshToken string `json:"refresh_token"`
	}{
		RefreshToken: refreshToken,
	}

	var resp TokenResponse
	if err := c.do(http.MethodPost, "/api/v1/auth/refresh", req, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// CreateUser creates a new user on the DittoFS API.
func (c *DittoFSClient) CreateUser(username, password, role string) error {
	req := struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role,omitempty"`
	}{
		Username: username,
		Password: password,
		Role:     role,
	}

	return c.do(http.MethodPost, "/api/v1/users", req, nil)
}

// DeleteUser deletes a user from the DittoFS API.
func (c *DittoFSClient) DeleteUser(username string) error {
	return c.do(http.MethodDelete, fmt.Sprintf("/api/v1/users/%s", username), nil, nil)
}

// AdapterInfo represents an adapter returned by the DittoFS API.
// Only fields needed by the operator are included.
type AdapterInfo struct {
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
	Running bool   `json:"running"`
	Port    int    `json:"port"`
}

// ListAdapters calls GET /api/v1/adapters and returns the adapter list.
func (c *DittoFSClient) ListAdapters() ([]AdapterInfo, error) {
	var adapters []AdapterInfo
	if err := c.do(http.MethodGet, "/api/v1/adapters", nil, &adapters); err != nil {
		return nil, err
	}
	return adapters, nil
}
