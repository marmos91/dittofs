//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContextManagement tests multi-context server management via CLI.
// Requirements: CTX-01 through CTX-05.
//
// IMPORTANT: These tests use XDG_CONFIG_HOME to isolate credential storage.
// This prevents tests from modifying real user credentials.
// Tests run sequentially (no t.Parallel) because they share credential file state.
//
// Note: The CLI's login command updates the current context. To test multi-context
// scenarios, we use helper functions that directly manipulate the credentials file
// for setup, then verify CLI operations work correctly on the resulting state.
func TestContextManagement(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping context management tests in short mode")
	}

	// CTX-01: List server contexts
	t.Run("CTX-01 list server contexts", func(t *testing.T) {
		// Setup isolated credentials for this test
		setupIsolatedCredentials(t)

		// Start server
		sp := helpers.StartServerProcess(t, "")
		t.Cleanup(sp.ForceKill)
		serverURL := sp.APIURL()

		// Create a CLI runner
		cli := helpers.NewCLIRunner(serverURL, "")

		// First login to create a context
		_, err := cli.Login(serverURL, "admin", helpers.GetAdminPassword())
		require.NoError(t, err, "Should login successfully")

		// List contexts
		contexts, err := cli.ListContexts()
		require.NoError(t, err, "Should list contexts")
		require.NotEmpty(t, contexts, "Should have at least one context after login")

		// Verify a context exists for our server
		var found bool
		for _, ctx := range contexts {
			if ctx.ServerURL == serverURL {
				found = true
				assert.True(t, ctx.LoggedIn, "Context should be logged in")
				assert.Equal(t, "admin", ctx.Username, "Username should be admin")
			}
		}
		assert.True(t, found, "Should find context for our server")
	})

	// CTX-02: Add new context via login
	// This test verifies that context operations work correctly with multiple contexts.
	t.Run("CTX-02 add new context via login", func(t *testing.T) {
		// Setup isolated credentials
		tempConfig := setupIsolatedCredentials(t)

		// Start two servers
		sp1 := helpers.StartServerProcess(t, "")
		t.Cleanup(sp1.ForceKill)
		server1URL := sp1.APIURL()

		sp2 := helpers.StartServerProcess(t, "")
		t.Cleanup(sp2.ForceKill)
		server2URL := sp2.APIURL()

		// Create a CLI runner
		cli := helpers.NewCLIRunner("", "")

		// Login to server 1 to get valid token
		_, err := cli.Login(server1URL, "admin", helpers.GetAdminPassword())
		require.NoError(t, err, "Should login to first server")
		token1, err := helpers.ExtractTokenFromCredentialsFile(server1URL)
		require.NoError(t, err)

		// Login to server 2 to get valid token
		_, err = cli.Login(server2URL, "admin", helpers.GetAdminPassword())
		require.NoError(t, err, "Should login to second server")
		token2, err := helpers.ExtractTokenFromCredentialsFile(server2URL)
		require.NoError(t, err)

		// Manually create credential file with both contexts
		err = createMultiContextCredFile(tempConfig, []contextSetup{
			{Name: "server1", ServerURL: server1URL, Token: token1},
			{Name: "server2", ServerURL: server2URL, Token: token2},
		}, "server2")
		require.NoError(t, err)

		// List contexts - should have both
		contexts, err := cli.ListContexts()
		require.NoError(t, err, "Should list contexts")
		require.GreaterOrEqual(t, len(contexts), 2, "Should have at least 2 contexts")

		// Verify both contexts exist with correct server URLs
		var found1, found2 bool
		for _, ctx := range contexts {
			if ctx.Name == "server1" {
				assert.Equal(t, server1URL, ctx.ServerURL, "server1 context should point to first server")
				found1 = true
			}
			if ctx.Name == "server2" {
				assert.Equal(t, server2URL, ctx.ServerURL, "server2 context should point to second server")
				found2 = true
			}
		}
		assert.True(t, found1, "Should have context for first server")
		assert.True(t, found2, "Should have context for second server")
	})

	// CTX-03: Remove server context
	t.Run("CTX-03 remove server context", func(t *testing.T) {
		// Setup isolated credentials
		tempConfig := setupIsolatedCredentials(t)

		// Start two servers
		spA := helpers.StartServerProcess(t, "")
		t.Cleanup(spA.ForceKill)
		spB := helpers.StartServerProcess(t, "")
		t.Cleanup(spB.ForceKill)

		serverA := spA.APIURL()
		serverB := spB.APIURL()

		// Create a CLI runner
		cli := helpers.NewCLIRunner("", "")

		// Get tokens
		_, err := cli.Login(serverA, "admin", helpers.GetAdminPassword())
		require.NoError(t, err)
		tokenA, _ := helpers.ExtractTokenFromCredentialsFile(serverA)

		_, err = cli.Login(serverB, "admin", helpers.GetAdminPassword())
		require.NoError(t, err)
		tokenB, _ := helpers.ExtractTokenFromCredentialsFile(serverB)

		// Create credential file with both contexts
		err = createMultiContextCredFile(tempConfig, []contextSetup{
			{Name: "serverA", ServerURL: serverA, Token: tokenA},
			{Name: "serverB", ServerURL: serverB, Token: tokenB},
		}, "serverB")
		require.NoError(t, err)

		// Verify both contexts exist
		contexts, err := cli.ListContexts()
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(contexts), 2, "Should have at least 2 contexts")

		// Delete context A
		err = cli.DeleteContext("serverA")
		require.NoError(t, err, "Should delete context A")

		// Verify context A is gone
		contexts, err = cli.ListContexts()
		require.NoError(t, err)

		var foundA, foundB bool
		for _, ctx := range contexts {
			if ctx.Name == "serverA" {
				foundA = true
			}
			if ctx.Name == "serverB" {
				foundB = true
			}
		}
		assert.False(t, foundA, "Context A should be deleted")
		assert.True(t, foundB, "Context B should still exist")
	})

	// CTX-04: Switch active context
	t.Run("CTX-04 switch active context", func(t *testing.T) {
		// Setup isolated credentials
		tempConfig := setupIsolatedCredentials(t)

		// Start two servers
		sp1 := helpers.StartServerProcess(t, "")
		t.Cleanup(sp1.ForceKill)
		sp2 := helpers.StartServerProcess(t, "")
		t.Cleanup(sp2.ForceKill)

		server1 := sp1.APIURL()
		server2 := sp2.APIURL()

		// Create a CLI runner
		cli := helpers.NewCLIRunner("", "")

		// Get tokens
		_, err := cli.Login(server1, "admin", helpers.GetAdminPassword())
		require.NoError(t, err)
		token1, _ := helpers.ExtractTokenFromCredentialsFile(server1)

		_, err = cli.Login(server2, "admin", helpers.GetAdminPassword())
		require.NoError(t, err)
		token2, _ := helpers.ExtractTokenFromCredentialsFile(server2)

		// Create credential file with both contexts
		err = createMultiContextCredFile(tempConfig, []contextSetup{
			{Name: "ctx1", ServerURL: server1, Token: token1},
			{Name: "ctx2", ServerURL: server2, Token: token2},
		}, "ctx1")
		require.NoError(t, err)

		// Switch to context 1
		err = cli.UseContext("ctx1")
		require.NoError(t, err, "Should switch to context 1")

		// Verify current context is context 1
		current, err := cli.GetCurrentContext()
		require.NoError(t, err)
		assert.Equal(t, "ctx1", current, "Current context should be ctx1")

		// Switch to context 2
		err = cli.UseContext("ctx2")
		require.NoError(t, err, "Should switch to context 2")

		// Verify current context is now context 2
		current, err = cli.GetCurrentContext()
		require.NoError(t, err)
		assert.Equal(t, "ctx2", current, "Current context should be ctx2")
	})

	// CTX-05: Credential isolation between contexts
	t.Run("CTX-05 credential isolation", func(t *testing.T) {
		// Setup isolated credentials
		tempConfig := setupIsolatedCredentials(t)

		// Start two servers
		sp1 := helpers.StartServerProcess(t, "")
		t.Cleanup(sp1.ForceKill)
		sp2 := helpers.StartServerProcess(t, "")
		t.Cleanup(sp2.ForceKill)

		server1 := sp1.APIURL()
		server2 := sp2.APIURL()

		// Create a CLI runner
		cli := helpers.NewCLIRunner("", "")

		// Get tokens for both servers
		_, err := cli.Login(server1, "admin", helpers.GetAdminPassword())
		require.NoError(t, err)
		token1, err := helpers.ExtractTokenFromCredentialsFile(server1)
		require.NoError(t, err)

		_, err = cli.Login(server2, "admin", helpers.GetAdminPassword())
		require.NoError(t, err)
		token2, err := helpers.ExtractTokenFromCredentialsFile(server2)
		require.NoError(t, err)

		// Create credential file with both contexts
		err = createMultiContextCredFile(tempConfig, []contextSetup{
			{Name: "server1", ServerURL: server1, Token: token1},
			{Name: "server2", ServerURL: server2, Token: token2},
		}, "server1")
		require.NoError(t, err)

		// Create a user on server1 only
		cli1 := helpers.NewCLIRunner(server1, token1)
		user1Name := helpers.UniqueTestName("ctx_user")
		user1Pass := "TestPassword123!"
		_, err = cli1.CreateUser(user1Name, user1Pass)
		require.NoError(t, err, "Should create user on server1")
		t.Cleanup(func() { _ = cli1.DeleteUser(user1Name) })

		// Verify user exists on server1
		_, err = cli1.GetUser(user1Name)
		require.NoError(t, err, "User should exist on server1")

		// Verify user does NOT exist on server2 (credential isolation)
		cli2 := helpers.NewCLIRunner(server2, token2)
		_, err = cli2.GetUser(user1Name)
		require.Error(t, err, "User should NOT exist on server2")

		// Verify context info shows correct server URLs
		ctx1Info, err := cli.GetContext("server1")
		require.NoError(t, err)
		assert.Equal(t, server1, ctx1Info.ServerURL, "Context server1 should point to server1")

		ctx2Info, err := cli.GetContext("server2")
		require.NoError(t, err)
		assert.Equal(t, server2, ctx2Info.ServerURL, "Context server2 should point to server2")

		// Verify that credentials are properly associated with each context
		assert.Equal(t, "admin", ctx1Info.Username, "Context server1 should have admin username")
		assert.Equal(t, "admin", ctx2Info.Username, "Context server2 should have admin username")

		// Final check: switch contexts and verify isolation
		err = cli.UseContext("server1")
		require.NoError(t, err)
		current, err := cli.GetCurrentContext()
		require.NoError(t, err)
		assert.Equal(t, "server1", current, "Should be on server1 context")

		err = cli.UseContext("server2")
		require.NoError(t, err)
		current, err = cli.GetCurrentContext()
		require.NoError(t, err)
		assert.Equal(t, "server2", current, "Should be on server2 context")
	})

	// Additional test: Rename context
	t.Run("rename context", func(t *testing.T) {
		// Setup isolated credentials for this test
		setupIsolatedCredentials(t)

		// Start a server
		sp := helpers.StartServerProcess(t, "")
		t.Cleanup(sp.ForceKill)
		serverURL := sp.APIURL()

		// Create a CLI runner
		cli := helpers.NewCLIRunner(serverURL, "")

		// Login to create context
		_, err := cli.Login(serverURL, "admin", helpers.GetAdminPassword())
		require.NoError(t, err)

		// Get context name (should be "default")
		contexts, err := cli.ListContexts()
		require.NoError(t, err)
		require.NotEmpty(t, contexts, "Should have at least one context")

		origName := contexts[0].Name
		require.NotEmpty(t, origName, "Should find context name")

		// Rename context
		newName := helpers.UniqueTestName("renamed_ctx")
		err = cli.RenameContext(origName, newName)
		require.NoError(t, err, "Should rename context")

		// Verify old name doesn't exist and new name does
		_, err = cli.GetContext(origName)
		assert.Error(t, err, "Old context name should not exist")

		renamedCtx, err := cli.GetContext(newName)
		require.NoError(t, err, "New context name should exist")
		assert.Equal(t, serverURL, renamedCtx.ServerURL, "Renamed context should have same server URL")
	})
}

