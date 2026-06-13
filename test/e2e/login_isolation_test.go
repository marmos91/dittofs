//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoginAsAdmin_XDGIsolation verifies that LoginAsAdmin does not read or
// write the developer's real ~/.config/dfsctl/config.json.
//
// Fail-before: with XDG_CONFIG_HOME unset, LoginAsAdmin lets dfsctl login fall
// back to ~/.config/dfsctl/config.json, creating or overwriting real
// credentials on disk.
//
// Pass-after: LoginAsAdmin sets XDG_CONFIG_HOME to a fresh t.TempDir() before
// invoking dfsctl login, so all credential I/O is confined to a temp tree that
// is removed when the test ends and the real ~/.config is never touched.
func TestLoginAsAdmin_XDGIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping login isolation test in short mode")
	}

	// Force the unprotected fallback path: with XDG_CONFIG_HOME empty, a
	// non-isolating LoginAsAdmin would write the real ~/.config/dfsctl/config.json.
	// t.Setenv restores any prior value when the test ends.
	t.Setenv("XDG_CONFIG_HOME", "")
	require.NoError(t, os.Unsetenv("XDG_CONFIG_HOME"))

	// Capture the real home config file so we can assert it is not touched.
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	realCredFile := filepath.Join(home, ".config", "dfsctl", "config.json")

	// Snapshot the real cred file's existence + contents before the test so we
	// can detect both creation and overwrite of a pre-existing file.
	beforeData, beforeErr := os.ReadFile(realCredFile)
	realCredExistedBefore := beforeErr == nil

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	// LoginAsAdmin must return a runner with a non-empty token.
	require.NotEmpty(t, runner.Token(), "LoginAsAdmin must return a non-empty token")

	// The real ~/.config/dfsctl/config.json must not have been created or
	// modified by this test run. A self-isolating LoginAsAdmin points dfsctl at
	// a per-test temp dir, so the real file is never created and, if it already
	// existed, its contents are left byte-for-byte unchanged.
	afterData, afterErr := os.ReadFile(realCredFile)
	if realCredExistedBefore {
		require.NoError(t, afterErr, "real cred file disappeared during test")
		assert.Equal(t, beforeData, afterData,
			"LoginAsAdmin must not modify the real ~/.config/dfsctl/config.json")
	} else {
		assert.True(t, os.IsNotExist(afterErr),
			"LoginAsAdmin must not create the real ~/.config/dfsctl/config.json; stat err: %v", afterErr)
	}
}
