//go:build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// NFSv4.2 extended attributes (RFC 8276)
//
// These tests mount with vers=4.2 and exercise the four xattr operations via
// the standard Linux userspace tools (setfattr/getfattr). They require a Linux
// kernel client with NFSv4.2 xattr support (>= 5.9); MountNFSWithVersion skips
// on macOS.
// =============================================================================

// setfattr sets a user.* xattr on path. Fails the test on error.
func setfattr(t *testing.T, path, name, value string) {
	t.Helper()
	out, err := exec.Command("setfattr", "-n", name, "-v", value, path).CombinedOutput()
	require.NoErrorf(t, err, "setfattr %s=%s on %s: %s", name, value, path, string(out))
}

// getfattr returns the value of a single user.* xattr on path (with -n NAME),
// using the raw-value encoding so the result is the literal string set.
func getfattr(t *testing.T, path, name string) (string, error) {
	t.Helper()
	// --only-values prints just the value; -e text keeps it as the literal string.
	// Trim a single trailing newline defensively (the test values carry none, so
	// this only guards against getfattr builds that append one).
	out, err := exec.Command("getfattr", "--only-values", "-e", "text", "-n", name, path).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

// getfattrDump returns the full `getfattr -d` listing (all names and values).
func getfattrDump(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("getfattr", "-d", path).Output()
	require.NoError(t, err, "getfattr -d on %s", path)
	return string(out)
}

// TestNFSv42XattrRoundTrip validates set/get/list/remove of a user.* xattr over
// a vers=4.2 mount (XATTR-01..04).
func TestNFSv42XattrRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.2 xattr tests in short mode")
	}

	_, _, nfsPort := setupNFSv4TestServer(t)

	mount := framework.MountNFSWithVersion(t, nfsPort, "4.2")
	t.Cleanup(mount.Cleanup)

	filePath := mount.FilePath("xattr_round_trip.txt")
	framework.WriteFile(t, filePath, []byte("payload"))

	t.Run("XATTR-01 set then get", func(t *testing.T) {
		setfattr(t, filePath, "user.comment", "hello world")
		got, err := getfattr(t, filePath, "user.comment")
		require.NoError(t, err, "getfattr should find the xattr")
		assert.Equal(t, "hello world", got)
	})

	t.Run("XATTR-02 list includes the name", func(t *testing.T) {
		setfattr(t, filePath, "user.second", "v2")
		dump := getfattrDump(t, filePath)
		assert.Contains(t, dump, "user.comment")
		assert.Contains(t, dump, "user.second")
	})

	t.Run("XATTR-03 replace value", func(t *testing.T) {
		setfattr(t, filePath, "user.comment", "updated")
		got, err := getfattr(t, filePath, "user.comment")
		require.NoError(t, err)
		assert.Equal(t, "updated", got)
	})

	t.Run("XATTR-04 remove then get fails", func(t *testing.T) {
		out, err := exec.Command("setfattr", "-x", "user.comment", filePath).CombinedOutput()
		require.NoErrorf(t, err, "setfattr -x: %s", string(out))
		_, err = getfattr(t, filePath, "user.comment")
		assert.Error(t, err, "removed xattr should no longer be readable")
	})

	t.Run("XATTR-05 non-user namespace rejected", func(t *testing.T) {
		// trusted.* is not exposed; the server returns NFS4ERR_NOXATTR, which the
		// client surfaces as an error from setfattr.
		out, err := exec.Command("setfattr", "-n", "trusted.x", "-v", "y", filePath).CombinedOutput()
		assert.Errorf(t, err, "trusted.* namespace should be rejected: %s", string(out))
	})
}

