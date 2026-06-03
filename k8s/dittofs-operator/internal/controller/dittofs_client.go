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
	"context"
	"crypto/tls"
	"crypto/x509"
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
// The 10s timeout is a safety net for in-cluster calls; the reconciler's context
// provides cancellation on shutdown and per-request deadlines.
func NewDittoFSClient(baseURL string) *DittoFSClient {
	return &DittoFSClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// NewDittoFSClientWithCA is NewDittoFSClient that additionally trusts the given
// PEM CA bundle when reaching an https base URL. This lets the operator verify
// a pod-served native-TLS certificate signed by a private CA (e.g. the ca.crt
// in a cert-manager-issued Secret) WITHOUT disabling certificate verification.
// When caPEM is empty the client behaves exactly like NewDittoFSClient (system
// roots only). An empty/invalid bundle is reported as an error rather than
// silently falling back, so a misconfigured CA cannot mask an unverified
// connection.
func NewDittoFSClientWithCA(baseURL string, caPEM []byte) (*DittoFSClient, error) {
	if len(caPEM) == 0 {
		return NewDittoFSClient(baseURL), nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("control plane CA bundle contains no valid PEM certificate")
	}
	return &DittoFSClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
					RootCAs:    pool,
				},
			},
		},
	}, nil
}

// SetToken sets the authentication token for subsequent requests.
func (c *DittoFSClient) SetToken(token string) {
	c.token = token
}

// do performs an HTTP request and decodes the response.
func (c *DittoFSClient) do(ctx context.Context, method, path string, body, result any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
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
		if json.Unmarshal(respBody, &apiErr) == nil &&
			(apiErr.Title != "" || apiErr.Status != 0 || apiErr.Code != "" || apiErr.Detail != "") {
			apiErr.StatusCode = resp.StatusCode
			return &apiErr
		}
		// Non-JSON or empty body: still return a typed error so callers can
		// classify on the HTTP status. Never lose the type via fmt.Errorf.
		return &DittoFSAPIError{StatusCode: resp.StatusCode, Detail: string(respBody)}
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}

// DittoFSAPIError represents an RFC 7807 problem+json error response from the
// DittoFS API. The server emits {"type","title","status","detail"} (and
// optionally "code"/"hint"); StatusCode carries the HTTP status and is
// authoritative for classification. The operator cannot import
// pkg/apiclient (separate go.mod), so this mirrors the canonical wire shape
// defined in internal/controlplane/api/handlers/problem.go.
type DittoFSAPIError struct {
	Type   string `json:"type,omitempty"`
	Title  string `json:"title,omitempty"`
	Status int    `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code,omitempty"`
	Hint   string `json:"hint,omitempty"`

	// StatusCode is the HTTP status code, authoritative for classification.
	StatusCode int `json:"-"`
}

// Error implements the error interface.
func (e *DittoFSAPIError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	if e.Title != "" {
		return e.Title
	}
	return fmt.Sprintf("request failed with status %d", e.StatusCode)
}

// IsConflict returns true if this is a conflict error (e.g., user already exists).
func (e *DittoFSAPIError) IsConflict() bool {
	return e.StatusCode == 409
}

// IsAuthError returns true if this is an authentication/authorization error.
func (e *DittoFSAPIError) IsAuthError() bool {
	return e.StatusCode == 401 || e.StatusCode == 403
}

// IsNotFound returns true if this is a not-found error.
func (e *DittoFSAPIError) IsNotFound() bool {
	return e.StatusCode == 404
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
func (c *DittoFSClient) Login(ctx context.Context, username, password string) (*TokenResponse, error) {
	req := LoginRequest{
		Username: username,
		Password: password,
	}

	var resp TokenResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/auth/login", req, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// RefreshToken refreshes the access token using the refresh token.
func (c *DittoFSClient) RefreshToken(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	req := struct {
		RefreshToken string `json:"refresh_token"`
	}{
		RefreshToken: refreshToken,
	}

	var resp TokenResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/auth/refresh", req, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// CreateUser creates a new user on the DittoFS API.
func (c *DittoFSClient) CreateUser(ctx context.Context, username, password, role string) error {
	req := struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role,omitempty"`
	}{
		Username: username,
		Password: password,
		Role:     role,
	}

	return c.do(ctx, http.MethodPost, "/api/v1/users", req, nil)
}

// DeleteUser deletes a user from the DittoFS API.
func (c *DittoFSClient) DeleteUser(ctx context.Context, username string) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/api/v1/users/%s", username), nil, nil)
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
func (c *DittoFSClient) ListAdapters(ctx context.Context) ([]AdapterInfo, error) {
	var adapters []AdapterInfo
	if err := c.do(ctx, http.MethodGet, "/api/v1/adapters", nil, &adapters); err != nil {
		return nil, err
	}
	return adapters, nil
}

// NFSSettingsResponse represents the NFS adapter settings returned by the DittoFS API.
type NFSSettingsResponse struct {
	PortmapperEnabled bool `json:"portmapper_enabled"`
}

// GetNFSSettings calls GET /api/v1/adapters/nfs/settings and returns NFS settings.
func (c *DittoFSClient) GetNFSSettings(ctx context.Context) (*NFSSettingsResponse, error) {
	var settings NFSSettingsResponse
	if err := c.do(ctx, http.MethodGet, "/api/v1/adapters/nfs/settings", nil, &settings); err != nil {
		return nil, err
	}
	return &settings, nil
}

// EnablePortmapper calls PATCH /api/v1/adapters/nfs/settings to enable the portmapper.
func (c *DittoFSClient) EnablePortmapper(ctx context.Context) error {
	req := map[string]bool{"portmapper_enabled": true}
	return c.do(ctx, http.MethodPatch, "/api/v1/adapters/nfs/settings", req, nil)
}
