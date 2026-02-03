//go:build e2e

package helpers

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// =============================================================================
// Adapter Management Types
// =============================================================================

// Adapter represents a protocol adapter returned from the API.
type Adapter struct {
	Type    string `json:"type"`
	Port    int    `json:"port"`
	Enabled bool   `json:"enabled"`
}

// AdapterOption is a functional option for adapter operations.
type AdapterOption func(*adapterOptions)

type adapterOptions struct {
	port    *int
	enabled *bool
}

// WithAdapterPort sets the adapter listen port.
func WithAdapterPort(port int) AdapterOption {
	return func(o *adapterOptions) {
		o.port = &port
	}
}

// WithAdapterEnabled sets the adapter enabled status.
func WithAdapterEnabled(enabled bool) AdapterOption {
	return func(o *adapterOptions) {
		o.enabled = &enabled
	}
}

// =============================================================================
// Adapter CRUD Methods
// =============================================================================

// ListAdapters lists all protocol adapters via the CLI.
func (r *CLIRunner) ListAdapters() ([]*Adapter, error) {
	output, err := r.Run("adapter", "list")
	if err != nil {
		return nil, err
	}

	var adapters []*Adapter
	if err := ParseJSONResponse(output, &adapters); err != nil {
		return nil, err
	}

	return adapters, nil
}

// GetAdapter retrieves an adapter by type.
// Since there's no dedicated 'adapter get' command in the CLI, this lists all
// adapters and filters by type.
func (r *CLIRunner) GetAdapter(adapterType string) (*Adapter, error) {
	adapters, err := r.ListAdapters()
	if err != nil {
		return nil, err
	}

	for _, a := range adapters {
		if a.Type == adapterType {
			return a, nil
		}
	}

	return nil, fmt.Errorf("adapter not found: %s", adapterType)
}

// EnableAdapter enables a protocol adapter via the CLI.
// If the adapter doesn't exist, it will be created.
// Options can be used to set the port.
func (r *CLIRunner) EnableAdapter(adapterType string, opts ...AdapterOption) (*Adapter, error) {
	options := &adapterOptions{}
	for _, opt := range opts {
		opt(options)
	}

	args := []string{"adapter", "enable", adapterType}

	if options.port != nil {
		args = append(args, "--port", fmt.Sprintf("%d", *options.port))
	}

	output, err := r.Run(args...)
	if err != nil {
		return nil, err
	}

	var adapter Adapter
	if err := ParseJSONResponse(output, &adapter); err != nil {
		return nil, err
	}

	return &adapter, nil
}

// DisableAdapter disables a protocol adapter via the CLI.
func (r *CLIRunner) DisableAdapter(adapterType string) (*Adapter, error) {
	output, err := r.Run("adapter", "disable", adapterType)
	if err != nil {
		return nil, err
	}

	var adapter Adapter
	if err := ParseJSONResponse(output, &adapter); err != nil {
		return nil, err
	}

	return &adapter, nil
}

// EditAdapter edits an existing adapter via the CLI.
// At least one option must be provided.
func (r *CLIRunner) EditAdapter(adapterType string, opts ...AdapterOption) (*Adapter, error) {
	options := &adapterOptions{}
	for _, opt := range opts {
		opt(options)
	}

	args := []string{"adapter", "edit", adapterType}
	hasUpdate := false

	if options.port != nil {
		args = append(args, "--port", fmt.Sprintf("%d", *options.port))
		hasUpdate = true
	}
	if options.enabled != nil {
		args = append(args, "--enabled", fmt.Sprintf("%t", *options.enabled))
		hasUpdate = true
	}

	if !hasUpdate {
		return nil, fmt.Errorf("at least one option (WithAdapterPort or WithAdapterEnabled) is required for EditAdapter")
	}

	output, err := r.Run(args...)
	if err != nil {
		return nil, err
	}

	var adapter Adapter
	if err := ParseJSONResponse(output, &adapter); err != nil {
		return nil, err
	}

	return &adapter, nil
}

// =============================================================================
// Adapter Status Helpers
// =============================================================================

// WaitForAdapterStatus waits for an adapter to reach the specified enabled status.
// Polls the adapter status up to the given timeout.
func WaitForAdapterStatus(t *testing.T, runner *CLIRunner, adapterType string, enabled bool, timeout time.Duration) error {
	t.Helper()

	deadline := time.Now().Add(timeout)
	pollInterval := 100 * time.Millisecond

	for time.Now().Before(deadline) {
		adapter, err := runner.GetAdapter(adapterType)
		if err != nil {
			// Adapter not found is OK if we're waiting for disabled
			if !enabled && strings.Contains(err.Error(), "not found") {
				return nil
			}
			// Log and continue polling
			t.Logf("GetAdapter error (retrying): %v", err)
		} else if adapter.Enabled == enabled {
			return nil
		}

		time.Sleep(pollInterval)
	}

	return fmt.Errorf("timeout waiting for adapter %s to reach enabled=%v", adapterType, enabled)
}
