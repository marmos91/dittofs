// Package apiclient provides a REST API client for dfsctl.
package apiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is the DittoFS API client.
type Client struct {
	baseURL    string
	httpClient *http.Client
	token      string
}

// New creates a new API client.
func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// WithToken returns a new client with the given token.
func (c *Client) WithToken(token string) *Client {
	return &Client{
		baseURL:    c.baseURL,
		httpClient: c.httpClient,
		token:      token,
	}
}

// SetToken sets the authentication token.
func (c *Client) SetToken(token string) {
	c.token = token
}

// do performs an HTTP request and decodes the response.
func (c *Client) do(method, path string, body, result any) error {
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
		var apiErr APIError
		if json.Unmarshal(respBody, &apiErr) == nil && apiErr.Message != "" {
			apiErr.StatusCode = resp.StatusCode
			return &apiErr
		}
		return &APIError{
			StatusCode: resp.StatusCode,
			Message:    string(respBody),
		}
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}

// get performs a GET request.
func (c *Client) get(path string, result any) error {
	return c.do(http.MethodGet, path, nil, result)
}

// post performs a POST request.
func (c *Client) post(path string, body, result any) error {
	return c.do(http.MethodPost, path, body, result)
}

// put performs a PUT request.
func (c *Client) put(path string, body, result any) error {
	return c.do(http.MethodPut, path, body, result)
}

// patch performs a PATCH request.
func (c *Client) patch(path string, body, result any) error {
	return c.do(http.MethodPatch, path, body, result)
}

// delete performs a DELETE request.
func (c *Client) delete(path string, result any) error {
	return c.do(http.MethodDelete, path, nil, result)
}