// setupIsolatedCredentials sets XDG_CONFIG_HOME to a temp directory
// to prevent tests from modifying real user credentials.
// Returns the temp config path for direct file manipulation if needed.
func setupIsolatedCredentials(t *testing.T) string {
	t.Helper()
	tempConfig := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tempConfig)
	return tempConfig
}

// contextSetup describes a context to be created in the credentials file.
type contextSetup struct {
	Name      string
	ServerURL string
	Token     string
}

// createMultiContextCredFile directly creates a credentials file with multiple contexts.
// This is needed because the CLI login command always updates the current context,
// making it difficult to create multiple contexts via CLI alone.
func createMultiContextCredFile(configHome string, contexts []contextSetup, currentContext string) error {
	type credContext struct {
		ServerURL    string    `json:"server_url"`
		Username     string    `json:"username"`
		AccessToken  string    `json:"access_token"`
		RefreshToken string    `json:"refresh_token,omitempty"`
		ExpiresAt    time.Time `json:"expires_at"`
	}

	type credConfig struct {
		CurrentContext string                  `json:"current_context"`
		Contexts       map[string]*credContext `json:"contexts"`
	}

	config := credConfig{
		CurrentContext: currentContext,
		Contexts:       make(map[string]*credContext),
	}

	for _, ctx := range contexts {
		config.Contexts[ctx.Name] = &credContext{
			ServerURL:   ctx.ServerURL,
			Username:    "admin",
			AccessToken: ctx.Token,
			ExpiresAt:   time.Now().Add(24 * time.Hour),
		}
	}

	dir := filepath.Join(configHome, "dittofsctl")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, "config.json"), data, 0600)
}
