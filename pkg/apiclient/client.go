// Package apiclient provides a REST API client for dfsctl.
package apiclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Client is the DittoFS API client.
type Client struct {
	baseURL            string
	httpClient         *http.Client
	token              string
	restoreHTTPTimeout time.Duration

	// TLS intent captured by the With* options and resolved once in New.
	caCertPath         string
	clientCertPath     string
	clientKeyPath      string
	insecureSkipVerify bool
	// tlsErr holds a deferred TLS-configuration failure (bad CA file, missing
	// client key, etc.). New does not return an error, so the failure is
	// surfaced on the first request instead of being silently dropped.
	tlsErr error
}

// ClientOption configures a Client at construction time.
type ClientOption func(*Client)

// WithRestoreTimeout overrides the http.Client timeout used for restore
// calls. The default is 30 minutes. Other endpoints continue to use the
// default 30 second client timeout.
func WithRestoreTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.restoreHTTPTimeout = d }
}

// WithCACert trusts the PEM-encoded CA bundle at path (in addition to the
// system roots) when verifying the server certificate. Use it to reach a
// server that presents a certificate signed by a private CA.
func WithCACert(path string) ClientOption {
	return func(c *Client) { c.caCertPath = path }
}

// WithClientCert presents the PEM-encoded client certificate/key pair for
// mutual TLS. Both paths are required together; supplying one without the
// other is a configuration error surfaced on the first request.
func WithClientCert(certPath, keyPath string) ClientOption {
	return func(c *Client) {
		c.clientCertPath = certPath
		c.clientKeyPath = keyPath
	}
}

// WithInsecureSkipVerify disables server certificate verification. This is
// insecure (vulnerable to man-in-the-middle) and intended only for
// development against self-signed certs; the CLI emits a warning when set.
func WithInsecureSkipVerify(skip bool) ClientOption {
	return func(c *Client) { c.insecureSkipVerify = skip }
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
	// Resolve TLS material once. Only build a custom transport when a TLS
	// option was set, so the default behavior (system-root trust over the
	// shared default transport) is preserved for plain HTTP / public-CA TLS.
	if c.caCertPath != "" || c.clientCertPath != "" || c.clientKeyPath != "" || c.insecureSkipVerify {
		if err := c.applyTLS(); err != nil {
			c.tlsErr = err
		}
	}
	return c
}

// applyTLS builds a *tls.Config from the captured options and installs it on a
// cloned default transport (preserving proxy/keep-alive defaults). It is the
// single place client TLS material is read from disk.
func (c *Client) applyTLS() error {
	if (c.clientCertPath == "") != (c.clientKeyPath == "") {
		return fmt.Errorf("client certificate and key must be provided together (got cert=%q key=%q)", c.clientCertPath, c.clientKeyPath)
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	tlsCfg.InsecureSkipVerify = c.insecureSkipVerify //nolint:gosec // opt-in via --tls-skip-verify; the CLI warns when enabled

	if c.caCertPath != "" {
		caPEM, err := os.ReadFile(c.caCertPath)
		if err != nil {
			return fmt.Errorf("read CA cert %s: %w", c.caCertPath, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(caPEM) {
			return fmt.Errorf("CA cert %s contains no valid PEM certificate", c.caCertPath)
		}
		tlsCfg.RootCAs = pool
	}

	if c.clientCertPath != "" {
		cert, err := tls.LoadX509KeyPair(c.clientCertPath, c.clientKeyPath)
		if err != nil {
			return fmt.Errorf("load client key pair (cert=%s key=%s): %w", c.clientCertPath, c.clientKeyPath, err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return fmt.Errorf("unexpected default HTTP transport type %T", http.DefaultTransport)
	}
	tr := base.Clone()
	tr.TLSClientConfig = tlsCfg
	c.httpClient.Transport = tr
	return nil
}

// WithToken returns a new client with the given token.
func (c *Client) WithToken(token string) *Client {
	return &Client{
		baseURL:            c.baseURL,
		httpClient:         c.httpClient,
		token:              token,
		restoreHTTPTimeout: c.restoreHTTPTimeout,
		caCertPath:         c.caCertPath,
		clientCertPath:     c.clientCertPath,
		clientKeyPath:      c.clientKeyPath,
		insecureSkipVerify: c.insecureSkipVerify,
		tlsErr:             c.tlsErr,
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
	// Surface a deferred TLS-configuration failure (captured in New) before
	// attempting any I/O, so the user gets a precise error instead of an
	// opaque transport failure.
	if c.tlsErr != nil {
		return c.tlsErr
	}

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
		if json.Unmarshal(respBody, &apiErr) == nil &&
			(apiErr.Title != "" || apiErr.Status != 0 || apiErr.Code != "" || apiErr.Detail != "") {
			apiErr.StatusCode = resp.StatusCode
			return &apiErr
		}
		return &APIError{StatusCode: resp.StatusCode, Detail: string(respBody)}
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
