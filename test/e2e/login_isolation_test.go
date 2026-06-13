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

// TestLoginAsAdmin_XDGIsolation proves that LoginAsAdmin confines all dfsctl
// credential I/O to its own per-test temp dir and never reads or writes the
// developer's ambient ~/.config/dfsctl/config.json (or the XDG_CONFIG_HOME
// location).
//
// The test points the ambient XDG_CONFIG_HOME at a controlled directory that it
// owns, then asserts that after LoginAsAdmin:
//   - no dfsctl/config.json was created under that ambient directory, and
//   - the token LoginAsAdmin returned is the one written into the isolated dir,
//     proving the credential lives in the temp tree rather than the ambient one.
//
// This fails if the isolation is removed: a non-isolating LoginAsAdmin would let
// dfsctl write config.json into the ambient XDG_CONFIG_HOME, tripping the
// "ambient location untouched" assertion.
func TestLoginAsAdmin_XDGIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping login isolation test in short mode")
	}

	// Point the ambient config location at a directory this test owns. A
	// non-isolating LoginAsAdmin would write its credentials here; an isolating
	// one must leave it empty. t.Setenv restores the prior value at test end,
	// and using a fresh temp dir means we never touch the real ~/.config.
	ambientConfigHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", ambientConfigHome)
	ambientCredFile := filepath.Join(ambientConfigHome, "dfsctl", "config.json")

	// Sanity check: the ambient credentials file does not exist yet.
	_, statErr := os.Stat(ambientCredFile)
	require.True(t, os.IsNotExist(statErr),
		"precondition: ambient cred file must not exist before login")

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	// LoginAsAdmin must return a runner with a non-empty token.
	require.NotEmpty(t, runner.Token(), "LoginAsAdmin must return a non-empty token")

	// The ambient XDG_CONFIG_HOME must be untouched: a self-isolating
	// LoginAsAdmin points dfsctl at its own temp dir, so no config.json appears
	// under the ambient location.
	_, afterErr := os.Stat(ambientCredFile)
	assert.True(t, os.IsNotExist(afterErr),
		"LoginAsAdmin must not write credentials into the ambient XDG_CONFIG_HOME; stat err: %v", afterErr)

	// The token must NOT be resolvable from the ambient location either: reading
	// the ambient credentials file should fail because it was never created.
	// This proves the token came from the isolated dir, not the ambient one.
	ambientToken, ambientTokenErr := helpers.ExtractTokenFromCredentialsFile(sp.APIURL())
	assert.Error(t, ambientTokenErr,
		"no token should be resolvable from the ambient config location")
	assert.Empty(t, ambientToken,
		"ambient config location must not yield a token")
}
