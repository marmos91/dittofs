package credentials

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextIsExpired(t *testing.T) {
	tests := []struct {
		name      string
		expiresAt time.Time
		expected  bool
	}{
		{
			name:      "expired in past",
			expiresAt: time.Now().Add(-1 * time.Hour),
			expected:  true,
		},
		{
			name:      "expires soon (within 60s)",
			expiresAt: time.Now().Add(30 * time.Second),
			expected:  true,
		},
		{
			name:      "not expired",
			expiresAt: time.Now().Add(2 * time.Hour),
			expected:  false,
		},
		{
			name:      "zero time is expired",
			expiresAt: time.Time{},
			expected:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &Context{ExpiresAt: tt.expiresAt}
			assert.Equal(t, tt.expected, ctx.IsExpired())
		})
	}
}

func TestContextHasRefreshToken(t *testing.T) {
	ctx := &Context{}
	assert.False(t, ctx.HasRefreshToken())

	ctx.RefreshToken = "token"
	assert.True(t, ctx.HasRefreshToken())
}

func TestStoreOperations(t *testing.T) {
	// Create temp directory for test
	tmpDir, err := os.MkdirTemp("", "dfsctl-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Set XDG_CONFIG_HOME to temp directory
	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)
	defer func() { _ = os.Setenv("XDG_CONFIG_HOME", oldXDG) }()

	// Create store
	store, err := NewStore()
	require.NoError(t, err)
	assert.NotNil(t, store)

	// Verify config file location
	expectedPath := filepath.Join(tmpDir, DefaultConfigDir, ConfigFileName)
	assert.Equal(t, expectedPath, store.ConfigPath())

	// Test empty state
	_, err = store.GetCurrentContext()
	assert.ErrorIs(t, err, ErrNoCurrentContext)
	assert.Empty(t, store.ListContexts())

	// Add a context
	ctx1 := &Context{
		ServerURL:    "http://localhost:8080",
		Username:     "admin",
		AccessToken:  "token1",
		RefreshToken: "refresh1",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	}
	err = store.SetContext("default", ctx1)
	require.NoError(t, err)

	// Use the context
	err = store.UseContext("default")
	require.NoError(t, err)

	// Get current context
	current, err := store.GetCurrentContext()
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:8080", current.ServerURL)
	assert.Equal(t, "admin", current.Username)

	// Add another context
	ctx2 := &Context{
		ServerURL: "http://production:8080",
		Username:  "prod-admin",
	}
	err = store.SetContext("production", ctx2)
	require.NoError(t, err)

	// List contexts
	contexts := store.ListContexts()
	assert.Len(t, contexts, 2)
	assert.Contains(t, contexts, "default")
	assert.Contains(t, contexts, "production")

	// Switch context
	err = store.UseContext("production")
	require.NoError(t, err)
	assert.Equal(t, "production", store.GetCurrentContextName())

	// Rename context
	err = store.RenameContext("production", "prod")
	require.NoError(t, err)
	assert.Equal(t, "prod", store.GetCurrentContextName())

	// Delete context
	err = store.DeleteContext("prod")
	require.NoError(t, err)
	assert.Empty(t, store.GetCurrentContextName())

	// Try to get non-existent context
	_, err = store.GetContext("nonexistent")
	assert.ErrorIs(t, err, ErrContextNotFound)

	// Try to use non-existent context
	err = store.UseContext("nonexistent")
	assert.ErrorIs(t, err, ErrContextNotFound)
}

func TestStoreUpdateTokens(t *testing.T) {
	// Create temp directory for test
	tmpDir, err := os.MkdirTemp("", "dfsctl-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)
	defer func() { _ = os.Setenv("XDG_CONFIG_HOME", oldXDG) }()

	store, err := NewStore()
	require.NoError(t, err)

	// Create and use a context
	ctx := &Context{
		ServerURL:   "http://localhost:8080",
		Username:    "admin",
		AccessToken: "old-token",
	}
	err = store.SetContext("default", ctx)
	require.NoError(t, err)
	err = store.UseContext("default")
	require.NoError(t, err)

	// Update tokens
	newExpiry := time.Now().Add(2 * time.Hour)
	err = store.UpdateTokens("new-access", "new-refresh", newExpiry)
	require.NoError(t, err)

	// Verify tokens updated
	current, err := store.GetCurrentContext()
	require.NoError(t, err)
	assert.Equal(t, "new-access", current.AccessToken)
	assert.Equal(t, "new-refresh", current.RefreshToken)
	assert.WithinDuration(t, newExpiry, current.ExpiresAt, time.Second)
}

func TestStoreClearCurrentContext(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dfsctl-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)
	defer func() { _ = os.Setenv("XDG_CONFIG_HOME", oldXDG) }()

	store, err := NewStore()
	require.NoError(t, err)

	// Create and use a context with tokens
	ctx := &Context{
		ServerURL:    "http://localhost:8080",
		Username:     "admin",
		AccessToken:  "token",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	}
	err = store.SetContext("default", ctx)
	require.NoError(t, err)
	err = store.UseContext("default")
	require.NoError(t, err)

	// Clear context
	err = store.ClearCurrentContext()
	require.NoError(t, err)

	// Verify tokens cleared but server/user remain
	current, err := store.GetCurrentContext()
	require.NoError(t, err)
	assert.Empty(t, current.AccessToken)
	assert.Empty(t, current.RefreshToken)
	assert.True(t, current.ExpiresAt.IsZero())
	assert.Equal(t, "http://localhost:8080", current.ServerURL)
	assert.Equal(t, "admin", current.Username)
}

func TestStorePreferences(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dfsctl-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	_ = os.Setenv("XDG_CONFIG_HOME", tmpDir)
	defer func() { _ = os.Setenv("XDG_CONFIG_HOME", oldXDG) }()

	store, err := NewStore()
	require.NoError(t, err)

	// Get default preferences
	prefs := store.GetPreferences()
	assert.Empty(t, prefs.DefaultOutput)
	assert.Empty(t, prefs.Color)

	// Set preferences
	newPrefs := Preferences{
		DefaultOutput: "json",
		Color:         "auto",
		Editor:        "vim",
	}
	err = store.SetPreferences(newPrefs)
	require.NoError(t, err)

	// Verify preferences persisted
	prefs = store.GetPreferences()
	assert.Equal(t, "json", prefs.DefaultOutput)
	assert.Equal(t, "auto", prefs.Color)
	assert.Equal(t, "vim", prefs.Editor)
}
