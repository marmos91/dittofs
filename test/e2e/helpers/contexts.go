//go:build e2e

package helpers

import (
	"fmt"
	"strings"
)

// =============================================================================
// Context Management Types
// =============================================================================

// ContextInfo represents context information from dittofsctl context list.
type ContextInfo struct {
	Name      string `json:"name"`
	Current   bool   `json:"current"`
	ServerURL string `json:"server_url"`
	Username  string `json:"username,omitempty"`
	LoggedIn  bool   `json:"logged_in"`
}

// =============================================================================
// Context Management Methods
// =============================================================================

// ListContexts lists all saved server contexts via the CLI.
// Returns contexts with their names, server URLs, and current status.
func (r *CLIRunner) ListContexts() ([]*ContextInfo, error) {
	output, err := r.RunRaw("context", "list", "--output", "json")
	if err != nil {
		return nil, err
	}

	// Handle empty output
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" || trimmed == "null" || trimmed == "[]" {
		return []*ContextInfo{}, nil
	}

	var contexts []*ContextInfo
	if err := ParseJSONResponse(output, &contexts); err != nil {
		return nil, err
	}

	if contexts == nil {
		return []*ContextInfo{}, nil
	}

	return contexts, nil
}

// GetCurrentContext returns the name of the currently active context.
func (r *CLIRunner) GetCurrentContext() (string, error) {
	output, err := r.RunRaw("context", "current", "--output", "json")
	if err != nil {
		return "", err
	}

	var info ContextInfo
	if err := ParseJSONResponse(output, &info); err != nil {
		return "", err
	}

	return info.Name, nil
}

// UseContext switches to the specified context.
func (r *CLIRunner) UseContext(name string) error {
	_, err := r.RunRaw("context", "use", name)
	return err
}

// DeleteContext removes a saved context.
// Uses --force to skip confirmation prompt.
func (r *CLIRunner) DeleteContext(name string) error {
	_, err := r.RunRaw("context", "delete", name, "--force")
	return err
}

// RenameContext renames a context.
func (r *CLIRunner) RenameContext(oldName, newName string) error {
	_, err := r.RunRaw("context", "rename", oldName, newName)
	return err
}

// GetContext retrieves a specific context by name.
func (r *CLIRunner) GetContext(name string) (*ContextInfo, error) {
	contexts, err := r.ListContexts()
	if err != nil {
		return nil, err
	}
	for _, ctx := range contexts {
		if ctx.Name == name {
			return ctx, nil
		}
	}
	return nil, fmt.Errorf("context not found: %s", name)
}