// TestNFSv42XattrPersistAcrossRestart proves an xattr written over vers=4.2
// survives a full server restart when backed by a persistent metadata store
// (badger) and an on-disk control-plane (sqlite) (XATTR-06).
func TestNFSv42XattrPersistAcrossRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFSv4.2 xattr persistence test in short mode")
	}

	// Stable state directory shared across both server lifetimes. t.TempDir is
	// removed only at test end, so it survives the in-test restart.
	stateDir := t.TempDir()
	apiPort := helpers.FindFreePort(t)
	nfsPort := helpers.FindFreePort(t)

	configFile := filepath.Join(stateDir, "config.yaml")
	writeXattrPersistConfig(t, configFile, apiPort, stateDir)

	t.Setenv("DITTOFS_ADMIN_INITIAL_PASSWORD", helpers.GetAdminPassword())

	badgerPath := filepath.Join(stateDir, "badger")
	blocksPath := filepath.Join(stateDir, "blocks")
	const metaName, localName = "xpmeta", "xplocal"
	const shareName = "/export"

	// ---- First lifetime: create stores/share/adapter, set the xattr ----
	sp1 := helpers.StartServerProcessWithConfig(t, configFile)
	// Guard cleanup so a t.Skip inside MountNFSWithVersion (unsupported platform)
	// doesn't leak sp1, while the intentional pre-restart kill below isn't
	// double-signalled to a possibly-reused PID.
	sp1Killed := false
	t.Cleanup(func() {
		if !sp1Killed {
			sp1.ForceKill()
		}
	})
	cli := helpers.LoginAsAdmin(t, sp1.APIURL())

	_, err := cli.CreateMetadataStore(metaName, "badger", helpers.WithMetaDBPath(badgerPath))
	require.NoError(t, err, "create badger metadata store")
	_, err = cli.CreateLocalBlockStore(localName, "fs",
		helpers.WithBlockRawConfig(fmt.Sprintf(`{"path":"%s"}`, blocksPath)))
	require.NoError(t, err, "create fs block store")
	_, err = cli.CreateShare(shareName, metaName, localName,
		helpers.WithShareDefaultPermission("read-write"))
	require.NoError(t, err, "create share")
	_, err = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "enable nfs adapter")
	require.NoError(t, helpers.WaitForAdapterStatus(t, cli, "nfs", true, 5*time.Second))
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	mount1 := framework.MountNFSWithVersion(t, nfsPort, "4.2")
	filePath := mount1.FilePath("persist.txt")
	framework.WriteFile(t, filePath, []byte("content"))
	setfattr(t, filePath, "user.persistent", "survives")
	got, err := getfattr(t, filePath, "user.persistent")
	require.NoError(t, err)
	require.Equal(t, "survives", got, "xattr readable before restart")

	mount1.Cleanup()
	sp1.ForceKill()
	sp1Killed = true

	// ---- Second lifetime: same config/sqlite/badger -> config + xattr reload ----
	sp2 := helpers.StartServerProcessWithConfig(t, configFile)
	t.Cleanup(sp2.ForceKill)
	cli2 := helpers.LoginAsAdmin(t, sp2.APIURL())
	require.NoError(t, helpers.WaitForAdapterStatus(t, cli2, "nfs", true, 10*time.Second),
		"nfs adapter should auto-start from persisted config")
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	mount2 := framework.MountNFSWithVersion(t, nfsPort, "4.2")
	t.Cleanup(mount2.Cleanup)
	filePath2 := mount2.FilePath("persist.txt")
	got2, err := getfattr(t, filePath2, "user.persistent")
	require.NoError(t, err, "xattr should be readable after restart")
	assert.Equal(t, "survives", got2, "xattr must persist across restart")
}

// writeXattrPersistConfig writes a config with an on-disk sqlite control-plane so
// share/store/adapter definitions survive a restart.
func writeXattrPersistConfig(t *testing.T, configFile string, apiPort int, stateDir string) {
	t.Helper()
	content := fmt.Sprintf(`logging:
  level: DEBUG
  format: text
  output: stdout

controlplane:
  port: %d
  jwt:
    secret: "test-secret-key-for-e2e-testing-only-must-be-32-chars"

database:
  type: sqlite
  sqlite:
    path: "%s/dittofs.db"
`, apiPort, stateDir)
	framework.WriteFile(t, configFile, []byte(strings.TrimSpace(content)+"\n"))
}
