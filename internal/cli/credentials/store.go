// Package credentials provides credential storage and context management for dittofsctl.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// DefaultConfigDir is the default directory for dittofsctl configuration.
	DefaultConfigDir = "dittofsctl"
	// ConfigFileName is the name of the configuration file.
	ConfigFileName = "config.json"
	// FilePermissions for config files (read/write for owner only).
	FilePermissions = 0600
	// DirPermissions for config directories.
	DirPermissions = 0700
)

var (
	// ErrNoCurrentContext indicates no context is currently set.
	ErrNoCurrentContext = errors.New("no current context set")
	// ErrContextNotFound indicates the requested context doesn't exist.
	ErrContextNotFound = errors.New("context not found")
	// ErrNotLoggedIn indicates no valid credentials exist.
	ErrNotLoggedIn = errors.New("not logged in - run 'dittofsctl login' first")
)

// Context represents a connection context to a DittoFS server.
type Context struct {
	ServerURL    string    `json:"server_url"`
	Username     string    `json:"username,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// IsExpired returns true if the access token has expired.
func (c *Context) IsExpired() bool {
	if c.ExpiresAt.IsZero() {
		return true
	}
	// Consider expired if within 60 seconds of expiration
	return time.Now().Add(60 * time.Second).After(c.ExpiresAt)
}

// HasRefreshToken returns true if a refresh token is available.
func (c *Context) HasRefreshToken() bool {
	return c.RefreshToken != ""
}

// Preferences represents user preferences.
type Preferences struct {
	DefaultOutput string `json:"default_output,omitempty"` // table, json, yaml
	Color         string `json:"color,omitempty"`          // auto, always, never
	Editor        string `json:"editor,omitempty"`
}

// Config represents the complete dittofsctl configuration.
type Config struct {
	CurrentContext string              `json:"current_context"`
	Contexts       map[string]*Context `json:"contexts"`
	Preferences    Preferences         `json:"preferences,omitempty"`
}

// Store manages credential storage and retrieval.
type Store struct {
	configPath string
	config     *Config
}

// NewStore creates a new credential store.
func NewStore() (*Store, error) {
	configPath, err := getConfigPath()
	if err != nil {
		return nil, err
	}

	store := &Store{
		configPath: configPath,
	}

	// Load existing config or create new
	if err := store.load(); err != nil {
		// If file doesn't exist, create empty config
		if os.IsNotExist(err) {
			store.config = &Config{
				Contexts: make(map[string]*Context),
			}
		} else {
			return nil, err
		}
	}

	return store, nil
}

// getConfigPath returns the path to the config file.
func getConfigPath() (string, error) {
	// Use XDG_CONFIG_HOME if set, otherwise ~/.config
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}

	return filepath.Join(configHome, DefaultConfigDir, ConfigFileName), nil
}

// load reads the config from disk.
func (s *Store) load() error {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return err
	}

	s.config = &Config{}
	return json.Unmarshal(data, s.config)
}

// save writes the config to disk.
func (s *Store) save() error {
	// Ensure directory exists
	dir := filepath.Dir(s.configPath)
	if err := os.MkdirAll(dir, DirPermissions); err != nil {
		return fmt.Errorf("cannot create config directory: %w", err)
	}

	data, err := json.MarshalIndent(s.config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(s.configPath, data, FilePermissions)
}

// GetCurrentContext returns the current context.
func (s *Store) GetCurrentContext() (*Context, error) {
	if s.config.CurrentContext == "" {
		return nil, ErrNoCurrentContext
	}

	ctx, ok := s.config.Contexts[s.config.CurrentContext]
	if !ok {
		return nil, ErrContextNotFound
	}

	return ctx, nil
}

// GetCurrentContextName returns the name of the current context.
func (s *Store) GetCurrentContextName() string {
	return s.config.CurrentContext
}

// GetContext returns a specific context by name.
func (s *Store) GetContext(name string) (*Context, error) {
	ctx, ok := s.config.Contexts[name]
	if !ok {
		return nil, ErrContextNotFound
	}
	return ctx, nil
}

// ListContexts returns all context names.
func (s *Store) ListContexts() []string {
	names := make([]string, 0, len(s.config.Contexts))
	for name := range s.config.Contexts {
		names = append(names, name)
	}
	return names
}

// SetContext creates or updates a context.
func (s *Store) SetContext(name string, ctx *Context) error {
	if s.config.Contexts == nil {
		s.config.Contexts = make(map[string]*Context)
	}
	s.config.Contexts[name] = ctx
	return s.save()
}

// UseContext switches to a different context.
func (s *Store) UseContext(name string) error {
	if _, ok := s.config.Contexts[name]; !ok {
		return ErrContextNotFound
	}
	s.config.CurrentContext = name
	return s.save()
}

// RenameContext renames a context.
func (s *Store) RenameContext(oldName, newName string) error {
	ctx, ok := s.config.Contexts[oldName]
	if !ok {
		return ErrContextNotFound
	}

	delete(s.config.Contexts, oldName)
	s.config.Contexts[newName] = ctx

	if s.config.CurrentContext == oldName {
		s.config.CurrentContext = newName
	}

	return s.save()
}

// DeleteContext removes a context.
func (s *Store) DeleteContext(name string) error {
	if _, ok := s.config.Contexts[name]; !ok {
		return ErrContextNotFound
	}

	delete(s.config.Contexts, name)

	if s.config.CurrentContext == name {
		s.config.CurrentContext = ""
	}

	return s.save()
}

// UpdateTokens updates the tokens for the current context.
func (s *Store) UpdateTokens(accessToken, refreshToken string, expiresAt time.Time) error {
	ctx, err := s.GetCurrentContext()
	if err != nil {
		return err
	}

	ctx.AccessToken = accessToken
	ctx.RefreshToken = refreshToken
	ctx.ExpiresAt = expiresAt

	return s.save()
}

// ClearCurrentContext clears credentials from the current context (logout).
func (s *Store) ClearCurrentContext() error {
	ctx, err := s.GetCurrentContext()
	if err != nil {
		return err
	}

	ctx.AccessToken = ""
	ctx.RefreshToken = ""
	ctx.ExpiresAt = time.Time{}

	return s.save()
}

// GetPreferences returns the user preferences.
func (s *Store) GetPreferences() Preferences {
	return s.config.Preferences
}

// SetPreferences updates the user preferences.
func (s *Store) SetPreferences(prefs Preferences) error {
	s.config.Preferences = prefs
	return s.save()
}

// ConfigPath returns the path to the config file.
func (s *Store) ConfigPath() string {
	return s.configPath
}

// GenerateContextName generates a unique context name from server URL.
func GenerateContextName(serverURL string) string {
	// Simple approach: use "default" for first context, then derive from URL
	return "default"
}
