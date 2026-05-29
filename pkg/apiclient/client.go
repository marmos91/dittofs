// Package apiclient provides a REST API client for dfsctl.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is the DittoFS API client.
type Client struct {
	baseURL            string
	httpClient         *http.Client
	token              string
	restoreHTTPTimeout time.Duration
}

// ClientOption configures a Client at construction time.
type ClientOption func(*Client)

// WithRestoreTimeout overrides the http.Client timeout used for restore
// calls. The default is 30 minutes. Other endpoints continue to use the
// default 30 second client timeout.
func WithRestoreTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.restoreHTTPTimeout = d }
}

// defaultRestoreHTTPTimeout is the timeout applied to the restore HTTP call
// when no override is supplied. Restore is an inherently long-running
// operation (full metadata-dump replay plus refcount rebuild), so the
// default 30 second http.Client timeout would routinely kill it.
const defaultRestoreHTTPTimeout = 30 * time.Minute

// New creates a new API client.
func New(baseURL string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		restoreHTTPTimeout: defaultRestoreHTTPTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// WithToken returns a new client with the given token.
func (c *Client) WithToken(token string) *Client {
	return &Client{
		baseURL:            c.baseURL,
		httpClient:         c.httpClient,
		token:              token,
		restoreHTTPTimeout: c.restoreHTTPTimeout,
	}
}

// SetToken sets the authentication token.
func (c *Client) SetToken(token string) {
	c.token = token
}

// Token returns the current authentication token.
func (c *Client) Token() string {
	return c.token
}

// BaseURL returns the base URL of the API client.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// do performs an HTTP request and decodes the response using the
// Client's default httpClient with a background context.
func (c *Client) do(method, path string, body, result any) error {
	return c.doVia(context.Background(), c.httpClient, method, path, body, result)
}

// doCtx performs an HTTP request and decodes the response using the
// Client's default httpClient, honoring the caller's context.
func (c *Client) doCtx(ctx context.Context, method, path string, body, result any) error {
	return c.doVia(ctx, c.httpClient, method, path, body, result)
}

// doVia performs an HTTP request and decodes the response, dispatching
// via the provided http.Client. This is the single I/O path used by both
// the default and the per-call-timeout flows. The context controls request
// cancellation and overall deadline; the http.Client's Timeout still acts
// as an upper bound.
func (c *Client) doVia(ctx context.Context, hc *http.Client, method, path string, body, result any) error {
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

	resp, err := hc.Do(req)
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
		return &APIError{StatusCode: resp.StatusCode, Message: string(respBody)}
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

// getCtx performs a GET request, propagating the caller's context so
// the underlying HTTP call is cancellable. Used by poll loops.
func (c *Client) getCtx(ctx context.Context, path string, result any) error {
	return c.doCtx(ctx, http.MethodGet, path, nil, result)
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

// doWithTimeout performs an HTTP request using a per-call timeout. It
// builds a transient http.Client that shares the underlying transport
// (so connection pooling still applies) and never mutates c.httpClient —
// safe for concurrent callers.
func (c *Client) doWithTimeout(method, path string, body, result any, timeout time.Duration) error {
	override := &http.Client{
		Transport:     c.httpClient.Transport,
		CheckRedirect: c.httpClient.CheckRedirect,
		Jar:           c.httpClient.Jar,
		Timeout:       timeout,
	}
	return c.doVia(context.Background(), override, method, path, body, result)
}
